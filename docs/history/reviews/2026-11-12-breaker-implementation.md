# 分类熔断施工与验收

<!-- version-scope -->
> **适用版本/阶段：Papa v4 演进过程中的历史工作区快照（具体基线以正文为准；未记录精确发布版的条目不推定版本号）。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 当前使用方式以 [v4 手册导航](../../README.md) 为准。
<!-- /version-scope -->


范围：Papa v4 单实例框架。原独立设计见 [分析方案](2026-11-12-breaker-analysis-design.md)，实际接入方式见 [分类熔断手册](../../runtime/BREAKER.md)。本次不改 crawler 业务模块，不发布、不部署、不提交。

最新规则以末节“整站恢复与运行过程报告”为准：用户进一步明确局部成功只能记录证据，不能独立恢复阶段；这覆盖下文早期的局部恢复和人工清零语义。

## 已实现

1. 终态任务轮次去重，分类和稳定指纹；同类短时快速保护、混合受限观察、时间窗口失败总数硬线。成功不会减去失败，容量耗尽显式进入保护。
2. 复用请求治理器处理 429/Retry-After，只有有效 AutoThrottle 开关开启才自动增加间隔。合法远端期限始终尊重；事件记录的期限在重启时恢复。
3. 观察→冷却→受限语义探测→限量恢复→完成；混合观察证据足够时也可直接退出。探测前保存预算，恢复前保存状态，预算耗尽或探针缺失/超时则保持暂停并转人工。人工暂停可以接管自动事件；旧作用域探针不能放行新范围，真实任务的晚到终态继续计数。
4. `crawler_breaker_incidents` 持久报告，支持修订号防覆盖、阶段保护扩大整站、重启读取、最终报告保留、后台文本/JSON 导出。报告分别呈现事实、假设、反证和缺失信息，不能证实宕机时明确保持不确定。
5. 配置字典、脚手架、Fetcher、运行治理、后台及开发手册同步。公开 API 为 `WithFailureCode` 和 `Engine.SetBreakerProbe`，没有单独引入策略注册体系或路由调度器。

## 验证与修正

- 全包 `go test ./... -timeout 180s`、`go vet ./...`、`go build ./...` 通过。
- 检测、分类、引擎恢复、持久层、限流的针对性 race 通过；HTTP/CDP 限流合成回归通过。脚手架生成项目的 tidy/build 测试通过。
- 隔离 MySQL 通过 `TestBreakerStoreMySQLLifecycleRestartAndRevision` 和 `TestSmartBreakerMySQLRestartAndManualClosure`，覆盖重启恢复、原恢复截止时间、人工关闭、最终历史、版本约束、作用域扩大及非 UTC 连接时区。测试创建并清理随机测试库，没有迁移业务数据库。
- `TestSmartBreakerFastScopeMixedObservationAndHardCeiling`、`TestSmartBreakerPersistenceFailureNeverReleasesAdmission`、`TestSmartBreakerLateProbeCannotOverrideManualTakeover`、`TestSmartBreakerSemanticSuccessPersistsBeforeRamping` 等覆盖关键恢复边界；Stop 最后一批状态也有回归。
- 首轮全包测试发现配置守卫不接受聚合路径，原文为 `规则键 "crawler.breaker.policy"（规范化后 "crawler.breaker.policy"）在 Config 结构体里找不到`；校验落在真实叶子路径，并继续校验完整策略。旧人工暂停快照回归发现 `已经停着时 PauseSite 应当返回 false`；仅有恢复所有权的自动暂停支持接管，旧无自动恢复的暂停保留原契约。
- 并发复查修正：报告追加证据不再废弃探针预算；其他阶段在途任务不占目标阶段恢复并发；整站升级立即回冷却，旧阶段探针不能解除整站保护；成功证据也递增报告修订号。

## 使用前提与实际限制

升级需显式迁移新增表；正常启动不迁移，读取持久状态失败则停止启动。需要自动恢复的站点应注册只读语义探针；框架不会自行调用完整 Fetcher。没有探针时仍能分类、保护和生成报告，恢复需人工处理。

报告通常按 200ms 周期刷写，恢复前等待保存确认，优雅停机补写；强杀仍存在最后一轮未刷写状态丢失的窗口，任务终态和事件快照不具有跨表原子事务。分类和统计不自动证明根因，路由/账号/代理影响仍需业务探针对照验证。没有运行真实站点的故障演练，没有部署服务。


## 后续收敛：人工暂停与唯一数据源

用户澄清人工暂停只在当前运行周期内有效。人工接管整个自动保护范围时结束自动事件，但保留故障历史；人工范围更窄时只叠加内存暂停。自动保护因预算耗尽进入的待人工处理状态仍会恢复。旧文案中“人工接管状态跨重启恢复”由此更正。

已删除旧桶计数器、旧引擎证据缓存/内存历史、`Engine.BreakerIncidents` 和 `MonitorConfig.BreakerIncidents`，也删除了 obs 从任务日志重建另一份熔断报告的路径。Detector 同一份终态结果提供计数和触发证据；Lifecycle 是唯一事件，事件流/后台/日报共用 ID。日报读取同一持久报告，离线只提供引用，不补造证据。

验证：全包 test/vet/build 通过；engine/breaker/store/analyze 的针对性 race 及隔离 MySQL 通过。新增 `TestBreakerReportsUseOneIDAndRetainHistory`、`TestBreakerDetectorOwnsBoundedDeduplicatedEvidence`、`TestManualTakeoverDoesNotRestoreAfterImmediateStop`、`TestNarrowManualPauseDoesNotOwnSiteProtection`、`TestBreakerReportReferencesDurableIDWithoutReconstruction`；MySQL 回归改用实际人工接管再重启，覆盖新报告按 ID 读取。

