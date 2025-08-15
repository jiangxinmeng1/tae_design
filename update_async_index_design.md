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

```
1.Register/Unregister Job  
                |     4.Update Job Spec        
                |             | 
                v             v
+--------------------------------------+
|            mo_iscp_log               |
|      (Stores all job changes)        |
+--------------------------------------+
                |
                | 2.runner subscribe mo_iscp_log
                v
+---------------------------------+
|             Runner              |
| (Listens to log, triggers jobs) |
+---------------------------------+
                |
                | 3. iteration
                v
+---------------------------------+
|           Scheduler             |
|    4. processes iterations      |
+---------------------------------+
```

Users can register/unregister jobs (1) and update job specs (4). All updates are first recorded in the `mo_iscp_log` table.
The Runner manages the in-memory job status. It subscribes to `mo_iscp_log` to receive job updates and decides when to trigger an iteration (i.e. a sync task).
The Scheduler performs iterations according to `job_spec`. The status changes of jobs in Runner and Scheduler are also recorded in `mo_iscp_log`. 

### mo_iscp_log

A new table `mo_iscp_log` is added to record the change propagation. Each row corresponds to a job.
```sql
CREATE TABLE mo_catalog.mo_iscp_log (
				account_id INT UNSIGNED NOT NULL,
				table_id BIGINT UNSIGNED NOT NULL,
				job_name VARCHAR NOT NULL,
				job_id BIGINT UNSIGNED NOT NULL,
				job_spec JSON NOT NULL,
				job_state TINYINT NOT NULL,
				watermark VARCHAR NOT NULL,
				job_status JSON NOT NULL,
				create_at TIMESTAMP NOT NULL,
				drop_at TIMESTAMP NULL, 
				primary key(account_id, table_id, job_name)
			);

```
- Within the same tenant, jobs are identified by `JobID`, which consists of DBName, TableName and JobName. 
  During registration/unregistration, JobID is converted to table_id and job_name and stored in the table.
  ISCP does not handle table ID changes. If the table ID changes (e.g., due to TRUNCATE), the job must be re-registered.
  ```golang
      type JobID struct{
        DBName string,
        TableName string,
        JobName string,
      }
  ```
- The `job_id` is used to distinguish jobs that have the same job name and repeatedly register/unregister on the same table. In the mo_iscp_log table, jobs on the same table with the same name that have not been garbage-collected have different IDs.

- `JobSpec` includes TriggerSpec and IterationDetail.
  `TriggerSpec` defines the rules for the Runner to trigger an iteration.
  `IterationDetail` defines how the Scheduler schedules and executes iterations.
  ```golang
      type JobSpec struct{
        TriggerSpec
        IterationDetail
      }
  ```

- `JobStatus` records status information about the job and its latest iteration.

- `job_state` include: 
  `pending`: Scheduler has received the iteration and is waiting for an idle worker. 
  `running`: The iteration is currently being executed by a worker.
  `canceled`: The iteration was canceled (e.g. due to a job spec update configured to interrupt ongoing iterations).
  `error`: The iteration failed with a permanent error.
  `completed`: The iteration finished successfully. The Runner will schedule the next iteration.
  When a job is first registered, its initial state is set to  `completed` to allow the Runner to begin iteration.

- `watermark`: the progress marker for job updates, i.e. the `toTS` of its last successful iteration.

#### Register Job, Unregeister Job
- All operations are first written to the `mo_iscp_log` and then propagated to the ISCP Runner through subscriptions.
- When a job is registered, a record will be inserted into `mo_iscp_log`. 
  When registering a job, the current time is recorded as its  `create_time`.
- When a job is unregistered, the `drop_at` will be updated and the record will be deleted asynchronously. When repeatedly dropping and creating jobs with the same name on the same table, if the rows of the previous job haven't been garbage collected it reports duplicate. We using REPLACE INTO instead of INSERT INTO avoids duplicate errors.
  ```golang
    // ctx contains the account ID.
    // return true if create, return false if job already exists, return error when error
    func RegisterJob(ctx context.Context, txn client.TxnOperator, pitr_name string, jobID *JobID, jobSpec *JobSpec)(bool, error)
    // return true if delete success, return false if no job found, return error when delete failed.
    func UnregisterJob(ctx context.Context, txn client.TxnOperator, jobID *JobID) (bool, error)
  ```

### ISCP Runner

A global ISCP Runner manages job metadata, triggers iterations, performs GC on `mo_iscp_log`, and periodically updates watermarks.
The runner maintains job_spec and job_status for all ISCP jobs. It subscribes to `mo_iscp_log` to detect job insertions, deletions, and updates--ensuring only committed data is received.

Runner triggers an iteration through the following steps:

- The runner first selects candidate iterations
- filters out tables that have no updates beyond their current watermark.

- For those tables, it directly updates the in-memory watermark. 

- Iterations for updated tables are send to schedulers.

```
1. Selects candidates  
           |
           v
2. Filter tables with no updates --[No updates]--> 3.1 Update In-Memory Watermark
           |
      [ Updated ]
           |
           v
3.2 Send to Sheduler 
```

#### Select candidate iterations

- A newly registered job trigger a iteration(i.e. a sychronization) immediately without checking: 1.If there are no other jobs on the table or it syncronize independently, it synchronizes data from timestamp 0 to the current time.2.If there are already jobs on the table, it synchronizes data from timestamp 0 to the watermark of the other jobs, so that they can be synchronized together in the future. Since this iteration may take a long time, other jobs on the table will continue updating normally to avoid being blocked.

