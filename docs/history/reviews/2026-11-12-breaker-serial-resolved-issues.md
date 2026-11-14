# 串行验证、10分钟超时及复查修复

<!-- version-scope -->
> **适用版本/阶段：Papa v4 演进过程中的历史工作区快照（具体基线以正文为准；未记录精确发布版的条目不推定版本号）。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 当前使用方式以 [v4 手册导航](../../README.md) 为准。
<!-- /version-scope -->


## 完成的行为

1. 同站点、跨阶段验证一次一个样本。选择和重投先占用统一准入位置，数据库访问期间正常调度不能抢跑；样本证明满足后不会再次重投。同一任务不足三个失败样本时仍按授权轮换为三个独立执行轮次。
2. 选中和截止时间先持久化再执行；最多10分钟，包含排队、资源/账号等待及 handler，整轮预算更短时优先。认领被延迟条件拒绝、资源准备失败不能重置等待期限。超时取消 context，A 未退出不执行 B、不叠加小时复查；退出后可检查剩余样本，但本周期不能恢复。
3. 已确认的迟到终态进入真实统计和历史，成功不能补算健康证明，实际失败仍计入失败数。忽略取消的 handler 最终成功时，限时补写完成时间及终态；存储无法确认则保留原有未结算责任。
4. 重启丢弃旧成功证明，保留已选样本原截止时间。旧格式混合事件恢复三次门槛；可能曾由混合升级的旧硬保护事件保守验证三次。迁移只在读取旧控制入口进行，运行时不使用展示报告驱动恢复。
5. obs 展示超过10000条时明确截止状态未知，不使用截断前缀的最后状态下结论。追加历史、原日期查询、重复保存幂等及控制/历史同事务沿用原实现。超时、退出、迟到结果、中断和升级进入追加历史；计划时间随重启与小时跳过同步。

## 复查与验证

- 真实 MySQL：`TestBreakerSerialRequeueExcludesNormalAdmissionMySQL`、`TestBreakerSerialDelayedClaimCannotRenewDeadlineMySQL`、`TestBreakerSerialDeadlineCancelsRealHandlerButWaitsForExitMySQL`、`TestBreakerSerialDeadlineIncludesResourceWaitMySQL`。
- 并发与重启：`TestBreakerSerialAdmissionIgnoresHigherRampConcurrency`、`TestBreakerSerialUnfinishedTimeoutBlocksNextAndHourlyRetry`、`TestBreakerSerialRestartPreservesDeadlineAndRejectsExpiredSelection`、`TestBreakerSerialUpgradeOldMixedSnapshotKeepsThreeSamples`。
- 超时与报告：`TestBreakerSerialWaitingTimeoutChecksRemainingSamplesWithoutRecovery`、`TestBreakerSerialProbeSelectedWaitHasTenMinuteLimit`、`TestBreakerTruncatedDailyHistoryMustNotInventFinalState`；原历史追加/跨日/事务回滚及丢确认测试继续执行。
- 全量 `go test ./...` 通过；相关包 race（启用隔离 MySQL）通过；后续计划时间及迟到终态调整后，相关定向 race 再次通过。`go vet ./...`、`go build ./...` 通过；Node 行为和真实 Chrome 熔断弹窗检查通过。
- 多阶段历史追加曾反复投影所有阶段、清理整份时间线；改为复用单阶段投影并只清理新条目，显示仍保留128条、未确认历史另存，不新增报告体系。

## 兼容与边界

不新增队列、管理器、数据库表或列；控制字段使用原 JSON 快照。已完成上一版历史表迁移的环境无需再次迁移。`ramp_concurrency` 保留配置兼容，但恢复验证固定串行；正常运行和不同站点并发不变。默认整轮预算仍是5分钟，10分钟是单样本上限。

业务必须响应 context；框架不能强杀忽略取消的 Go handler，此时保持占用和保护。存储中断/进程崩溃的既有未确认业务执行风险仍存在，不宣称业务副作用恰好一次。测试只使用合成任务和随机隔离测试库，没有部署、业务库迁移或真实站点故障演练；没有 Git 写操作。
