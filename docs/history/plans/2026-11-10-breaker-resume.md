# v4 熔断恢复修复

<!-- version-scope -->
> **适用版本/阶段：Papa v4 演进过程中的历史工作区快照（具体基线以正文为准；未记录精确发布版的条目不推定版本号）。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 本文为设计/计划记录，不能仅凭计划步骤或复选框认定已实现或已验收。 当前使用方式以 [v4 手册导航](../../README.md) 为准。
<!-- /version-scope -->


**Goal:** 整站恢复后从零累计终态失败，不被旧窗口计数立即再次熔断。

**Architecture:** 计数器提供 Reset；引擎通过现有 breakerMu 串行化计数/暂停和恢复/清零，锁序保持 breakerMu → pauseMu。只重置真正恢复的站点，单阶段恢复不重置整站计数。告警使用触发时捕获的快照，发送在锁外。终态失败按完成时刻计数，恢复不取消在途任务，不删除失败记录。

**Tech Stack:** Go、现有 fake DB、race detector。

用户已明确要求在当前 v4 工作区实施；不做 Git 写操作，不修改 crawler/v3。

- [x] 在 engine/pause_test.go 增加恢复后重新达到阈值、站点隔离、重复恢复及阶段恢复回归，运行并确认旧实现失败。
- [x] 在 internal/breaker/breaker.go 增加 nil 安全 Reset，保留配置；在 breaker_test.go 验证。
- [x] 在 engine/pause.go 的整站/全部恢复路径持 breakerMu，在唤醒前清零实际恢复站点。
- [x] 在 engine/stage.go 将计数和写暂停信号放在同一个 breakerMu 临界区，捕获不可变告警快照。
- [x] 补并发恢复/失败测试，验证临界点之前/之后的合法结果和告警快照。
- [x] 同步 docs/operations/MONITOR.md；gofmt、定向测试、全量 test/vet/build 和并发 race。

不增加数据库列、配置项或新生产文件；无需迁移和模板变更。诊断报告功能不包含在此次恢复缺陷修复内。

验证结果：旧实现三个恢复回归测试失败；修复后定向测试、go test ./...、go vet ./...、go build ./...、CGO_ENABLED=1 go test -race ./engine ./internal/breaker 均通过。
