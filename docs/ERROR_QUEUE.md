# Papa 错误队列处理手册

> 面向：任务重试耗尽进入 `failed` 后，怎么把它重新投递回阶段重跑，以及如何防止永久坏任务无限空转。
> 配套：[RECOVER_QUEUE.md](./RECOVER_QUEUE.md)（启动恢复，两者按状态分工）。

---

## 0. 一句话

任务最终失败（`status = failed`）后不会丢，持久化在 `crawler_tasks` 表。`error_queue` 负责把这些失败任务**重置为 pending 并重新入队**，支持自动轮询 + 手动触发，并用 `reprocess` 计数防止无限重试。

## 1. 配置

```yaml
error_queue:
  enabled: false      # 是否启用错误队列处理
  worker_count: 2     # 并发重新投递失败任务的数量
  interval: "10m"     # 自动轮询间隔；0 = 不自动轮询，仅手动触发
  max_retry: 3        # 单个失败任务最多再处理代数；0 = 不限
  batch_size: 1000    # 每批查询处理的任务数；0 = 默认 1000（分页流式）
```

## 2. 触发方式

| 方式 | 说明 |
| --- | --- |
| 自动轮询 | `enabled: true` 且 `interval` 非 0，后台按间隔自动投递 |
| 手动（OA） | `POST /api/errorqueue/process` |
| 手动（代码） | `engine.ProcessErrorQueue()` |

## 3. 再处理代数上限（防无限重试）

每次重新投递会把这个任务的 `reprocess + 1`（同时 `retry` 清零）。
重新投递失败（队列满以外的提交错误）时，这一行会被标成 `failed` 并把原因追加进 `error` 列
（`engine.Engine.markRequeueFailed`）。早先这里只记日志、行留在 `pending` —— 而本队列只捞 `failed`，
于是这条任务再也没人管：运营看着是"排队中"，实际永远不会执行。
`max_retry > 0` 时，`reprocess >= max_retry` 的任务不再投递，避免「结构错误 / 404」这类永久坏任务无限空转。

## 4. 与错误分类配合

配合 fetcher 里的错误分类（见 [FETCHER_WRITING_GUIDE.md](./FETCHER_WRITING_GUIDE.md)）可更精准：

- `return papa.WrapNoRetryKind("structure", err)` / `"not-found"` / `"protected"`：这些是「重试无意义」的失败，通常不该进错误队列；
- 普通可重试错误（网络抖动、超时）才值得进错误队列重跑。

`engine.ProcessErrorQueue()` 已公开，可在你自己的 cron / 调度里调用并按需加过滤（如按阶段、按 `error` 里的 kind）。

## 5. 与 recover_queue 的分工

| 机制 | 目标状态 | 语义 |
| --- | --- | --- |
| `error_queue` | `failed` | 重跑「明确失败」的任务 |
| `recover_queue` | `pending` / `processing` | 启动时把中断留下的任务全部捡回来（**只在启动跑一次**，见 [RECOVER_QUEUE.md](./RECOVER_QUEUE.md)） |

两者按状态互斥，不会同时盯上同一个任务。
