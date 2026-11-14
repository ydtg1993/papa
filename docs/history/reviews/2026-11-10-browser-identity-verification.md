# 浏览器身份首期实施与验证记录

<!-- version-scope -->
> **适用版本/阶段：Papa v4 演进过程中的历史工作区快照（具体基线以正文为准；未记录精确发布版的条目不推定版本号）。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 当前使用方式以 [v4 手册导航](../../README.md) 为准。
<!-- /version-scope -->


日期：2026-11-10。基线：Papa `1cbda61`（v4.5.1）。改动仅在工作区，无 Git 索引/历史写操作；未迁移业务库、未访问真实采集站点。使用手册见 [BROWSER_IDENTITY.md](../../accounts/BROWSER_IDENTITY.md)。

## 交付范围

- 原生 Chromium 身份声明、不可变档案版本、运行时解析与页面覆盖保护；可选固定 webdriver v1 脚本，无 Node 部署依赖。
- 两张身份表、内存/GORM Store、独立绑定 CAS、在途作用域、排空复验、取消与重启恢复；保留 Session/Cookie、去重、账号池和单实例架构。
- 原凭据与待发布候选分开保存，未知写入结果按元数据恢复；HTTP/浏览器网站挑战独立分类。
- OA 档案/绑定/本地诊断，账号池关联，鉴权、审计、草稿冲突保护；默认无绑定，不自动启用身份。
- 诊断专用有界启动、精确回环出口、第一段脚本/首个请求/高熵 CH 检查、环境版本摘要；关停取消并限时排空，未结束时保留依赖资源。

## 实施复审发现并修正

| 发现方式与问题 | 修复与回归 |
| --- | --- |
| 代码复审：候选先覆盖凭据 Store，绑定 CAS 失败后会丢旧持久凭据 | `StoredState.PendingIdentity` 保存独立候选；`TestIdentityAccountDurableCandidateSurvivesUncertainPublish` 覆盖提交前失败、已提交但确认丢失、重启和取消 |
| 代码复审：挑战响应的 Set-Cookie 在账号分类之前已作用于共享 CookieJar | 在 HTTP transport 返回前识别挑战，不触发 Jar 更新/重定向；`TestSiteChallengeAccountCannotDeleteExistingSessionCookie`、`TestSiteChallengeCookiesPreserveConcurrentSuccessfulUpdate`、`TestSiteChallengeRedirectStopsBeforeCookiesAndFollow` |
| 代码复审：取消改绑清空已验证的 A，使新 A 操作仍等待旧 A 排空 | 保留匹配的已验证活动快照；`TestIdentityCancelResumesVerifiedActiveWithoutDrain`、`TestIdentityCancelAfterRejectedCandidateRetainsActiveVerification` |
| 代码复审：诊断去敏若丢掉 UA/设备覆盖，会误判生产配置可用 | 只保留受保护身份头与设备，冲突参数明确拒绝；`TestIdentityDiagnosticConfigExcludesCredentialsAndEgress`、`TestIdentityAccountSnapshotAndDiagnosisConfigDoNotAuthenticate` |
| 代码复审：Rod 启动/握手等待可能超出诊断 context | 专用启动器传递取消、关闭连接、终止自有进程并清理目录；`TestIdentityDiagnosticStartupCancellationKillsOwnedProcess` 覆盖不输出 endpoint 和卡住握手；普通池启动方式不变 |
| 代码复审：高熵 CH 不可用仍可能显示通过，环境摘要遗漏检查器/OS 版本 | 单列高熵检查与 unavailable，比较原生字段/完整版本，环境摘要加检查器和 platformVersion；`TestIdentityDiagnosticDoesNotPassMissingOrMismatchedHighEntropy` |
| 代码复审：只取消身份服务便关闭 DB，可能打断复验持久化 | 独立生命周期计数、`Shutdown(ctx)` 与 App 在途 API 排空；`TestIdentityShutdownWaitsForUncooperativeVerifier`、`TestIdentityShutdownDeadlineDoesNotWaitForSubjectMutex`、`TestIdentityAPIShutdownCancelsAndWaitsForInFlightOperation` |
| 数据语义复审：MySQL varchar 尾空白比较与内存键不一致 | 拒绝首尾空白；`TestIdentitySubjectRejectsWhitespaceAliases` |

## 验证

统一设置 `GOWORK=off`；race 使用 `CGO_ENABLED=1`。真实 Chromium 使用本机 Chrome，`PAPA_EGRESS_BROWSER_BIN` 与 `PAPA_TEST_CHROME` 指向其路径。MySQL 测试通过 `PAPA_RUNTIME_TEST_CONFIG` 读取现有连接配置，清除 DBName 后创建随机、自有测试库；不打印 DSN，不对业务表执行 DDL。测试结束清理失败会使测试失败。

| 检查 | 结果 |
| --- | --- |
| `go test ./...` | 初轮及最终完整复跑全部通过；最终日志 `go-test-final.log` |
| `go test -race -p 2 ./...`（真实 Chromium、隔离 MySQL 开启） | 全部通过 |
| 最后关停/初始化/取消修正后的 runtime/account/engine/App 定向 race | 全部通过；日志 `final-lifecycle-race.log` |
| `go vet ./...` / `go build ./...` | 通过 |
| 真实 Chromium 首段脚本、首请求头、两身份隔离、状态恢复、重导航、覆盖冲突 | 通过，含 `TestIdentityFirstScriptHeadersStateAndProtection`、`TestIdentityTwoAccountsSameBrowserRemainIndependent` |
| OA 保存/冲突草稿/改绑/取消/离开保护/窄屏键盘；本地诊断 | 通过，`TestIdentityUIRealBrowserDraftConflictBindingAndNavigation`、`TestIdentityDiagnosticRealBrowserFirstDocumentAndCleanup` |
| 真实 MySQL 两表迁移与并发 CAS | `TestIdentityMySQLMigrationCAS` 通过；12 并发仅一个写入成功，测试库已清理 |
| 任务网站挑战一次执行、不换号、后续错误队列补偿 | `TestSiteChallengeTaskExecutesOnceWithoutAccountSwitch`、`TestSiteChallengeErrorQueueCanCompensateLater` 通过 |
| 新项目生成 → `go mod tidy` → `go build ./...` → `papa check` | 通过，使用本地 replace；没有启动采集或迁移 |
| 更新手册的本地 Markdown 链接、JS 语法、`git diff --check` | 通过 |

脚手架 check 初次遇到 Windows 默认临时目录/缓存权限报错 `Access is denied.`；仅为该验证进程设置工作区内 `GOTMPDIR`、`GOCACHE` 后通过，未更改系统权限或全局 Go 配置。

日志及本地 UI 截图留在 `.codex/verification/browser-identity/`。生成的合成项目、临时 CLI 二进制和专用编译缓存已在验证后清理，未改业务项目依赖。

## 已知边界

- 身份诊断明确不支持 `leakless=true`，不悄悄忽略；不调用目标站认证，不检测网络出口信誉。
- 主文档受管路径有验证；popup、跨进程 iframe、Worker、原始 Rod 修改不在自动保证范围，同目标 frame 需独立验证。
- 字体有限采样，Canvas/WebGL 只诊断；HTTP 头不自动联动，TLS/HTTP2 不模拟，不承诺 Cloudflare/DataDome 通过率。
- 新增身份表需要宿主显式执行迁移；本次仅对隔离测试库迁移。历史档案保留，首期无删除接口。