上述清理完成时，复查第 2—5 项尚待修；用户随后确认修复，结果见下节。

## 复查 2—5 项修复

- 阶段恢复不再切换整站 epoch。Detector 标记对应阶段旧短窗口故障已处理，保留全部硬窗口失败及轮次去重，其他阶段旧在途任务的终态照常接收。人工单阶段恢复同样隔离；显式人工整站恢复仍可重置窗口。
- 限量恢复到期后先关准入、再等范围内在途任务结束。晚失败回冷却；局部恢复不等待其他阶段。结案报告还在等待保存时遇到新硬失败，继续同一个事件并扩大保护，不能丢掉该失败。
- 管理与写库使用两个固定有界的 timer 循环：正常写入间隔 200ms、失败重试间隔 1s、单批写入 context 2s。写入槽等待可取消。以已保存控制修订判断能否放行，追加证据不无限拖住状态推进，超时保护独立于报告写入。
- 混合观察最后一次失败后满 observe_for，且每个受影响阶段至少两个不同任务成功，排空在途任务后退出观察；覆盖不足/证据截断则保守探测。内部到达序号区分系统时钟值相同的先后结果，失败之前的成功不证明之后已恢复。

新增回归：`TestDetectorResolveOnlyAcknowledgesScopedShortFaults`、`TestDetectorObservationOrdersEqualTimestamps`、四个 `TestReview*` 复现、`TestSmartBreakerEvidenceUpdatesDoNotStarveProbe`、`TestSmartBreakerBlockedWriterCannotDelayDeadline`、`TestSmartBreakerWriterTimesNormalFlushAndFailureRetry`、`TestSmartBreakerObservationRequiresAffectedStagesAndFreshSuccess`、`TestSmartBreakerManualStageRecoveryKeepsOtherStageOutcome`、`TestSmartBreakerRestartDuringRampReturnsToCooldown`、`TestSmartBreakerUnsavedRecoveryCannotLoseNewHardFailure`。

未引入第二套计数器、策略注册器、通用状态机或配置项；新增 LastFailureAt 只存在已有 JSON 报告中，无需新增列。精确 Detector 窗口不跨重启重建，重启恢复的是活动保护、剩余预算及报告；强杀未刷写窗口的限制继续保留。

最终验证：`go test ./... -timeout 180s`、`go vet ./...`、`go build ./...`、`git diff --check` 通过；engine、internal/breaker、internal/breakerstore、obs/incident 的上述熔断定向回归在 race 下通过，包含已启用连接配置的隔离 MySQL 重启/持久层测试。模板生成构建回归随全包测试通过。只改工作区，未提交、未部署，未运行真实站点故障演练。

## 整站恢复与运行过程报告

用户进一步要求全阶段验证正常后，才评定整站恢复并清空计数；运营必须看到暂停、恢复和重试的完整过程。

- 自动保护改为整站，阶段仍用于错误分类。阶段清单来自注册表，包括未运行阶段；一个阶段通过不单独放行。逐阶段调用专用探针或站点兜底探针，一整轮只消耗一次探测预算。任一阶段失败后整站冷却，下轮全部重新验证。
- 全阶段试探通过后才能开始整站限量真实任务，每阶段至少一个本轮任务成功、观察时间完成、全站在途任务排空后，保留清零前报告并清空 Detector。最终状态未保存不全量放行。混合观察要求所有注册阶段各有两个不同任务成功，不能用局部成功代替。
- 删除已经不再使用的局部 Resolve 和阶段在途计数路径。人工解除暂停不构成健康证明，保留失败计数；人工暂停依然是进程内状态。全部阶段验证成功是运行时唯一主动清零路径，重启仍按既有机制重建内存 Detector、恢复持久保护与预算。
- Lifecycle 增加最多 128 个阶段验证进度项与清零时间；同一条时间线带阶段、轮次、窗口计数和重试计划。记录阶段试探开始/结果、真实任务验证、等待在途、暂停冷却、重启、人工操作及整站恢复。obs 通过 breaker_transition 引用同一持久报告，包含只观察后恢复而未触发暂停的事件。
- 后台直接展示阶段表及运行时间线，3s 刷新并保留历史选择，关闭即停止；文本/JSON 使用同一数据源。阶段表和时间线有界且脱敏，不新增独立报告或表。缺少探针/真实任务或超出阶段容量时明确保留未验证状态，不猜测通过。

回归：`TestSmartBreakerLocalSuccessCannotReopenSite`、`TestSmartBreakerChecksIncludeIdleRegisteredStage`、`TestSmartBreakerAllStagesDrainThenResetWithReport`、`TestSmartBreakerStageFailureInvalidatesWholeRound`、`TestSmartBreakerMissingStageProbeStopsWholeSite`、`TestSmartBreakerOldTaskSuccessCannotProveNewTrial`、`TestManualResumeDoesNotCertifySiteOrClearFailureWindow`；原并发、截止期限、晚结果和重启用例按整站契约更新。MySQL 持久层用例增加阶段结果/重试时间线/清零时间读取校验。真实 Chrome 验证桌面与 390px 布局、阶段进度、时间线和 HTML 注入防护。

本轮最终验证：全包 `go test ./... -timeout 180s`、`go vet ./...`、`go build ./...`、`git diff --check` 通过；熔断引擎/检测器/持久层/obs 的定向 race（包含隔离 MySQL）通过。真实 Chrome 桌面及窄屏、Node 弹窗刷新/保留历史选择/关闭和登出后停止请求均通过。全包首次发现 Node 模拟环境缺定时器，原文 `ReferenceError: setTimeout is not defined`；补有界假定时器并验证刷新行为后通过。没有提交、部署或真实站点故障演练。
