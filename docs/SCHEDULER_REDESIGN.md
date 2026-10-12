# 调度器 / 工作池重设计（大改版）

> 状态：**设计已拍板；第 1–5 步全部落地（重设计完成）**。遗留的 OA 面（§5）、预留接口（§6）与
> docs-sync / CHANGELOG 见文末。
> 决策日期：2026-10-12。这份文档是下次迭代的接续依据，别每次重推一遍。

## 0. 一句话

把「每阶段**常驻**工作池 + 三个队列扫描器各自 ticker + 一套 cron 调度器」改成
「**中央 submit 调度器 + 懒建短命工作池 + 周期扫描器**」，并删掉冗余的 cron 调度器
（`admin/scheduler` / `RegisterCronJob`）。

## 1. 现状（对照代码）

- **工作池**：每阶段一个，`ApplyRegisterStage` 第一趟**全量建**、活到 `Stop`
  （engine/stage.go:132-143）。池内两条队列（常规 + 加急快车道），绑 per-site 熔断闸门。
- **三个「治理队列」不是池，是扫描器**：error/repeat 用 `engine.runDynamicTicker` 定时
  `SELECT` 到期任务 → 条件重置 pending → 投回**该阶段的池**；recover 启动跑一次
  （errorqueue.go / repeatpoll.go / recoverqueue.go）。共用 `processInBatches`（keyset 分页）
  + `processConcurrently`（临时 goroutine 扇出，不是持久池）。
- **两套「定时」**：`engine.runDynamicTicker`（interval 动态节拍 + 数据唤醒）与
  `admin/scheduler`（robfig/cron v3 秒级，业务走 `RegisterCronJob`）。

## 2. 已拍板的决策

- **D1 — 三队列继续是「扫描器」，error/repeat 留在 `runDynamicTicker`，不迁 cron。**
  理由：repeat 的动态节拍（`min(全局 interval, 距最早一条到点)`）+ 数据唤醒
  （`wakeRepeatQueue`）是 per-task `repeat_interval`（B 档）的承重墙，cron 表达不了；
  error 是纯 interval，没必要为它单独引 cron。`error_queue` / `repeat_queue` /
  `recover_queue` 三个配置段**原样保留**。
- **D2 — 删 `admin/scheduler` + `RegisterCronJob` + `scheduler` 配置段 + `docs/SCHEDULER.md`。**
  已核实：crawler 无真实调用（只有 docs 提及），`robfig/cron` 在 crawler 是**间接依赖** →
  删了只动文档、不伤业务代码。
- **D3 — 池模型 = 每阶段一个池，任务类型（error/repeat/recover/常规/加急）只是路由/优先级标签，不拆池。**
  **recover 走加急快车道**（`requeueRecoverTask` 里 submit 前 `task.Urgent = true`；`claimTask`
  认领时把 urgent 归零，语义正确：恢复的任务插队先跑，跑完变普通）。
- **D4 — 池从「常驻」改「懒建 + 空闲回收」**：首次有任务才建，空闲 N 分钟退出。N 进
  `config` 的 `crawler:` 段（新配置项，如 `crawler.stage_idle_timeout`）。
- **D5 — 新增中央 submit 调度器（一个协程收所有 submit）+ 活池登记表（带锁）。**

## 3. 常驻骨架 vs 短命池（keep / cut 结论）

| 项 | 结论 | 怎么动 |
| --- | --- | --- |
| **熔断闸门** | **留（常驻）** | per-site，已在池生命周期之外（`breakerFor(site)` 返回常驻闸门）。池每次懒建 `SetGate(同一把)` 再 `Start`。砍了它，被闸住的站重建池后会照样猛打。 |
| **Stop** | **留，改形状** | 从遍历 `e.stages` 停所有池 → 停**活着的池**（活池表带锁，防与懒建并发）。`drained` 契约（没排空别关库/关浏览器）更重要了：空闲回收绝不能在任务跑到一半时 Stop。 |
| **drain / spill** | **留机制，折进中央调度器** | 「队列满 → 不丢、稍后重试」与池寿命无关（突发照样灌满短命池）。spill 列表不再单开 goroutine，折进中央调度器的「稍后重投」路径，否则池被回收了、spill 里还躺着它的任务。 |
| **跨类型背压** | **不动** | A 方案（单池 + 有界快车道 `urgentMinQueueSize`）自带，在池内部，与生命周期无关。B 方案才会把它变成要重建的东西。 |
| **监控 stats** | **留能力，计数抬出池** | 累计计数进 engine（按 stage 记），池只留「当前实例」计数；`track.StatsQueue` 随池建随池死。**本次不动，见 §5。** |

