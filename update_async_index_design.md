- Update Async Index Design
- Status: Draft
- Start Date: 2025-06-18
- Author: [XuPeng](https://github.com/XuPeng-SH)

# Design Goals

This document describes the design of the **Update Async Index** feature in the **MatrixOne DBMS**. Traditionally, the index is updated synchronously, which means the index is updated after the data is committed (Strong consistency). However, in some cases, the index update is not critical, and the data can be committed without waiting for the index update to complete(Weak consistency). This feature is designed to support this scenario.

# Design Overview

## Background

In analytical scenarios, the consistency is not critical, and the index could be updated asynchronously:
1. Some indexes take too long to build synchronously
2. Some indexes achieve better performance when built with larger datasets.

Full-text and vector indexes are good examples.

## Challenges

1. The total number of indexes may be too large, which may affect the performance of the system.
2. Most of the indexes are not modified and how to find the indexes that need to be updated with low cost.
3. Tenant isolation and charging.

## Design

1. Update async index is a task per tenant. The following design is based on this assumption.

2. A new table `mo_async_index_log` is added to record the async index update information.
```sql
CREATE TABLE mo_async_index_log (
    id INT AUTO_INCREMENT PRIMARY KEY,
    account_id INT NOT NULL,
    table_id INT NOT NULL,
    tableName VARCHAR NOT NULL,
    dbName VARCHAR NOT NULL,
    index_name VARCHAR NOT NULL,
    last_sync_txn_ts VARCHAR(32)  NOT NULL,
    err_code INT NOT NULL,
    error_msg VARCHAR(255) NOT NULL,
    info VARCHAR(255) NOT NULL,
    drop_at VARCHAR(32) NULL,
    sinker_config VARCHAR(32) NULL,
);
```
- When a table is created with async indexes, a record will be inserted into `mo_async_index_log` for each async index.
- When a index is updated, the `last_sync_txn_ts` will be updated.
- When the index is dropped, the `drop_at` will be updated and the record will be deleted asynchronously.
- To ensure that the transaction for creating or dropping an index has been committed before registering the index in memory, it subscribes to the logtail of `mo_async_index_log`.It waits for the logtail to deliver the insert or delete information.

3. Every 10 seconds, scan indexes and tables in memory.
- It filters out the tables that meet the criteria and checks for updates. If there are updates, data synchronization is triggered. The iteration task is passed to the executor.
- Indexes on the same table try to maintain consistent watermarks so they can be synchronized together.

- Iterations will be created under the following conditions:

   If an index has a watermark of 0, it is a newly created index: 1.If there are no other indexes on the table, it synchronizes data from timestamp 0 to the current time.2.If there are already indexes on the table, it synchronizes data from timestamp 0 to the watermark of the other indexes, so that they can be synchronized together in the future. Since this task may take a long time, other indexes on the table will continue updating normally to avoid being blocked.

   If all indexes on a table have the same timestamp and there is no running iteration (except for newly created indexes), synchronization occurs from the watermark to the current time.

   Some indexes may fall behind others on the same table: 1.This happens because during the initial full sync of a new index, other indexes on the table might continue to update, causing this index to lag behind. 2.It may also happen if multiple indexes are created in a row on a new table, and each gets a different initial watermark. In such cases, one lagging index is selected at a time to catch up to the table's maximum watermark. These lagging indexes should be few in number and can quickly be brought into alignment with the table's overall watermark.

4. After collecting the table list, it starts to update index tables according to the table list, which will be called a `iteration`.Each synchronization task corresponds to a row in `mo_async_index_iterations`. In a iteration:
```sql
CREATE TABLE mo_async_index_iterations (
    id INT AUTO_INCREMENT PRIMARY KEY,
    account_id INT NOT NULL,
    table_id INT,
    index_names VARCHAR(255),--multiple indexes
    from_ts VARCHAR(32) NOT NULL,
    to_ts VARCHAR(32) NOT NULL,
    error_json VARCHAR(255) NOT NULL,--Multiple errors are stored. Different sinkers may have different errors.
    start_at VARCHAR(32) NULL,
    end_at VARCHAR(32) NULL,
);
```
- If there're too many iterations, they will be executed in multiple executors.
- Each executor processes one iteration at a time, which includes one table and one or more indexes on that table. The source table is scanned once, and the data is synchronized to all the indexes.
- Each index corresponds to one sinker. The synchronized data is sent to the sinker in `DecoderOutput` format
```golang
type DecoderOutput struct {
  tableName string
	outputTyp      OutputType//snapshot,tail
	noMoreData     bool
	fromTs, toTs   types.TS
  insertBatch *batch.Batch//replaceinto(all columns from table+ts)
  deleteBatch *batch.Batch//delete(pk+ts)
}
```
- When any error occurs, the `error_json` will be updated, which contains the errors from all sinkers. Each sinker has an error code and an error message.
- err_code: 0 means success, 1-9999 means temporary error, which will be retried in the next iteration, 10000+ means permanent error, which need to be repaired manually.

- At the end of the iteration, insert a row into `mo_async_index_iterations` and update `error_code`, `error_message`, and `watermark` in `mo_async_index_log`.

6. The task periodically handle the `GC` of the `mo_async_index_log` table and `mo_async_index_iterations` table.
- It will clean up the `mo_async_index_log` table for the tables that with `drop_at` is not empty and one day has passed.
- It will clean up the `mo_async_index_iterations` table for the iterations with smaller `end_at` of a account.
- It could be very low frequency.

## Interface
```golang
// Multiple tables can share a single sinker.
// Changes to table IDs are not monitored — if a truncate occurs, the task needs to be rebuilt.
type SinkerInfo struct{
  SinkerType int8
  TableName string
  DBName string
  IndexName string
}

// return true if create, return false if task already exists, return error when error
func CreateTask(ctx context.Context,txn client.TxnOperator, pitr_id int, sinkerinfo_json *SinkerInfo)(bool, error)

// return true if delete success, return false if no task found, return error when delete failed.
func DeleteTask(ctx context.Context,txn client.TxnOperator,sinkinfo *SinkerInfo) (bool, error)

func NewSinker(
  cnUUID     string,
  tableDef   *plan.TableDef,
  sinkerInfo *SinkerInfo,
) (Sinker,error)

type Sinker interface{
  Sink(ctx context.Context,data *DecoderOutput)
  SendBegin()
  SendCommit()
  SendRollback()
  sendDummy()
  Error() error
  ClearError()
  Reset()
  Close()
  RUN(ctx context.Context, ar *ActiveRoutine)
}
type DecoderOutput struct {
  tableName string
	outputTyp      OutputType//snapshot,tail
	noMoreData     bool
	fromTs, toTs   types.TS
  insertBatch *batch.Batch//replaceinto(all columns from table+ts)
  deleteBatch *batch.Batch//delete(pk+ts)
}
```














