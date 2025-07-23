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

1. Intra system change propagation is a task per tenant. The following design is based on this assumption.

2. A new table `mo_intra_system_change_propagation_log` is added to record the change propagation.
```sql
CREATE TABLE mo_catalog.mo_intra_system_change_propagation_log (
				account_id INT UNSIGNED NOT NULL,
				table_id BIGINT UNSIGNED NOT NULL,
				job_name VARCHAR NOT NULL,
				job_config VARCHAR NOT NULL,
				column_names VARCHAR NOT NULL,
				last_sync_txn_ts VARCHAR(32)  NOT NULL,
				err_code INT NOT NULL,
				error_msg VARCHAR(255) NOT NULL,
				info VARCHAR NOT NULL,
				drop_at DATETIME NULL,
				consumer_config VARCHAR NULL,
				primary key(account_id, table_id, job_name)
			);

```
- When a job is registered, a record will be inserted into `mo_intra_system_change_propagation_log`. Each job corresponds to a consumer and synchronizes data into it.
- When synchronize change, the `last_sync_txn_ts` will be updated.
- When a job is unregistered, the `drop_at` will be updated and the record will be deleted asynchronously.
- To ensure that the transaction to register or unregister a job has been committed before registering the index in memory, it subscribes to the logtail of `mo_intra_system_change_propagation_log`.It waits for the logtail to deliver the insert or delete information.
- There can be multiple jobs on a table, distinguished by their job names.

- A newly registered job trigger a iteration(i.e. a sychronized task) immediately: 1.If there are no other indexes on the table or it syncronize independently, it synchronizes data from timestamp 0 to the current time.2.If there are already indexes on the table, it synchronizes data from timestamp 0 to the watermark of the other indexes, so that they can be synchronized together in the future. Since this task may take a long time, other indexes on the table will continue updating normally to avoid being blocked.

3. Every `10 seconds`, executor scan jobs and tables in memory and trigger iterations.

- It filters out the tables that meet the criteria and checks for updates. If there are updates, data synchronization is triggered. The iteration task is passed to the executor.
- Jobs on the same table try to maintain consistent watermarks so they can be synchronized together.It is also possible to configure a job to synchronize independently, so it will not be blocked by other jobs.

- It skip jobs with watermark of 0, which are newly created jobs and iterations have already been triggered.

- The job configuration controls when iterations are triggered and whether iterations are shared:

- Default Job Config

   If all indexes on a table have the same timestamp and there is no running iteration (except for newly created indexes), synchronization occurs from the watermark to the current time.

   Some indexes may fall behind others on the same table: 1.This happens because during the initial full sync of a new index, other indexes on the table might continue to update, causing this index to lag behind. 2.It may also happen if multiple indexes are created in a row on a new table, and each gets a different initial watermark. In such cases, one lagging index is selected at a time to catch up to the table's maximum watermark. These lagging indexes should be few in number and can quickly be brought into alignment with the table's overall watermark.

- Always Update Job Config
    Always update the watermark for each job. Each job has its own iteration.

- Timed Job Config
    TimedJobConfig is a job configuration that only updates when the time difference between now and watermark exceeds a specified duration.

4. Job config is persisted in `mo_intra_system_change_propagation_log`. It can be modified and the changes take effect with a slight delay. For example, users can speed up a job by changing `Default Job Config` to `Always Update Job Config`. Job config can be customized by the customer.

5. After collecting the table list, it starts to synchronize data to consumers, which will be called a `iteration`. 
- If there're too many iterations, they will be executed in multiple executors.
- Each executor processes one iteration at a time, which includes one table and one or more jobs on that table. The source table is scanned once, and the data is synchronized to all the consumers.
```
              +--------------------+
              |   Source Table     |
              |  data [from, to]   |
              +--------+-----------+
                       |
               [Read a data batch]
                       |
        +--------------+--------------+--------------+
        |                             |              |
   +----v----+                  +-----v----+    +-----v----+
   |Consumer1|                  |Consumer2 |    |Consumer3 |
   | .Next() |                  | .Next()  |    | .Next()  |
   +---------+                  +----------+    +----------+
        |                             |              |
  Receives batch               Receives batch   Receives batch

```
- Each index corresponds to one consumer. Each consumer calls `Next()` to retrieve data, including insert batches and delete batches. Since multiple consumers share the same iteration, the returned insert batch may contain columns for other job as well, so consumers need to distinguish them using `batch.Attr`. Consumers involved in the same iteration can affect each other — if one consumer is slow in processing, the others may get blocked when calling `Next()`.
- When any error occurs, the `error_json` will be updated, which contains the errors from all consumers. Each consumer has an error code and an error message.
- err_code: 0 means success, 1-9999 means temporary error, which will be retried in the next iteration, 10000+ means permanent error, which need to be repaired manually.
- In each iteration, data consumption and watermark updates are placed in the same transaction, so they commit together. Upon restart, the latest watermark can be retrieved, avoiding duplicate data delivery. The initial synchronization involves a large volume of data, so it is not placed in a single transaction. If the process is not completed before a restart, all data will be deleted and the synchronization will restart from scratch.


6. The task periodically handle the `GC` of the `mo_intra_system_change_propagation_log` table.
- It will clean up the `mo_intra_system_change_propagation_log` table for the tables that with `drop_at` is not empty and one day has passed.
- It could be very low frequency.


## Interface
```golang
// Multiple tables can share a single sinker.
// Changes to table IDs are not monitored — if a truncate occurs, the task needs to be rebuilt.
type ConsumerInfo struct{
  ConsumerType int8
  TableName string
  DBName string
  JobName string
}

// return true if create, return false if task already exists, return error when error
func RegisterJob(ctx context.Context,cnUUID string,txn client.TxnOperator, pitr_name string, sinkerinfo_json *ConsumerInfo, jobConfig JobConfig)(bool, error)

// return true if delete success, return false if no task found, return error when delete failed.
func UnregisterJob(ctx context.Context,cnUUID string,txn client.TxnOperator,sinkinfo *ConsumerInfo) (bool, error)

type JobConfig interface {
        Marshal() ([]byte, error)
        Unmarshal([]byte) error
        GetType() uint16
        Check(
                otherConsumers []*JobEntry,
                consumer *JobEntry,
                now types.TS,
        ) (
                ok bool, from, to types.TS, shareIteration bool,
        )
}

func NewConsumer(
  cnUUID string,
  tableDef *plan.TableDef,
  consumerInfo *ConsumerInfo,
)(Consumer,error)

// insertBatch: all columns+ts
// deleteBatch: pk+ts
// DataRetriever should have a member (txn client.TxnOperator)
interface DataRetriever {
  //SNAPSHOT = 0, TAIL = 1
  //TAIL can use INSERT, SNAPSHOT need REPLACE INTO
  //in SNAPSHOT, deleteBatch is nil
    Next() (insertBatch *AtomicBatch, deleteBatch *AtomicBatch, noMoreData bool, err error)
    UpdateWatermark(executor.TxnExecutor,executor.StatementOption)error
    GetDataType() int8
}
type Consumer interface{
  Consume(context.Context, DataRetriever) error
}
```














