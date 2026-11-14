# Papa 错误队列处理手册

> 面向：任务重试耗尽进入 `failed` 后，怎么把它重新投递回阶段重跑，以及如何防止永久坏任务无限空转。
> 配套：[RECOVER_QUEUE.md](./RECOVER_QUEUE.md)（持久化投递恢复，两者按状态分工）。

---

## 0. 一句话

任务最终失败（`status = failed`）后不会丢，持久化在 `crawler_tasks` 表。`error_queue` 负责把这些失败任务**重置为 pending 并重新入队**，支持自动轮询 + 手动触发，并用 `reprocess` 计数防止无限重试。

## 1. 配置

写在 `configs/config.yaml` 的全局段（**不再按站点拆**）：

```yaml
error_queue:
  worker_count: 2             # 并发重新投递失败任务的数量，0 = 默认 1
  interval: "10m"             # 自动轮询间隔；"0" = 默认 4h
  max_retry: 3                # 单个失败任务最多再处理代数；0 = 不限
  batch_size: 100             # 每批查询处理的任务数，0 = 默认 1000（分页流式）
```

> 队列始终运行，不再支持 `enabled`。省略配置或间隔为 0 使用默认 4h；worker_count 为 0 默认 1，batch_size 为 0 默认 1000。负数配置拒绝启动。
> 错误队列是**全局一条**：扫所有站点的 failed 任务重投回各自的阶段，后台一行。
> 队列参数是**启动时读一次**：改完要重启（后台没有热更这回事了）。

## 2. 触发方式

| 方式 | 说明 |
| --- | --- |
| 自动轮询 | 始终按有效间隔自动投递 |
| 手动（OA） | 队列治理面板上那一行的「立即执行」（`POST /api/errorqueue/process`）。每次触发都写一条操作日志（`server.operation_log` 开启时） |
| 手动（代码） | `engine.ProcessErrorQueue()` |

## 3. 再处理代数上限（防无限重试）

每次重新投递会把这个任务的 `reprocess + 1`（同时 `retry` 清零）。
重置成功后通知目标阶段的持久化投递协程；暂停或队满时保持 pending，不标业务失败、不再次消耗 reprocess。恢复投递按阶段独立进行，慢阶段不会阻塞其它阶段的重置。
`max_retry > 0` 时，`reprocess >= max_retry` 的任务不再投递，避免「结构错误 / 404」这类永久坏任务无限空转。

## 4. 与错误分类配合

配合 fetcher 里的错误分类（见 [FETCHER_WRITING_GUIDE.md](./FETCHER_WRITING_GUIDE.md)）可更精准：

- `return papa.WrapNoRetryKind("structure", err)` / `"not-found"` / `"protected"` 会让当前阶段跳过进程内尝试重试，并把错误种类写入失败信息；
- 普通可重试错误（网络抖动、超时）则先按阶段的 `Retry` 策略尝试。

需要注意：当前 `error_queue` 的查询条件只看 `status = failed` 和 `reprocess` 上限，**不会自动排除 `NoRetry` 错误种类**。若希望结构错误、404 等永久失败不再被自动重投，应设置有限的 `max_retry`；没有关闭全局自动轮询的开关。公开的 `engine.ProcessErrorQueue()` 使用框架固定查询条件，不能通过参数附加按阶段或错误种类过滤；自定义筛选需要业务自行查询和重投。

## 5. 与 recover_queue 的分工

| 机制 | 目标状态 | 语义 |
| --- | --- | --- |
| `error_queue` | `failed` | 重跑「明确失败」的任务 |
| `recover_queue` | 本进程启动前遗留的 `pending` / `processing` | 启动时恢复中断任务（以 `updated_at < startedAt` 为边界，**只在启动跑一次**，见 [RECOVER_QUEUE.md](./RECOVER_QUEUE.md)） |

两者按状态互斥，不会同时盯上同一个任务。

## 与周期队列竞争时的代次校验

重投 UPDATE 同时匹配查询时的 repeat、reprocess，并再次检查 status=failed 和 max_retry 上限。周期或错误队列中任一方先重投，旧查询快照便失效；即使 worker 已经快速完成并再次失败，也不能用上一轮快照追加重投。影响 0 行表示条件已改变，直接跳过，不计数、不补偿写回。暂停期间接受的重投保持 pending，恢复或重启后执行；关停沿用取消扫描、保留持久化任务的机制。
