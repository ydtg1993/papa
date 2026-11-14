# 整站恢复复查修复与验收

<!-- version-scope -->
> **适用版本/阶段：Papa v4 演进过程中的历史工作区快照（具体基线以正文为准；未记录精确发布版的条目不推定版本号）。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 当前使用方式以 [v4 手册导航](../../README.md) 为准。
<!-- /version-scope -->


范围：Papa 通用框架，单实例多阶段并发。修复本轮五项复查问题，区分人工重试与强制解除，提供空闲阶段验证方案。未改业务模块、未提交 Git、未部署。

## 修复结果

| 问题 | 当前行为 | 主要回归 |
| --- | --- | --- |
| 报告截断误暂停 | 控制使用独立 Recovery 与 Detector；Lifecycle 只投影，时间线截断不影响健康判断 | `TestBreakerReviewTimelineTruncationMustNotInvalidateHealthyStages` |
| 阶段名裁剪导致验证卡住 | 控制、探针查找和真实任务匹配使用原始标识；报告允许数据库合法的 128 个 Unicode 字符 | `TestBreakerReviewReportCleaningMustPreserveValidStageIdentity` |
| obs 遗漏长期暂停/最近更新事件 | 数据库查询活动与窗口相关事件，离线按最后出现顺序引用，上限显式提示 | `TestBreakerReportLoadsSilentActiveIncidentWithoutLogEvents`、`TestBreakerStoreMySQLWindowIncludesSilentActiveAndMarksLimit` |
| 超时后阶段仍显示试探中 | 总预算到期、人工结束等路径补中断状态、结束时间、原因；晚结果不恢复站点 | `TestBreakerReviewDeadlineMustFinalizeRunningStageReport`、`TestBreakerReviewLateHealthyProbeCannotBeatDeadline` |
| 重启提前清空失败数 | 已形成事件的失败轮次、原始时间、去重身份、容量保护与控制快照一同保存；重启丢弃旧成功证明并逐阶段重验 | `TestCheckpointRestoresFailureIdentityAndOriginalExpiry`、`TestSmartBreakerMySQLRestartAndManualClosure` |

## 人工操作

`ResumeSite` / `ResumeCrawling` 请求新的有界恢复尝试，保留失败数、整站保护和历史。活动探针尚未退出时不重叠发起。纯人工暂停直接解除；同一进程内接管过自动事件的人工暂停，普通恢复仍重验。显式整站恢复会解除该站的进程内人工暂停，同时重新建立自动保护。

`ForceResumeSite` 与 `/api/breaker/force-resume` 明确绕过验证，必须指定单站；后台有独立确认入口，操作日志为 `force_resume`，事件关闭但不标记健康恢复、不清空计数。新失败仍可以重新触发保护。

回归：`TestBreakerReviewRetryKeepsProtectionAndCounts`、`TestBreakerReviewRetryDoesNotOverlapTimedOutCallback`、`TestBreakerReviewManualTakeoverResumeStillVerifiesWithinRun`、`TestBreakerRetryAndForceAreSeparateEndpoints`、Node `breaker_control_test.js`。

## 并发与重启复查

- 控制变更、准入、终态记录在同一把 breaker 锁下；写库不持准入锁，写入槽仍可取消。
- 阶段证明绑定本轮受控任务，旧任务成功不能证明新一轮健康；任何验证期晚失败取消恢复，最终放行仍等在途排空和控制快照保存。
- 探针包装器与扩展回调分别跟踪，超时回调未退出时拒绝重复重试。旧结果还需匹配事件、状态和轮次。
- 准入、管理定时器和探针结果各自检查总预算，避免等下一次 tick 才拒绝超时操作。
- 成功证明不跨重启复用；自动待人工处理仍保持保护，人工暂停及接管标记仅在当前进程有效。原始失败时间保持不变，重启不延长统计窗口。
- 每次保存快照分配新修订。补充复现“提交成功但确认丢失，重试前窗口过期”，避免相同修订内容冲突锁住控制保存：`TestBreakerReviewLostCommitAckAndWindowExpiryCanRetry`。
- 配置容量缩小时，保留已有有效失败记录并启用容量保护，新失败不继续扩张记录：`TestCheckpointSmallerCapacityKeepsKnownFailuresBounded`。

没有增加恢复管理器、报告服务或第二套计数器。Recovery 是控制数据，Checkpoint 是 Detector 的持久副本，Lifecycle 是只读报告；仍在原事件 JSON 列中按修订一起保存。已删无生产调用的旧按 ID 查询、仅活动事件启动查询，以及全部直接放行内部旁路。

## 验证

- `go test ./...`：完整套件；首轮一个旧用例仍将普通恢复当作强制放行，已改用显式 ForceResumeSite，保留原先计数不能清零的断言。
- `go test -race`：覆盖 engine 熔断、报告并发、人工操作、Checkpoint、存储与 obs；100 轮并发人工操作/失败回归通过，随后新增的保存确认丢失回归也通过 race。
- 真实 MySQL：使用随机隔离测试库验证事件与窗口重启、关闭事件后的计数、修订冲突、静默活动事件、时间范围与展示上限；未改业务数据库。
- Chrome 真实浏览器及 Node：报告显示/刷新和独立重试、强制解除请求路径通过。
- `go vet ./...`、`go build ./...`、`git diff --check`。

本机日志位于 `.review-tmp/breaker-fixes-complete-*.log`；浏览器日志为 `.review-tmp/breaker-fixes-browser.log`，100 轮并发回归为 `.review-tmp/breaker-fixes-final-race.log`。

## 边界与空闲阶段方案

空闲阶段的建议方案见 [BREAKER.md](../../runtime/BREAKER.md)「空闲阶段验证方案」：业务显式声明完整只读语义校验能力、提供有效样本、两次低频验证；仍统一整站评定和排空。不自动重投已完成任务，不因队列为空就认为健康。**本轮仅提供方案，未启用该豁免，默认真实任务门槛保留。**

强杀仍可能丢失未刷写周期，任务终态与快照不是跨表事务；首次形成熔断事件前的窗口没有独立持久化。旧快照缺少精确窗口时保留已知失败下界并转人工，明确报告缺失；不能把有限报告样本当作精确计数。已有事件表无需新增迁移；新快照格式不能由旧二进制直接读取，回退需要升级前备份或转换。
