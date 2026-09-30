# Papa 定时任务手册

> 面向：想在业务层注册自己的定时任务（周期轮询、对账、清理等）。
> 背景：框架**不再内置** `repeat` / `recover` 定时 job；业务定时任务统一通过 `app.RegisterCronJob` 注册，框架级恢复/失败重试走 [RECOVER_QUEUE.md](./RECOVER_QUEUE.md) / [ERROR_QUEUE.md](./ERROR_QUEUE.md)。

---

## 0. 一句话

```go
app.RegisterCronJob("名字", "0 0 8 * * *", func() { /* 业务逻辑 */ })
```

在 `papa.New()` 之后、`app.Run(ctx)` 之前调用即可，任务随框架一起启动/优雅停止。

## 1. RegisterCronJob 用法

```go
app, err := papa.New()
// ... RegisterStage ...

// 每天 08:00 轮询所有 repeatable 任务
app.RegisterCronJob("repeat_daily", "0 0 8 * * *", func() {
    if n, err := app.Engine.RepollRepeatableTasks(); err != nil {
        app.Logger.Scheduler.Errorf("repoll: %s", err.Error())
    } else {
        app.Logger.Scheduler.Infof("repoll submitted %d tasks", n)
    }
})

// 另一个自定义任务
app.RegisterCronJob("每日对账", "0 30 2 * * *", func() {
    app.Engine.RecordMetric("对账结果", map[string]any{"ok": true})
})

app.Run(ctx)
```

要点：

- **必须在 `Run` 之前调用**（`Run` 里才启动调度器）。
- `fn` 是普通 `func()`，通过**闭包**访问 `app.Engine` / `app.DB` 等资源。
- 时区沿用 `scheduler.timezone`；生命周期随框架优雅停止。

## 2. Cron 表达式（6 段、秒级）

调度器用 `cron.WithSeconds()`，格式为 **6 段**：

```
秒 分 时 日 月 周
```

| 示例 | 含义 |
| --- | --- |
| `"0 0 8 * * *"` | 每天 08:00:00 |
| `"0 30 2 * * *"` | 每天 02:30:00 |
| `"*/10 * * * * *"` | 每 10 秒 |
| `"0 0 8 * * 1"` | 每周一 08:00:00 |

> ⚠️ 旧的 5 段写法（如 `"0 8 * * *"`）在秒级解析器下会**解析失败**，务必用 6 段。

## 3. 引擎已公开的「队列/轮询」方法

这些方法本身不发定时，供你在 `RegisterCronJob` 的 `fn` 里（或任意代码里）直接调用：

| 方法 | 作用 | 返回 |
| --- | --- | --- |
| `engine.RepollRepeatableTasks()` | 重新投递所有 `repeatable` 任务 | `(投递数, error)` |
| `engine.ProcessErrorQueue()` | 手动触发失败任务重投 | `(投递数, error)` |
| `engine.ProcessRecoverQueue()` | 手动触发卡死任务恢复 | `(恢复数, error)` |

## 4. 与其它机制的分工

| 能力 | 归属 | 触发方式 |
| --- | --- | --- |
| 业务自定义定时任务 | 本文（`RegisterCronJob`） | 自定 cron |
| 周期轮询 repeatable 任务 | `RepollRepeatableTasks` | 业务用 cron 触发 |
| 失败任务重试 | [ERROR_QUEUE.md](./ERROR_QUEUE.md) | interval + 手动 |
| 卡死任务恢复 | [RECOVER_QUEUE.md](./RECOVER_QUEUE.md) | 启动时 + interval + 手动 |
