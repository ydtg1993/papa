# Papa 包职责重整实施计划

<!-- version-scope -->
> **适用版本/阶段：Papa v4 演进过程中的历史工作区快照（具体基线以正文为准；未记录精确发布版的条目不推定版本号）。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 本文为设计/计划记录，不能仅凭计划步骤或复选框认定已实现或已验收。 当前使用方式以 [v4 手册导航](../../README.md) 为准。
<!-- /version-scope -->


用户已确认全部实施、保留已有修改、不提交。由主助手协调独立编辑范围，逐项验证。

**Goal:** 实现已确认设计的职责边界，同时保留公开 API、路由和状态语义。
**Architecture:** 兼容门面委派给拥有状态的组件；后台依赖窄接口；存储收口语义操作。
**Tech Stack:** Go、GORM、嵌入 JS、现有 Node/Chromium/MySQL 回归。

- [x] 记录当前 diff 与 GOWORK=off go test ./... 基线，保护未提交结算修复。
- [x] A：迁移 accounts、runtimecontrol、diagnostics 的 HTTP 与 static；保留页面 key、路由、脚本顺序和 App 兼容扩展。
- [x] B：提取内部运行策略服务、身份/浏览器诊断和报表生命周期；策略与池删除共享事务边界。
- [x] C：internal/taskstore 收口任务状态持久化；保留轮次、CAS 和终态不确定处理。
- [x] D：engine 按状态拥有者提取调度、执行、熔断和抓取资源组件；保留现有测试与失败结算修复。
- [x] E：提取账号 GORM 适配与熔断配置契约；保留类型别名和构造入口。
- [x] 为新包补 doc.go、依赖守卫和独立组件行为测试，迁移原有测试。
- [x] 同步 DEVELOPMENT/MONITOR 与最终迁移记录，明确与设计的任何差异。
- [x] gofmt；GOWORK=off go test ./...；go vet ./...；go build ./...；并发包 race；可用环境的 Node/Chromium/隔离 MySQL。
- [x] 检查公开 API 与业务模块构建、原始改动保留、git diff --check；报告真实结果。

## 编辑分工

引擎负责人：engine/**、internal/taskstore/**、engine/internal/**，不得改 App。
运行策略负责人：internal/app/runtime*、account_pools_api.go、proxy_health_api.go、account_compensation.go；admin/runtimecontrol、internal/runtimecontrol；与主助手协调 App 字段变更。
诊断/适配负责人：internal/app/browser_identity*、rod_diagnostics*；admin/diagnostics、internal/browserdiagnostics；pkg/account 存储适配及 config/breaker 契约；与主助手协调 App 字段变更。
主助手：app.go、accounts.go、admin/accounts、报表组件、公共 HTTP 辅助、文档及整体验证。

各负责人在实际编辑前读取目标源文件及测试，维护具体迁移与验证记录。不机械创建空目录、不把整个 App/Engine 传入新包、不删除测试以绕过失败。


## 已核对证据

- 重构前 GOWORK=off go test ./... 全部包通过，日志 `.review-tmp/package-boundaries-baseline.log`。
- 保留原有 1628 个不同测试名称；新的组件与接口用例新增，不删除原覆盖。
- 后台四个 JS 迁移前后内容一致（归一化 CRLF 后比较），node --check 通过。
- App/运行策略/诊断及账号 GORM 适配的 scoped tests、race、vet 通过；真实 Chromium 和随机独占 MySQL 测试通过。
- Grab 的 GOWORK=off go build -mod=readonly ./... 通过，未修改业务模块。
- 同包执行/熔断组件的实施细化已写入设计文档；不是尚待创建的空子包。
- 最终验证结果如下；所有改动留在工作区，没有执行 Git 写操作。

## 最终验证结果

均使用 `GOWORK=off`。完整测试使用 `-p 2` 控制并行资源。

| 检查 | 结果与证据 |
| --- | --- |
| `go test -p 2 ./... -count=1 -json` | 通过，2543 个测试/子测试通过；日志 `.review-tmp/package-boundaries-final-tests.jsonl`。随后新增的账号进度回归已通过整包普通测试及最终全量 race。 |
| `go test -race -p 2 ./... -count=1 -json` | 修复下述既有竞争后全量通过，退出码 0；日志 `.review-tmp/package-boundaries-final-race-fixed.jsonl`。启用真实 Chrome、隔离 MySQL、FFmpeg 和浏览器临时目录生命周期用例。 |
| `go vet ./...` / `go build -mod=readonly ./...` | 最终代码均通过，日志分别为 `.review-tmp/package-boundaries-vet.log`、`package-boundaries-build.log`。 |
| Grab 兼容构建 | `go build -mod=readonly ./...` 通过，未修改业务模块。 |
| 页面资源 | 四份 JS 内容迁移前后一致，`node --check` 通过；合成 Chromium 页面回归通过。 |
| MySQL 专用验收 | auth / frameworkdata 两项需专用 DSN 的用例额外在独占随机数据库中以 race 执行通过；日志 `.review-tmp/package-boundaries-optional-mysql.log`。仅迁移测试库、清理测试库，未迁移业务库。 |
| 格式与差异 | 180 个新增/修改 Go 文件全部 gofmt；`git diff --check` 通过。 |

全量 race 中两项专用 DSN 用例由上述独立验收补齐；唯一未执行的是 Windows 不支持的 Unix 文件权限测试 `TestWriteKeepsFileMode`。无真实站点业务验收或部署。原有熔断失败结算改动及其回归保留并通过验证。

## 全量验证发现的既有竞争

首次全量 race 在 `TestFailedUpdateAndIdentityMismatchPreserveOldCredentials/identity` 报出 `WARNING: DATA RACE`：`onboarding.Service.Start` 启动 worker 后直接返回 `p.view`，与 worker 的失败进度更新竞争。该文件此前未参与结构重构。

已改为启动 worker 前复制初始进度，保证返回 running/revision=1 的独立快照。新增 `TestStartReturnsInitialSnapshotWhenProviderFailsImmediately`，每轮执行 32 次立即失败；连同原失败用例执行 `go test -race ... -count=50` 通过。原失败日志保留在 `.review-tmp/package-boundaries-final-race.jsonl`，修复后的全量日志单独保存，避免覆盖发现证据。
