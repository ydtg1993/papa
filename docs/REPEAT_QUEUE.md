# Papa 周期轮询队列手册

> 面向：想让标记了 `Repeatable: true` 的任务（如 catalog 目录页）被周期性地重新抓取、发现新内容。
> 配套：[SCHEDULER.md](./SCHEDULER.md)（业务自定义 cron）、[RECOVER_QUEUE.md](./RECOVER_QUEUE.md)（启动恢复）。

---

## 0. 一句话

框架内置一个「周期轮询队列」`repeat_queue`：按 `interval` 定时把**已完成**（success/failed）的 repeatable 任务重新投递回各自阶段，实现「周期重跑轮询任务」。走的是与 error_queue 同构的分页 + 并发队列（分页用 keyset 游标，见 [RECOVER_QUEUE.md](./RECOVER_QUEUE.md) 第 6 节）。

## 1. 配置

```yaml
repeat_queue:
  enabled: true       # 是否启用周期轮询 repeatable 任务
  worker_count: 2     # 并发重新投递 repeatable 任务的数量
  interval: "10m"     # 轮询间隔；0 = 不自动轮询，仅手动触发
  batch_size: 1000    # 每批查询处理的任务数；0 = 默认 1000（分页流式）
```

## 2. 触发方式

| 方式 | 说明 |
| --- | --- |
| 自动轮询 | `enabled: true` 且 `interval` 非 0，后台按间隔定时重投 |
| 手动（OA） | `POST /api/repeatqueue/process` |
| 手动（代码） | `engine.RepollRepeatableTasks()` |

## 3. 语义：只重投「已完成」的

`repeat_queue` 只重投 `status = success / failed` 的 repeatable 任务，**不碰还在 pending/processing 的**——后者由 [RECOVER_QUEUE.md](./RECOVER_QUEUE.md) 在**启动时**兜底。这样避免「上一轮还没处理完，这一轮又入队」的双入队。
重新投递失败（队列满以外的提交错误）时，这一行会被标成 `failed` 并把原因追加进 `error` 列
（`engine.Engine.markRequeueFailed`）。早先这里只记日志、行留在 `pending` —— 而本队列只捞 `failed`，
于是这条任务再也没人管：运营看着是"排队中"，实际永远不会执行。


## 4. 与业务 cron 的分工

| 能力 | 归属 | 调度方式 |
| --- | --- | --- |
| 周期轮询 repeatable（推荐） | 本文 `repeat_queue` | interval 定时 |
| 业务自定义定时任务 / 指定时刻轮询 | [SCHEDULER.md](./SCHEDULER.md) 的 `RegisterCronJob` + `RepollRepeatableTasks` | cron（6 段秒级） |

> 如果轮询需要「每天 08:00 整点」这种 cron 语义，用 `RegisterCronJob`；如果只要「每隔 N 分钟」，用 `repeat_queue`。
