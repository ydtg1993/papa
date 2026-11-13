# Papa 错误队列处理手册

> 面向：任务重试耗尽进入 `failed` 后，怎么把它重新投递回阶段重跑，以及如何防止永久坏任务无限空转。
> 配套：[RECOVER_QUEUE.md](RECOVER_QUEUE.md)（启动恢复，两者按状态分工）。

---

## 0. 一句话

任务最终失败（`status = failed`）后不会丢，持久化在 `crawler_tasks` 表。`error_queue` 在补偿条件满足后把**原记录重置为 pending 并重新入队**，支持自动轮询和手动触发，用 `reprocess` 计数限制自动补偿代数。账号、时间或人工条件仍未满足时保留失败状态，不增加代数。

先在独立 v4 数据库显式执行 `papa migrate`（业务项目使用自己的迁移入口），创建任务补偿、账号关联列及索引。普通启动不会迁移，旧版本任务身份不原地转换。补偿模式空串表示默认自动策略，不改变任务 ID、标准身份或账号池归属。

## 1. 配置

写在 `configs/config.yaml` 的全局段（**不再按站点拆**）：

```yaml
error_queue:
  enabled: true               # 自动失败重投；false / 不写 = 不自动，仍可手动触发
  worker_count: 2             # 并发重新投递失败任务的数量，开启时必填 > 0
  interval: "10m"             # 自动轮询间隔；"0" = 不自动，仅后台手动触发
  max_retry: 3                # 单个失败任务最多再处理代数；0 = 不限
  batch_size: 100             # 每批查询处理数；0 = 默认 1000，开启时不能为负
```

> 开着（`enabled: true`）时 `worker_count` 必填 > 0，`batch_size` 为 0 使用默认 1000，`batch_size` / `max_retry` 不能为负、
> `interval` 不能为负 —— 缺一项 / 越界启动就报（与 `crawler.breaker`
> 「enabled=true 时 threshold 必填」同一档）。
> 错误队列是**全局一条**：扫所有站点的 failed 任务重投回各自的阶段，后台一行。
> 队列参数是**启动时读一次**：改完要重启（后台没有热更这回事了）。

## 2. 触发方式

| 方式 | 说明 |
| --- | --- |
| 自动轮询 | `enabled: true` 且 `interval` 不是 `"0"`，后台按间隔自动投递 |
| 手动（OA） | 队列治理面板上那一行的「立即执行」（`POST /api/errorqueue/process`）。每次触发都写一条操作日志（`server.operation_log` 开启时） |
| 手动（代码） | `engine.ProcessErrorQueue()` |

## 3. 再处理代数上限（防无限重试）

错误队列或人工重投成功取得原记录时，把该任务的 `reprocess + 1`（同时 `retry` 清零）。只读预览、等待账号、等待时间、解除人工等待都不增加 `reprocess`。轮询队列沿用自己的 `repeat` / `RepeatLimit`，不会因此改成错误队列的代数模型。

错误重投属于当前轮询轮次，**不增加 `repeat`**。直接入队、队满后溢出回灌、暂停后恢复的计数一致；后台单条「重投 / 加急」同样不因经过溢出路径而额外消耗 `RepeatLimit`。历史版本已经多计的存量值不自动回退。
重新投递失败（队列满以外的提交错误）时，这一行会被标成 `failed` 并把原因追加进 `error` 列
（`engine.Engine.markRequeueFailed`，只更新仍为 pending 的行）。未注册阶段直接跳过，保留原状态；投递责任和持续 pending 补充不等同于本队列的 failed 扫描。
`max_retry > 0` 时，`reprocess >= max_retry` 的任务不再投递，避免「结构错误 / 404」这类永久坏任务无限空转。

## 4. 与错误分类配合

`WrapNoRetry` / `WrapNoRetryKind` 只停止当前阶段的立即重试，**不表示永久禁止后续补偿**。普通网络错误仍先按阶段 `MaxAttempts` / `Backoff` 执行。要声明后续条件，业务从 `github.com/ydtg1993/papa/v4/core` 返回带补偿策略的错误：

| 持久模式 | 业务写法或来源 | 放行条件 |
| --- | --- | --- |
| 空串（默认自动） | 普通错误、仅使用 `WrapNoRetry` | 符合队列状态和代数条件；有账号池的任务仍要求原池就绪 |
| `retry_after` | `core.RetryAfter(err, time.Now().Add(5*time.Minute))` | 到达持久的 `next_compensation_at`，且原池就绪 |
| `account_ready` | `ErrNeedsVerify`、确认失效、账号挑战、固定出口不可用、账号预算等待等账号错误 | 原任务的 `(site, account_pool)` 已就绪 |
| `manual` | `core.ManualCompensation(err, "等待业务审核")` | 运营显式解除任务人工等待，再检查其他条件 |
| `permanent` | `core.PermanentFailure(err, "业务确认资源永久不存在")` | 不再补偿；普通重投、轮询和人工解除均不能复活 |

