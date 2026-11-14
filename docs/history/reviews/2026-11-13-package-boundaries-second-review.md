# 包职责重构二次审查

<!-- version-scope -->
> **适用版本/阶段：Papa v4 演进过程中的历史工作区快照（具体基线以正文为准；未记录精确发布版的条目不推定版本号）。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 当前使用方式以 [v4 手册导航](../../README.md) 为准。
<!-- /version-scope -->


本轮按用户要求复查并直接修复，保留工作区与已有暂存内容，不执行 Git 写操作。范围包括全仓依赖边界、重构新增组件及 App/Engine 接线；重点检查状态所有权、版本原子性、取消排空、公开兼容入口和持久化失败路径。

## 发现与修复

| 问题 | 影响与修复 | 回归证据 |
| --- | --- | --- |
| 运行策略响应在 HTTP 层重新采集策略快照（重构回归） | 服务锁释放后发生另一次保存时，响应的 revision/effective 与 snapshot.Policy 可属于不同版本。服务在发布锁内捕获策略相关快照，HTTP 仅补充独立指标。 | `TestPolicySnapshotStaysWithItsRevision`、`TestPolicyEnrichmentPreservesRevisionBoundSnapshot` |
| 诊断 View 依赖 HTTP 外层登记生命周期（边界遗漏） | 账号池页面直接调用身份 View 时，关停可能报告排空，但存储读取尚未返回。View 自己登记取消与在途计数，关停后拒绝新调用。 | `TestServiceViewCancelsAndDrainsWithoutHTTP`；Go overlay 恢复旧函数后出现 `shutdown completed while view still used storage`，修复后通过。 |
| 策略提交结果未确认时仍按旧缓存删除池（既有事务漏洞） | 数据库已提交新引用、确认丢失且回读失败时，可误删持久策略引用的池。DeletePool 在同锁内先核对持久策略；不可确认则拒删。 | `TestPoolDeletionReconcilesUncertainPolicyBeforeCheckingReferences` |
| 策略结果未确认时仍接受旧版本远程暂停确认（既有版本漏洞） | 旧 revision 能清除 RemotePaused。未确认时返回 Unavailable，刷新核对后按最新版本确认；不改变 Retry-After 截止时间。 | `TestRemoteAcknowledgementRejectsUncertainRevision` |
| 新后台扩展注册接口保留调用方 Owns 切片（状态所有权遗漏） | 调用方注册后修改切片会改变已挂载菜单，且可能与请求并发。注册时复制切片；原 App 路径已有复制，本次补齐新独立接口。 | `TestExtensionMetadataOwnsRegistrationSnapshot`；恢复旧赋值后明确失败，响应包含 `page:changed`。 |
| 报表组件仍依赖整份应用配置（解耦不完整） | Runner 保留 config.Config 指针。改为报表专用值配置，由 App 映射，增加配置快照测试和禁止反向配置依赖的守卫。 | `TestRunnerOwnsConfigurationSnapshot`、`TestPackageResponsibilityBoundaries` |

## 未发现新增回归的关键路径

- scheduler 实际拥有池、延迟堆、锁及唤醒通道；execution 拥有持久化重试和未确认计数；fetchruntime 拥有抓取资源及下载覆盖快照，均无 App/Engine 反向引用。
- taskstore 的认领、终态、释放、重投及 repeat 轮次围栏保持；公开 Task 方法兼容，内部失败确认不会重放业务 handler 或重复加计数。
- attempt 的最终失败意图先于账号反馈与收尾持久化；原有失败结算改动及 crash-cut 回归保留。
- 账号 contract/gormstore 的类型别名、错误身份、表名及 CAS 语义保持；breakerpolicy 与配置类型兼容。
- 熔断协调器仍是 engine 同包组件，Engine 仍协调其锁与暂停/溢出事务，**不是完全独立的熔断子包**。保留该边界以维持跨状态原子性，未将仅移动字段描述为全部封装完成。
- App 的 HTTP 文件仍包含装配、审计与启动编排；领域策略事务和诊断执行由各自服务承担。文件仍较长本身不构成需要继续拆包的证据。

## 验证

全部验证完成，命令均使用 `GOWORK=off`：

- `go test -p 2 ./... -count=1 -json` 通过：2476 个测试/子测试通过，默认环境跳过 79 项；日志 `.review-tmp/review2-full-tests.jsonl`。
- `go test -race -p 2 ./... -count=1 -json` 通过，启用实际 Chromium、隔离 MySQL、FFmpeg 与浏览器临时目录清理，未出现竞争报告；日志 `.review-tmp/review2-full-race.jsonl`。
- race 中两项需要专用 DSN 的 auth/frameworkdata 测试另用随机独占测试库补跑通过，测试库已清理；日志 `.review-tmp/review2-optional-mysql.log`。无业务库迁移。
- 外部环境补齐后，唯一未执行的是 Windows 不支持的 Unix 文件权限用例 `TestWriteKeepsFileMode`。
- `go vet ./...`、`go build -mod=readonly ./...`、Grab 模块兼容构建、gofmt 与 `git diff --check` 全部通过。
- 定向组件测试、实际 Chromium 页面、隔离 MySQL CAS/恢复/失败结算回归通过。未执行真实站点业务验收或部署。

Go overlay 反向验证不修改工作区：加载修复前 runtime 服务和 handler，四个新增回归均失败，分别出现 `snapshot must belong to the returned policy revision`、`HTTP enrichment replaced a revision-bound snapshot`、`deleted pool while durable references were unknown`、`accepted unconfirmed revision`。日志保留在 `.review-tmp/review2-runtime-negative.log`；扩展元数据反向验证见 `.review-tmp/review2-extension-negative.log`。