## 4. 新增件的形状

**中央 submit 调度器**（一个协程，收所有 submit）：`SubmitTask` / `SubmitTasks` /
三队列扫描器 / delay / spill **全部走它**。收到任务 → 分析 stage（site 是 stage 的
`config.Site` 属性，type 只决定走不走快车道）→ 查活池表：有 → 投递；无 → 按
`StageConfig` 建池 + 取 fetcher + `SetGate(breakerFor(site))` → 投递。**投递必须非阻塞**：
池满走溢出，不能反过来堵住调度器。

**活池登记表**：key = **stage 名**（`e.stages` 是 `map[string]*stageInfo`，stage 名全局唯一、
自带 Site，无需把 site/type 塞进 key）。值 = 池 + fetcher + `lastActivity`。建 / 查 / 回收
都走它、带锁；`stageInfo.workerPool` 这个裸指针字段**退掉**。

## 5. 本次不动（OA 后台面，留下次迭代）

原则：**这次只动 engine 内部 + 删 cron，不碰 `admin/`。**

- 监控 stats 计数搬家（后台读 `e.statsQueue[stage]` 的那批）。
- `mdMsgListener` / `Engine.Errors()` 的活池化（`internal/app/app.go`）。
- 后台三队列手动入口、oao 表格、站点页 —— 不动。`ProcessErrorQueue` /
  `RepollRepeatableTasks` / `ProcessRecoverQueue` **签名保持不变**，只是内部改走中央
  submit 口，所以 admin 里 `ProcessErrorQueue: a.Engine.ProcessErrorQueue` 这类接线不受影响。
- docs 同步（CORE_CONFIG / DEVELOPMENT / FETCHER_WRITING_GUIDE / REPEAT_QUEUE / README 里
  所有 `RegisterCronJob` 与 `scheduler` 段字样）—— 归下次 docs-sync。

## 6. 预留的后期接口（下次迭代接 OA 用）

```go
// 监控/后台读活池与累计计数用（本次不实现，签名先定下来，改内部不伤 admin 接线）。
type StagePoolStats struct {
    Stage      string
    Submitted  int64
    Completed  int64
    Failed     int64
    InProgress int64
    QueueDepth int
}

// LiveStageStats 活池快照 + 累计计数（池 churn 不清零）。
func (e *Engine) LiveStageStats() []StagePoolStats

// LiveErrors 活池错误通道列表（替代现在遍历 e.stages[*].workerPool 的 Errors()）。
func (e *Engine) LiveErrors() []<-chan error
```

手动入口三个方法（`ProcessErrorQueue` / `RepollRepeatableTasks` / `ProcessRecoverQueue`）
签名不变，不另留。

## 7. 落地顺序（增量）

1. **中央 submit 口 + 活池登记表**（不改行为，只收口）。**✅ 已完成（2026-10-12）**
   —— 新增 `engine/pools.go`（`poolRegistry`：`set/get/delete/snapshot`）+ `Engine.pools` 字段；
   `stageInfo.workerPool` 已退掉；`submitTo(info, task)` 改成 `submitToStage(task)`（查活池表 + 路由加急/常规），
   新增 `poolFor(stage)`；`Stop`/`Errors` 改读 `e.pools.snapshot()`。投递仍是同步、池仍常驻（懒建/回收留给 §3/§5）。
   影响：`submit.go` / `spill.go` / `delayqueue.go` / `errorqueue.go` / `repeatpoll.go` /
   `recoverqueue.go` / `taskadmin.go` / `stage.go` / `engine.go`。
2. **删 cron**：`admin/scheduler` + `RegisterCronJob` + `SchedulerConfig` + `docs/SCHEDULER.md`；
   `go.mod` 去掉 `robfig/cron`（`go mod tidy`）。**✅ 已完成（2026-10-12）**
   —— 删 `admin/scheduler`（scheduler.go + scheduler_test.go）与 `docs/SCHEDULER.md`；app.go 去掉
   `RegisterCronJob` / `schedule` / `customJobs` / `cronJob`；config.go 去掉 `SchedulerConfig`；模板去掉
   `scheduler:` 段；README / DEVELOPMENT / CORE_CONFIG / FETCHER_WRITING_GUIDE / REPEAT_QUEUE 里的
   RegisterCronJob / SCHEDULER.md 引用同步清掉。`LoggerSet.Scheduler` 日志字段**保留**（业务可能仍用，
   不在本步范围）。全测试绿。
