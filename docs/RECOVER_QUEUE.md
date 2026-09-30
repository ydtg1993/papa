# Papa 中断恢复队列手册

> 面向：主程序崩溃 / 重启后，怎么把卡在 `pending` / `processing` 的任务捡回来继续跑。
> 配套：[ERROR_QUEUE.md](./ERROR_QUEUE.md)（失败任务重试，两者按状态分工）。

---

## 0. 一句话

进程在抓取中途挂了，会留下一批停在 `pending`（已入库未入队）或 `processing`（入队了但没处理完）的任务。`recover_queue` 负责在**启动时立即** + 按 `interval` 定时，把这些「卡死」任务重置为 `pending` 并重新入队。

## 1. 配置

```yaml
recover_queue:
  enabled: true       # 启用后启动时立即恢复一次
  worker_count: 2     # 并发恢复数量
  interval: "10m"     # 自动轮询间隔；0 = 仅启动时 + 手动触发
  timeout: "6h"       # 任务卡住多久算卡死（updated_at 早于 now-timeout）
```

## 2. 触发方式

| 方式 | 说明 |
| --- | --- |
| 启动立即恢复 | `enabled: true` 时，启动后异步恢复一次，避免重启后等下一轮 interval |
| 自动轮询 | `interval` 非 0 时按间隔定时恢复 |
| 手动（OA） | `POST /api/recoverqueue/process` |
| 手动（代码） | `engine.ProcessRecoverQueue()` |

## 3. 判定逻辑

恢复的条件是：`status IN (pending, processing)` 且 `updated_at < now - timeout`。即「卡了超过 `timeout` 还没动静」的任务才会被捡回；刚提交、仍在正常处理中的任务不受影响。

## 4. 与 error_queue 的分工

| 机制 | 目标状态 | 语义 |
| --- | --- | --- |
| `recover_queue` | `pending` / `processing`（超时） | 救「卡死」的任务 |
| `error_queue` | `failed` | 重跑「明确失败」的任务 |

一个任务同一时刻只属于其中一种状态，两者不会重叠处理。
