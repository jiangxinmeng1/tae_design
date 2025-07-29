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

用户可以register/unregister job（1），更新job spec（4），这些更新都先记录在mo_iscp_log中。
Runner管理内存中的job状态。Runner订阅mo_iscp_log来获取job的更新。它决定什么时候触发iteration（i.e.同步任务）。
Scheduler根据优先级做iteration。Scheduler和Runner中iteration的状态更新也记录在mo_iscp_log中。

### mo_iscp_log

A new table `mo_iscp_log` is added to record the change propagation.每行对应一个job
```sql
CREATE TABLE mo_catalog.mo_iscp_log (
				account_id INT UNSIGNED NOT NULL,
				table_id BIGINT UNSIGNED NOT NULL,
				job_name VARCHAR NOT NULL,
				job_spec JSON NOT NULL,
        job_status JSON NOT NULL,
        create_at TIMESTAMP NOT NULL,
				drop_at TIMESTAMP NULL,
				primary key(account_id, table_id, job_name)
			);

```
- 同一个租户下用JobID来区分Job，包含DBName,TableName,JobName。注册/注销时JobID转化成tableid和jobname存到表里。
  ISCP不处理表ID变化，如果表ID改变（例如truncate），要重新注册Job。
```golang
    type JobID struct{
      DBName string,
      TableName string,
      JobName string,
    }
```
- JobSpec
job spec中包含TriggerSpec和TriggerSpec。TriggerSpec决定runner触发iteration的规则, ExecutionSpec决定Scheduler调度和执行时的行为。
```golang
    type JobSpec struct{
      TriggerSpec
      ExecutionSpec
    }
```

- JobStatus
  JobStatus记录job的最新iteration的信息。


#### 注册/注销job，更新JobSpec
- 这些操作先写入iscp表，之后通过订阅传给ISCP Runner.
- When a job is registered, a record will be inserted into `mo_iscp_log`.
- 注册时会记录当前时间作为create time.
- When a job is unregistered, the `drop_at` will be updated and the record will be deleted asynchronously. When repeatedly dropping and creating jobs with the same name on the same table, if the rows of the previous job haven't been garbage collected it reports duplicate. We using REPLACE INTO instead of INSERT INTO avoids duplicate errors.
  ```golang
    // ctx里有account id
    // return true if create, return false if job already exists, return error when error
    func RegisterJob(ctx context.Context, txn client.TxnOperator, pitr_name string, jobID *JobID, jobSpec *JobSpec)(bool, error)
    // return true if delete success, return false if no job found, return error when delete failed.
    func UnregisterJob(ctx context.Context, txn client.TxnOperator, jobID *JobID) (bool, error)
  ```

### ISCP Runner

A global ISCP Runner manages job metadata, triggers iterations, performs GC on `mo_iscp_log`, and periodically updates watermarks.
The runner maintains job_spec and job_status for all ISCP jobs. It subscribes to `mo_iscp_log` to detect job insertions, deletions, and updates--ensuring only committed data is received.

这些时runner触发iteraton的步骤：

1. The runner first selects candidate iterations
2. filters out tables that have no updates beyond their current watermark.
3.1 For those tables, it directly updates the in-memory watermark. 
3.2 Iterations for updated tables are send to executors.

```
+---------------------+
|      Runner         |
| 1. Selects          |
|   candidates        |
+---------------------+
           |
           v
+---------------------+
| 2. Watermark Filter |
|   - Skips tables    |
|     with no updates |
|     (updates memory)|
+---------------------+
           |
           |--[No updates]--> 3.1 Update In-Memory Watermark
           |
           v
+---------------------+
| 3.2 Send to         |
|    Executors        |
|   (Only updated     |
|    tables)          |
+---------------------+
```

#### 选出候选的iteration

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
        可以配置是否共享iteration.

  - TiggerSpec can be modified. For example, users can speed up a job by changing `Default Job Config` to `Greedy Job Config`. Job config can be customized by the customer.
  ```golang
    func UpdateJobSpec(ctx context.Context, txn client.TxnOperator, jobID *JobID, jobSpec *JobSpec, opts *JobUpdateOptions)(error)
  ```
- 用户可以修改JobSpec。传入的JobSpec会覆盖原有的JobSpec，其中的空值会用原有的JobSpec覆盖。
  可以通过JobUpdateOptions控制更新时的行为，例如是否打断scheduler中Pending的iteration。
  如果已经交给scheduler，这个配置要等iteration做完才生效。

####
检查table是否有change

* Iteration can be slow, so `from_ts` may be earlier than the earliest available records. When checking whether a table has changed, if the `from_ts` is too old (and the table's updates are no longer queryable due to TTL), skip the check and proceed with iteration directly.

####
3. flush watermark

- If the table has no new data, only the in-memory watermark is updated; the `mo_iscp_log` will not be updated. After a restart, the system will query for updates after this timestamp. To avoid querying very old data, the backend updates all watermarks every hour. These updates will be split into DELETE and INSERT statements.

* To avoid background transactions that update the watermark from overwriting drop_at updates, the Delete statement includes a condition to ensure `drop_at IS NULL`. 

####
4. gc
 The task periodically handle the `GC` of the `mo_iscp_log` table.
- It will clean up the `mo_iscp_log` table for the tables that with `drop_at` is not empty and one day has passed.
- GC runs once every hour.

### scheduler


- 查询worker里等待和正在做的iteration，（job_name,state,from,to,startat,endat）

- 更改排队的iteration的优先级，取消正在排队的iteration。

```golang
type ExecutionSpec struct{
  Priority int
  ConsumerInfo{
    ConsumerType int8
    TableName string
    DBName string
  }
}
```

- scheduler根据Priority调度worker。

```golang
type IterationStatus struct {
  State int8 // running, completed, error, canceled, pending

  ErrorCode int
  ErrorMsg string

//如果running，就是现在在做的from,to。不然就是上次的from,to
// data的from,to
  FromTS types.TS
  ToTS types.TS
  //iteration的开始结束
  StartAt types.TS
  EndAt types.TS
  CNUUID string//执行的cn

}
```
- state: 
pending: scheduler收到了，正在排队等worker闲下来处理这个iteration
running: worker正在执行的iteration
canceled: 被取消的iteration。例如：更新jobSpec的时候，可以配置打断正在做的iteration。
error: 发生永久错误的iteration。
completed: 已完成iteration。runner会安排job做下一个iteration。刚注册的job初始化的时候会把state设置成complete，方便接下来做iteration。




### Iteration

1. 更新job status

2. 同步数据

3. 更新jobstatus

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

- After synchronize change, `last_sync_txn_ts`,`err_code`,`error_msg` will be updated.
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