3. **池懒建**：`ApplyRegisterStage` 第一趟不再建池，改首次 submit 建；fetcher 先复用注册值
   （**不改 `AddStage` 签名**）；`Stop` 内部改停活池；stats 先用 §6 预留接口顶着，OA 面不动。
   **✅ 已完成（2026-10-12）**
   —— 抽出 `stageHandler(info)` / `buildPool(stage, info)` / `getOrCreatePool(stage)`；
   `ApplyRegisterStage` 只留依赖校验 + 下载器解析 + 站点行 + 入口回调 + 后台协程，不再全量建池；
   `submitToStage` 改调 `getOrCreatePool`（懒建）；`poolFor` 删掉；`submitToPool` 的「先判池」删掉，
   `RetryTask`/`UrgentTask`/`drainSpilled` 改判 `e.stages[stage] == nil`；registry 增 `getOrCreate`。
   stats 队列随池一起在 `buildPool` 里懒建（监控 `GetStageStats` 仍读 `e.statsQueue`，只是某阶段
   首次 submit 前不显示）。全测试绿。
4. **recover 走加急**：`requeueRecoverTask` 里 `task.Urgent = true`（一行）。**✅ 已完成（2026-10-12）**
   —— 恢复的任务是上一轮没跑完的孤儿，插到队首先跑完；`claimTask` 认领时把 urgent 归零，只是排队位置。
   `TestRequeueRecoverTaskResubmits` 收紧为断言「urgent=1 / main=0」。
5. **空闲回收**：活池表记 `lastActivity`，idle N 分钟回收（N 进 `crawler:` 段）；**有 inFlight 或队列非空不回收**，防误杀在途。
   **✅ 已完成（2026-10-12）**
   —— 新增 config `crawler.stage_idle_timeout`（<=0 不回收，默认常驻）；`poolRegistry` 记 `lastSubmit`，
   `getOrCreate` 每次投递都刷新它；新增 `reapIdle(idle, stopTimeout)`（判「lastSubmit 超 idle + 队列空 + 无在途」
   才 Stop + 移除）；`startIdleReaper` 协程每 idle/2 扫一次（进 tickerWG）；`getOrCreatePool` 去掉 get 快路径
   （保证每次投递都刷新 lastSubmit）；池回收时 `removeStageStats` 配套清 stats（`track.StatsQueue` 新增
   `Stop()` 停掉监控 goroutine，否则泄漏到引擎结束）。测试：`TestPoolRegistryReapIdle` /
   `TestPoolRegistryGetOrCreateRefreshesLastSubmit`。

---

## 重设计已落地，遗留清单（下次迭代）

1. **CHANGELOG**（按 [[changelog-append-only]] 记 [未发布] 段）：删 `RegisterCronJob` + `scheduler:` 段、
   池懒建 + `stage_idle_timeout` 空闲回收、recover 走加急 —— 这几处是行为/API 变更，要写清症状/修法/为什么。
2. **docs-sync**：`CORE_CONFIG.md` 补 `crawler.stage_idle_timeout`；`RECOVER_QUEUE.md` 补「恢复走加急」一句。
   （step 2 已把 RegisterCronJob / SCHEDULER.md 的引用清掉了。）
3. **§5 的 OA 面**（仍没动）：监控 stats 的累计计数抬出池（池 churn 不清零）、`mdMsgListener` / `Engine.Errors()`
   的活池化 —— 接 §6 预留的 `LiveStageStats()` / `LiveErrors()`。

## 8. 爆炸半径清单

- **代码**：`engine/{stage,submit,spill,delayqueue,errorqueue,repeatpoll,recoverqueue,taskadmin,engine}.go`
  （`dynticker.go` **保留**）；`internal/workerpool` 预期不动；`admin/scheduler`（删）；
  `internal/app/app.go`（去 `RegisterCronJob` / `schedule` / `customJobs`）；
  `config/config.go`（去 `SchedulerConfig`）+ `validate.go`（若有对它的规则）。
- **文档/模板**：`docs/SCHEDULER.md`（删/重写）、`CORE_CONFIG.md`、`DEVELOPMENT.md`、
  `FETCHER_WRITING_GUIDE.md`、`REPEAT_QUEUE.md`、`README.md`；`cmd/papa/templates/config.yaml.tmpl`。
- **业务侧**：crawler 无代码改动；`robfig/cron` 在 crawler 变间接→`go mod tidy` 后消失。
  **核对** crawler 的 `configs/config.yaml` 是否写了 `scheduler:` 段（写了要删，否则
  「未知键拒绝启动」会 panic）。
