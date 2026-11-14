# 未结算失败修复与复查

<!-- version-scope -->
> **适用版本/阶段：Papa v4 演进过程中的历史工作区快照（具体基线以正文为准；未记录精确发布版的条目不推定版本号）。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 当前使用方式以 [v4 手册导航](../../README.md) 为准。
<!-- /version-scope -->


## 最终行为

- 真实失败先保存到现有 Recovery.PendingFailure，再写任务终态。保留 ID、repeat/reprocess、错误分类/指纹、补偿条件和样本原截止时间。
- handler 未退出、终态未确认、计数检查点未保存时，后续验证都不能放行。恢复/错误/轮询队列和人工重投不能提前替换该轮次；人工强制解除也不能丢弃待结算责任。
- 原报告循环在 handler 退出后重试结算，每分钟最多一轮；sampleDispatching 排斥并发。只重试 SQL，不调用业务。
- processing + repeat/reprocess 条件写与同轮终态查询处理丢回执；错误追加和补偿版本只变化一次。确认后的失败检查点与移除 PendingFailure 在同一快照保存，历史与快照同事务写入。
- 重启先加载待结算结果并拦截 recover 重投。原错误类别和补偿条件不靠错误字符串重新猜测；原截止时间不延长，成功证明不恢复。
- obs 追加等待、重试、失败原因、重启接续和确认过程。统计窗口已过的结果仍记录实际失败确认，不补入当前窗口，不继续显示待落库，更不算验证成功。

## 复查范围

1. 同站点八路同时触发结算，只有一条路径操作 SQL；原 handler 收尾在途时后台不能接手。
2. 持续终态失败、完成时间写入失败、终态提交但回执丢失、控制快照写失败、计数检查点写失败。
3. 待确认时重启、确认后再次重启；恢复队列不重跑业务，不重复计数或追加错误。
4. 数据库轮次改变时保留保护并记录冲突，不覆盖新轮次。
5. 每日追加历史、迟到结果、小时复查和已有串行验证回归。

核心测试：TestBreakerSettlementRestartAndLostAcknowledgementMySQL、TestBreakerSettlementCheckpointOutageAndRoundConflictMySQL、TestBreakerSettlementExpiredResultStillReportsConfirmation。MySQL 测试只创建并清理随机隔离库，借用业务配置中的连接信息，不改业务库。

## 范围与边界

没有新增控制器、后台循环或表列。新增的是现有恢复控制中的一个待确认结果，沿用原锁、串行占用和快照保存屏障；报告投影仍不参与控制。

数据库连待确认快照也无法保存时，只能在运行中保留结果并阻止放行；强杀无法恢复从未持久化的错误原文。正常停止会报告保存失败。跨重启接续以待确认快照已提交为前提。关闭熔断配置也不越过已有待结算结果，重新启用后再结算。

未提交、部署或迁移业务数据库；未执行真实站点故障演练。

## 验证结果

全量 `go test ./...`、`go vet ./...`、`go build ./...`、`node --check admin/server/static/mo.js` 和 `git diff --check` 均通过。带隔离 MySQL 的定向 race 覆盖上述新回归及 BreakerSerial、BreakerHourly、BreakerHistory、BreakerTruncated、AttemptFinishPersistence、BreakerReviewRetry、BreakerReviewManual：engine 18.628s、breakerstore 2.339s、obs/analyze 2.739s，全部通过。

验证日志：`.review-tmp/settlement-full.log`、`.review-tmp/settlement-race.log`。复查未发现本次路径仍会提前放行、重复执行业务、重复追加错误或重启误恢复；持久化前强杀的边界如上。