这些策略都会结束当前尝试。也可以返回 `core.WithCompensation(err, core.CompensationPolicy{Mode: ..., NotBefore: ..., Reason: ...})`，或让业务错误实现 `Compensation() core.CompensationPolicy`。模式无效、`retry_after` 没有时间等配置错误会保守转为人工等待。分类保存在 `failure_kind`，脱敏理由保存在 `compensation_reason`，`compensation_version` 用于并发保护。

账号疑似异常结束本次尝试，不在任务里换号重跑。补偿器只读 `Pools.Readiness`，不登录、不核验、不切换成员；账号控制器负责恢复。依赖未知或池未就绪时不会反复消耗 `reprocess`。任务重新进入执行时，正常的账号准备过程再次检查可用性，并在认领及全局执行许可之前等待。原记录、标准任务身份和 `AccountPool` 保持不变，下一次尝试使用原池当时的可用账号。

**显式非池账号调用的边界：**任务未声明 `AccountPool` 却返回账号错误时，自动策略会阻塞并提示 `account-ready compensation requires task pool ownership`，因为引擎不能安全判断它依赖哪个池。业务应声明正确池归属，或明确返回自己的 `retry_after` / `manual` / `permanent` 策略；显式策略优先于自动账号分类。不要通过创建重复页面任务绕过等待。

错误队列、人工重投、普通/强制轮询共用时间和模式门控，任务认领的 SQL 同样检查这些条件。成功取得重投资格会递增补偿版本；自动队列与人工动作竞争时，旧版本不能再次取得这次资格。`max_retry` 仍是错误队列自动处理上限，人工重投和轮询的原有计数规则不变。

## 5. 与 recover_queue 的分工

| 机制 | 目标状态 | 语义 |
| --- | --- | --- |
| `error_queue` | `failed` | 重跑「明确失败」的任务 |
| `recover_queue` | 本进程启动前遗留的 `pending` / `processing` | 启动时恢复中断任务（以 `updated_at < startedAt` 为边界，**只在启动跑一次**，见 [RECOVER_QUEUE.md](RECOVER_QUEUE.md)） |

两者按状态互斥，不会同时盯上同一个任务。

启动恢复也遵守持久的时间、人工和永久终止条件。原有 `pending` 账号任务可以恢复排队，其执行前仍会等待账号就绪，不增加错误补偿代数。处理中孤儿的重置语句再次确认 `updated_at < startedAt`，避免旧恢复快照覆盖本进程刚认领的任务。

## 6. 预览、人工解除与受影响任务

后台任务表展示账号池、最近账号、失败分类、补偿模式、原因、最早补偿时间和补偿版本；可按站点、池、最近账号及模式筛选。任务行的「解除人工等待」只修改 `manual` 条件，不立即投递，不清除原错误，也不能解除账号自身的停用、挑战或永久终止状态。

公开引擎接口：

| 接口 | 说明 |
| --- | --- |
| `PreviewCompensation(ctx, CompensationFilter{Site, AccountPool, AccountID, Limit})` | 查询失败任务，逐项返回 `Eligible` / `BlockedReason`、版本和关联记录；默认 50、最多 200 条 |
| `ReleaseTaskCompensation(ctx, id, wasVersion)` | CAS 解除 `manual`，仍须通过账号与时间条件；成功后重新预览再重投 |
| `BatchCompensate(ctx, []CompensationRequest{{ID, Version}})` | 最多 200 项；逐项重新检查失败状态、版本和就绪条件，返回每项 `Submitted` / `Error`，支持部分成功 |
| `RetryTaskVersioned(ctx, id, wasReprocess, wasCompensationVersion)` | 任务表使用的双版本重投；前置查询、CAS 更新和冲突回查均传递调用方 context；页面过期、轮询或其他操作已修改该行时返回冲突 |

预览不是执行承诺：账号可能在预览后进入待核验，或其他人先处理任务，执行时会重新检查。`Eligible` / `Blocked` 汇总只统计本次返回的条目，`Truncated` 表示还有更多匹配数据，不是全库总数。

每条任务独立于 Trace 保存**最近 8 次认领的账号关联**，包含账号 ID、选择/凭据版本、时间、轮询轮次和补偿代数，不含凭据。`AccountID` 筛选匹配这段有界关联；任务表的「最近账号」只匹配最后一次。更早关联会被淘汰，未记录的关联也不会补造，不能把查询空结果当作该账号从未影响过任务。

账号池事件通过 `Engine.BindAccountHealthEvents(pools)` 接入现有 Notifier，通知在控制器锁外执行。等待过久、池耗尽、需要人工验证及恢复分别通知；相同站点/池/账号/事件种类每分钟最多一次。事件订阅是有界旁路，完整诊断应结合持久健康事件与补偿预览，而不是把通知数量当作任务失败数量。

### 关停期间的批处理

Engine 取消后不再开始新批次或领取同批后续任务，批间暂停为 0 时同样生效；查询和条件重置使用生命周期 context。已经成功重置的任务按原投递/溢出路径收尾，批处理等待已准入回调返回后才退出。返回计数只包含实际处理成功部分，取消不会把尚未处理的记录标成失败。