- For jobs that have already completed their first iteration, the runner scans them every 10 seconds to select candidates. Followings are the rules:

  - The `TriggerSpec` controls when iterations are triggered and whether iterations are shared:

    - Default Job Config
        ```json
        {
          "job_type": "default",
          "schedule": {}
        }
        ```
        Jobs on the same table try to maintain consistent watermarks so they can be synchronized together.It is also possible to configure a job to synchronize independently, so it will not be blocked by other jobs. If all jobs on a table have the same timestamp and there is no running iteration (except for newly created jobs), synchronization occurs from the watermark to the current time.

        Some jobs may fall behind others on the same table: 1.This happens because during the initial full sync of a new job, other jobs on the table might continue to update, causing this job to lag behind. 2.It may also happen if multiple jobs are created in a row on a new table, and each gets a different initial watermark. In such cases, one lagging job is selected at a time to catch up to the table's maximum watermark. These lagging jobs should be few in number and can quickly be brought into alignment with the table's overall watermark.

    - Greedy Job Config
        ```json
        {
          "job_type": "greedy",
          "schedule": {}
        }

        ```
        Always trigger iteration. Each job has its own iteration. Greedy Job Config is suitable for jobs with higher performance requirements. It consumes more resources.

    - Periodic Job Config
        ```json
        {
          "job_type": "periodic",
          "schedule": {
            "interval": "10s",
            "share": "true",
          }
        }

        ```
        Periodic Job Config is a job configuration that only updates when the time difference between now and watermark exceeds a specified duration.
        It can be configured to share iterations

    - TriggerSpec can be customized by the customer.

- User can modify TriggerSpec. For example, users can speed up a job by changing `Default Job Config` to `Greedy Job Config`. 
  TriggerSpec is modified together with IterationDetail.
  The incoming JobSpec will overwrite the current one. Any empty fields in the new JobSpec will be filled with values from the current JobSpec.
  For TriggerSpec, it will take effect after current iteration completes.
  For IterationDetail, they can optionally interrupt currently queued or running iterations so the new spec takes effect immediately.
  ```golang
    func UpdateJobSpec(ctx context.Context, txn client.TxnOperator, jobID *JobID, jobSpec *JobSpec, opts *JobUpdateOptions)(error)
  ```

#### Filter Tables without Updates

* Iteration can be slow, so `from_ts` may be earlier than the earliest available records. When checking whether a table has changed, if the `from_ts` is too old (and the table's updates are no longer queryable due to TTL), skip the check and proceed with iteration directly.

#### Flush Watermark

- If the table has no new data, only the in-memory watermark is updated; the `mo_iscp_log` will not be updated. After a restart, the system will query for updates after this timestamp. To avoid querying very old data, the backend updates all watermarks every hour. These updates will be split into DELETE and INSERT statements.

* To avoid background transactions that update the watermark from overwriting drop_at updates, the Delete statement includes a condition to ensure `drop_at IS NULL`. 

#### GC
 The task periodically handle the `GC` of the `mo_iscp_log` table.
- It will clean up the `mo_iscp_log` table for the tables that with `drop_at` is not empty and one day has passed.
- GC runs once every hour.

### Scheduler

- The Scheduler, upon receiving an iteration, places it in a queue waiting for an available worker.
  It uses IterationDetail: `priority` determines which iteration runs first.
  `ConsumerInfo` specifies how to consume synchronized data.
  ```golang
  type IterationDetail struct{
    Priority int
    JobID JobID
    ConsumerInfo ConsumerInfo
  }
  ```

- The Scheduler updates the JobStatus in the `mo_iscp_log` when an iteration starts and when it completes.

```golang
type JobStatus struct {
  FromTS types.TS
  ToTS types.TS
  StartAt types.TS
  EndAt types.TS
  ErrorCode int
  ErrorMsg string
}
```
- `JobStatus` records status of job's last iteration.
  `FromTS` and `ToTS` mark the time range of the data being synchronized.
  `StartAt` and `EndAt` mark when the iteration started and finished.
  `ErrorCode` and `ErrorMsg` describe the result of the last iteration.

####
Users can query job status by JobID, or filter jobs by state.
```golang
func GetJobStatus(ctx context.Context, jobID *JobID)(jobStatus *JobStatus,err error)
func GetJobCount(state int8) ([]JobID,[]JobStatus)
```

####
Scheduler iterations can be canceled. e.g. on shutdown all iterations are canceled.
When users update a job spec, they can optionally interrupt currently queued or running iterations so the new spec takes effect immediately.

#### Iteration
In a `iteration`, ISCP synchronize data to consumers.
- Each job can specify different consumers. Users can customize consumers.
```golang
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

// Multiple tables can share a single sinker.
// Changes to table IDs are not monitored — if a truncate occurs, the job needs to be rebuilt.
type ConsumerInfo struct{
  ConsumerType int8
  TableName string
  DBName string
  JobName string
}

type Consumer interface{
  Consume(context.Context, DataRetriever) error
}

func NewConsumer(
  cnUUID string,
  tableDef *plan.TableDef,
  consumerInfo *ConsumerInfo,
)(Consumer,error)
```
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
- When any error occurs, it will be stored in `error_code` and `error_msg`, which contains the errors from all consumers. Each consumer has an error code and an error message.
- err_code: 0 means success, 1-9999 means temporary error, which will be retried in the next iteration, 10000+ means permanent error, which need to be repaired manually.
- In each iteration, data consumption and watermark updates are placed in the same transaction, so they commit together. Upon restart, the latest watermark can be retrieved, avoiding duplicate data delivery. The initial synchronization involves a large volume of data, so it is not placed in a single transaction. If the process is not completed before a restart, all data will be deleted and the synchronization will restart from scratch.

- After synchronize change,`state`, `watermark`,`err_code`,`error_msg` will be updated.

