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
    iteration_state INT NOT NULL, -- running/finished
    info VARCHAR(255) NOT NULL,
    drop_at VARCHAR(32) NULL,
    sinker_config VARCHAR(32) NULL,
);
```
- When a table is created with async indexes, a record will be inserted into `mo_async_index_log` for each async index.
- When a index is updated, the `last_sync_txn_ts` will be updated.
- When the index is dropped, the `drop_at` will be updated and the record will be deleted asynchronously.

//TODO index name may be ""
3. 每10s会扫描一次mo_async_index_log。
挑出符号要求的表检查是否有更新。如果没有更新直接更新watermark。如果有更新就同步数据。同步之前先更新mo_async_index_iterations表。执行器按mo_async_index_iterations执行同步任务。同一张表的index会尽量保持watermark一致，这样可以一起同步。
会按这些要规则创建iteration：
* watermark为0的index是新建的,如果这张表没有其他Index，同步从0到现在的数据。如果表上已经有index了，同步从0到其他Index的watermark，这样下次它们可以一起同步。因为这个任务可能非常久，为了不影响表上其他索引，其他索引会正常更新。
* 如果一张表的时间戳一致，而且没有正在跑的iteration（除了新建的索引以外），同步从watermark到now的数据。
* 可能某几个index的watermark落后表上的其他索引。因为新建索引第一次同步的过程中可能表上其他的索引又更新了，这个索引会落后于整张表。如果新的表上连续建多个索引，它们拿到的第一个watermark不一样，这样也会产生落后的索引。每次挑选一个索引同步到表上最大的watermark。这样的索引应该不多，很快能让整张表的水位一致。

每个同步任务对应mo_async_index_iterations中的一行。选中表和index后往mo_async_index_iterations插入，填写account_id,table_id,index_names,from_ts,to_ts。

- When any error occurs, the `err_code` and `error_msg` will be updated.
  - err_code: 0 means success, 1-9999 means temporary error, which will be retried in the next iteration, 10000+ means permanent error, which need to be repaired manually.

4. After collecting the table list, it starts to update index tables according to the table list, which will be called a `iteration`. In a iteration:
```sql
CREATE TABLE mo_async_index_iterations (
    id INT AUTO_INCREMENT PRIMARY KEY,
    account_id INT NOT NULL,
    table_id INT,
    index_names VARCHAR(255),--multiple indexes
    from_ts VARCHAR(32) NOT NULL,
    to_ts VARCHAR(32) NOT NULL,
    error_json VARCHAR(255) NOT NULL,--存多个error，多个sinker的error可能不一样
    start_at VARCHAR(32) NOT NULL,
    end_at VARCHAR(32) NOT NULL,
);
```
- If the list of tables is too large, it will be executed in multiple executors.
- 每10s扫描一次
- 每个executor一次处理一个iteration，包括一张表和上面的一个或多个index。
- It's better to avoid transferring the diff data to SQL.
- At the end of the iteration, a record will be inserted into `mo_async_index_iterations` for the iteration.

5. The task periodically handle the `GC` of the `mo_async_index_log` table and `mo_async_index_iterations` table.
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
  IndexName string//可能是""
}

// return true if create, return false if task already exists, return error when error
func CreateTask(c *Compile, pitr_id int, sinkerinfo_json SinkerInfo)(bool, error)

// return true if delete success, return false if no task found, return error when delete failed.
func Deletetask(c * Compile, sinkinfo SinkerInfo) (bool, error)

func NewSinker(
  	cnUUID     string,
  	dbTblInfo  *DbTableInfo,
    tableDef   *plan.TableDef,
    sinkerInfo SinkerInfo,
) Sinker {}
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















