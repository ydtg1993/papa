# 任务时间与标准身份施工验证

<!-- version-scope -->
> **适用版本/阶段：Papa v4 演进过程中的历史工作区快照（具体基线以正文为准；未记录精确发布版的条目不推定版本号）。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 当前使用方式以 [v4 手册导航](../../README.md) 为准。
<!-- /version-scope -->


日期：2026-11-12。仅修改 Papa 工作区，未提交、未推送、未修改业务数据库。

## 已完成

- `crawler_tasks` 增加延迟毫秒、UTC 最早执行时间、最近 Handler 开始/结束记录及到期索引。排期跨恢复保留，加急不绕过期限。
- 新增 `Task.ResourceID`，标准身份由站点、阶段、ID/URL 类型和值生成。内存、数据库、单条、批量和任务流去重一致；不同 ID 可使用同一 URL。
- 显式迁移为旧表补身份列、回填、检查冲突，完成后删除旧双唯一索引；冲突不合并、不删任务。重复提交同身份沿用持久化 URL。
- 后台时间列、公开 API、迁移/Fetcher/恢复/落库/监控文档与模板同步。
- 熔断设计另存 `2026-11-12-breaker-analysis-design.md`，本次未实施熔断、AutoThrottle 或失败间隔变更。

## 验证结果

- `go test ./... -timeout 180s`：通过（包括默认启用的浏览器测试；未配置的可选集成用例仍按原规则 skip）。
- `go vet ./...`、`go build ./...`：通过。
- 定向 `go test -race ./core ./models ./engine`：身份、UTC 序列化、延迟/恢复、记录失败、取消行为及身份/时间 MySQL 用例通过。
- 真实 MySQL：`TestTaskIdentityMySQLDistinctIDsSameURLAndRestart`、`TestTaskIdentityMySQLExplicitMigration`、`TestTaskIdentityMySQLMigrationRejectsCanonicalConflict`、`TestTaskTimingMySQLPersistenceRecoveryAndAttempts`、`TestTaskTimingMySQLBatchExplicitDeadlineAndNegativeDelay` 通过。使用随机独立测试库，helper 创建并清理；仅复用本地连接配置，不使用其业务库。
- 时区覆盖：MySQL 会话 +08:00、驱动 Asia/Tokyo +09:00；UTC DATETIME 序列化和绝对/相对排期均通过。`TestUTCTaskTimeSerializerIgnoresDriverLocation` 另覆盖驱动附加时区、字符串、字节、空值与 UTC 写入。
- `git diff --check`：通过。

## 本轮审查修复

补齐重复提交的时间字段投影；认领因到期守卫拒绝后重新托管；结束记录失败与普通业务重试分离；取消时区分已成功与未成功 Handler；新时间字段避免数据库/驱动时区漂移；同身份重复提交不产生内存 URL 与持久化 URL 分歧。所有新时间写入只针对已认领、带执行轮次的任务。

## 升级与限制

停止 worker 并备份后执行 `papa migrate`。规范化造成历史身份冲突或历史 URL 非法时迁移停止，保留记录，人工核对后重跑；迁移完成前不启动新版。无生产升级或真实站点采集验收。

每次 Handler 的开始和结束各增加一次数据库取时和一次更新；故障时保留 worker 并背压。数据库取时不可用时，记录时间可能晚于实际 Handler 边界，精确耗时仍看 trace。当前运行不会因时间记录失败重放成功 Handler，但进程崩溃后的任务恢复仍为至少一次执行，业务写入必须幂等。未运行全包 race；执行的是与本次变更相关的定向 race。
