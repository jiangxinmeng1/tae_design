- Intra-System Change Propagation Design
- Status: Draft
- Start Date: 2025-06-18
- Author: [XuPeng](https://github.com/XuPeng-SH)

# Design Goals

This document describes the design of the **Intra-System Change Propagation** feature in the **MatrixOne DBMS**. Traditionally, the index is updated synchronously, which means the index is updated after the data is committed (Strong consistency). However, in some cases, the index update is not critical, and the data can be committed without waiting for the index update to complete(Weak consistency). This feature is designed to support this scenario.

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

### mo_intra_system_change_propagation_log

A new table `mo_intra_system_change_propagation_log` is added to record the change propagation.
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
- There can be multiple jobs on a table, distinguished by job names.
- When a job is registered, a record will be inserted into `mo_intra_system_change_propagation_log`. Each job corresponds to a consumer and synchronizes data into it.
- When a job is unregistered, the `drop_at` will be updated and the record will be deleted asynchronously.

* When repeatedly dropping and creating jobs with the same name on the same table, if the rows of the previous job haven't been garbage collected it reports duplicate. We using REPLACE INTO instead of INSERT INTO avoids duplicate errors.

- `job_config`: The mode of job synchronization, e.g. triggering frequency, whether the job shares executor with other jobs, etc.

- `column_names`: To reduce memory usage, each job can specify column names. It collects necessary columns instead of all columns.

- After synchronize change, `last_sync_txn_ts`,`err_code`,`error_msg` will be updated.
- Each job can specify different consumers. Users can customize consumers.

### Iteration

In a `iteration`, ISCP synchronize data to consumers.

- Each iteration at a time includes one table and one or more jobs on that table. The source table is scanned once, and the data is synchronized to all the consumers.
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
- Each job corresponds to one consumer. Each consumer calls `Next()` to retrieve data, including insert batches and delete batches. Since multiple consumers share the same iteration, the returned insert batch may contain columns for other job as well, so consumers need to distinguish them using `batch.Attr`. Consumers involved in the same iteration can affect each other — if one consumer is slow in processing, the others may get blocked when calling `Next()`.
- When any error occurs, the `error_json` will be updated, which contains the errors from all consumers. Each consumer has an error code and an error message.
- err_code: 0 means success, 1-9999 means temporary error, which will be retried in the next iteration, 10000+ means permanent error, which need to be repaired manually.
- In each iteration, data consumption and watermark updates are placed in the same transaction, so they commit together. Upon restart, the latest watermark can be retrieved, avoiding duplicate data delivery. The initial synchronization involves a large volume of data, so it is not placed in a single transaction. If the process is not completed before a restart, all data will be deleted and the synchronization will restart from scratch.

- To make use of multiple CN nodes, iterations can be executed on any CN via `mo_ctl`:

  ```sql
  select mo_ctl('CN', 'ISCP', 'accountID:tableID:index_name1:index_name2...')
  ```

### ISCP Runner

A global ISCP Runner manages job metadata, triggers iterations, performs GC on `mo_intra_system_change_propagation_log`, and periodically updates watermarks.

1. The runner maintains the watermark and error status for all ISCP jobs. It subscribes to `mo_intra_system_change_propagation_log` to detect job insertions, deletions, and updates--ensuring only committed data is received.

2. The runner triggers iterations and sends them to executors.

- The runner first selects candidate iterations, then filters out tables that have no updates beyond their current watermark. For those tables, it directly updates the in-memory watermark. Iterations for updated tables are send to executors.

- The following are the rules for selecting candidate iterations and determining `from_ts`, `to_ts`:

- A newly registered job trigger a iteration(i.e. a sychronization) immediately without checking: 1.If there are no other indexes on the table or it syncronize independently, it synchronizes data from timestamp 0 to the current time.2.If there are already indexes on the table, it synchronizes data from timestamp 0 to the watermark of the other indexes, so that they can be synchronized together in the future. Since this iteration may take a long time, other indexes on the table will continue updating normally to avoid being blocked.

- For jobs that have already completed their first iteration, the runner scans them every 10 seconds to select candidates. Followings are the rules:

- Jobs on the same table try to maintain consistent watermarks so they can be synchronized together.It is also possible to configure a job to synchronize independently, so it will not be blocked by other jobs.

- The job configuration controls when iterations are triggered and whether iterations are shared:

- Default Job Config

   If all indexes on a table have the same timestamp and there is no running iteration (except for newly created indexes), synchronization occurs from the watermark to the current time.

   Some indexes may fall behind others on the same table: 1.This happens because during the initial full sync of a new index, other indexes on the table might continue to update, causing this index to lag behind. 2.It may also happen if multiple indexes are created in a row on a new table, and each gets a different initial watermark. In such cases, one lagging index is selected at a time to catch up to the table's maximum watermark. These lagging indexes should be few in number and can quickly be brought into alignment with the table's overall watermark.

- Always Update Job Config
    Always update the watermark for each job. Each job has its own iteration. Always Update Job Config is suitable for jobs with higher performance requirements. It consumes more resources.

- Timed Job Config
    TimedJobConfig is a job configuration that only updates when the time difference between now and watermark exceeds a specified duration.

* Job config is persisted in `mo_intra_system_change_propagation_log`. It can be modified and the changes take effect with a slight delay. For example, users can speed up a job by changing `Default Job Config` to `Always Update Job Config`. Job config can be customized by the customer.

* Iteration can be slow, so `from_ts` may be earlier than the earliest available records. When checking whether a table has changed, if the `from_ts` is too old (and the table's updates are no longer queryable due to TTL), skip the check and proceed with iteration directly.

3. flush watermark

- If the table has no new data, only the in-memory watermark is updated; the `mo_intra_system_change_propagation_log` will not be updated. After a restart, the system will query for updates after this timestamp. To avoid querying very old data, the backend updates all watermarks every hour. These updates will be split into DELETE and INSERT statements.

* To avoid background transactions that update the watermark from overwriting drop_at updates, the Delete statement includes a condition to ensure `drop_at IS NULL`. 

4. gc
 The task periodically handle the `GC` of the `mo_intra_system_change_propagation_log` table.
- It will clean up the `mo_intra_system_change_propagation_log` table for the tables that with `drop_at` is not empty and one day has passed.
- GC runs once every hour.

## Interface
```golang
// Multiple tables can share a single sinker.
// Changes to table IDs are not monitored — if a truncate occurs, the job needs to be rebuilt.
type ConsumerInfo struct{
  ConsumerType int8
  TableName string
  DBName string
  JobName string
}

// return true if create, return false if job already exists, return error when error
func RegisterJob(ctx context.Context,cnUUID string,txn client.TxnOperator, pitr_name string, sinkerinfo_json *ConsumerInfo, jobConfig JobConfig)(bool, error)

// return true if delete success, return false if no job found, return error when delete failed.
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
