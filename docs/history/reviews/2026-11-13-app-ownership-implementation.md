# App 状态所有权与关停收口记录（2026-11-13）

<!-- version-scope -->
> **适用版本/阶段：Papa v4 演进过程中的历史工作区快照（具体基线以正文为准；未记录精确发布版的条目不推定版本号）。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 当前使用方式以 [v4 手册导航](../../README.md) 为准。
<!-- /version-scope -->


按用户批准的[审查结果](2026-11-13-app-structure-review.md)实施。当前逐文件职责、依赖方向、状态拥有者和测试位置见 [Internal 分册中的 App 职责](../../package_structure/INTERNAL_STRUCTURE.md)。本记录说明实际修复与验证，不用目录移动代替解耦。

## 1. 已完成的职责收口

| 范围 | 实际改变 | 保持的契约 |
| --- | --- | --- |
| 站点声明 | 删除 App.sites；App.Site 委派 Engine.Site。sitedecl 登记、读取、计划、每次入口回调分别复制框架配置；Overlay.Clone 由下载器类型所属包维护 | Fetcher 对象继续共享；公开声明类型和入口保留；整批有效 breaker 配置（含继承默认）校验后才登记 |
| HTTP 生命周期 | App 私有 requestDrain 记录 handler 的实际返回，包括 hijack；shutdownHTTP 返回结果，连接强关不能代表排空 | 不包装 ResponseWriter；既有可选接口、路由、鉴权保持；HTTP 请求和白名单重载共用 HTTP 关停预算 |
| 运行组件生命周期 | 所有运行组件先收到停止信号，再共享截止时间收集结果；采样、日志消费者都有 cancel/done；Run 初始化失败也走统一收尾 | 各组件拥有准入/状态/等待；一个组件超时不会阻止其它组件停止；不关闭 Engine 的公开错误通道 |
| 资料/账号池锁边界 | onboarding 使用短生命周期锁管理准入和计数，资料事务仍保留原锁；账号池唯一等待流程把 Manager 摘除也计入 drained | 不拆开候选发布事务，不改变 Manager 锁序；超时保留依赖，后续 Shutdown 可继续等待 |
| 白名单读取 | access.WhitelistReloader 拥有循环、取消和等待；configstore 的 context 读口传入 SQL，缓存用失效代次拒绝在途旧读回填 | 旧 Load/StringListReader 方法保留；普通读错仍记日志并放宽；取消不发布空白名单；无法取消的旧读端必须真实返回才算排空 |
| 页面和审计 | admin 功能包拥有 Page 描述和动作审计语义；tasksource.WorkspaceTable 拥有工作台展示；App 只选择、注入、挂载 | 公开 Table 默认值不变；页面 key、导航和资源顺序不变；server.TaskFlowScript/CSS 的公开覆盖仍生效 |
| 冗余转发 | admin_routes.go 直接绑定功能 handler，删除 5 个私有逐端点转发文件；纯前端断言归功能包 | 公开 RodDiagnostics 工厂、App 扩展接口继续保留；App 保留跨组件 HTTP/浏览器集成测试 |

App 主动关闭共享 DB/浏览器的条件为 HTTP（含重载）、业务钩子、Engine、运行组件、日志消费者全部排空。未排空时继续其它清理，但不提前关闭这些共享依赖。HTTP 和运行组件各自共用其阶段预算，整个 Run 的收尾不宣称只有一个总时限。

## 2. 实施与复核中追加修复

### 2.1 短生命周期门不能等待长事务锁

初版停止逻辑仍可能在 BeginShutdown 或摘除协调器时等待资料/候选持久化锁，使 context 超时失效。复核后将准入计数与资料事务锁分离，并把账号池协调器摘除放进唯一完成流程。Start 在 workflow 取得 WaitGroup 计数前保留入口计数，避免 Add/Wait 交错；既有资料事务和候选发布原子性保持。

回归：`TestShutdownDeadlineIncludesProfileOperations`、`TestPoolsShutdownDeadlineIncludesCoordinatorDetach`、`TestRuntimeDrainProfileTransactionCannotBlockOtherStops`、`TestShutdownRunRetainsDatabaseForPendingRuntimeStorage`。测试用真实阻塞回调验证停止能返回、其他组件收到取消、未排空时数据库仍可使用，不以 sleep 后猜测 goroutine 已退出。

### 2.2 公开资源覆盖兼容

页面描述迁到功能包后，直接读取 taskflow 默认资源会绕过公开的 server.TaskFlowScript/CSS 变量。App 的装配点继续应用这两个公开覆盖值；`TestBuiltinTaskFlowPreservesPublicResourceOverrides` 验证实际输出的脚本和样式。

### 2.3 MySQL 回归的时间依赖

首轮全仓测试在 `TestBreakerSettlementRestartAndLostAcknowledgementMySQL` 出现原报错：`unsettled failure released admission`。其 50ms 样本期限在认领 SQL 完成前已到期，FetchHandler 没有进入，无法形成测试本来要验证的业务失败结算。使用受控认领阻塞复现后，测试改为 FetchHandler 进入后确定性取消，保留原始业务错误、调用次数、跨重启、待结算和准入断言；自然超时路径仍由既有专门用例验证。

只调整测试时序，不修改该结算生产逻辑。该测试普通与 race 各运行 count=3，共 18 个隔离 MySQL 子用例通过。

### 2.4 Windows 诊断 profile 的文件句柄释放

最终全量过程中，`TestIdentityDiagnosticRealBrowserFirstDocumentAndCleanup` 的 17 项身份检查全部通过，但清理报错：

```text
诊断临时目录清理失败：unlinkat C:\Users\pcuser\AppData\Local\Temp\papa-identity-diagnostic-1244012814\browser-diagnostic-2029029491\Default\Cache\Cache_Data\data_0: Access is denied.
```

诊断进程退出与 Chromium 子进程缓存句柄释放不是同一个完成点。修复放在资源拥有者 pkg/browser：终止进程、等待、删除自有 profile 共用 5 秒预算；Windows 仅重试 AccessDenied/SharingViolation/LockViolation/DirNotEmpty，截止时保留最后的系统错误。诊断专用 CloseDiagnostic 返回清理结果，internal/browserdiagnostics 显式汇总错误；重复关闭幂等，普通 Browser.Close 接口和浏览器池锁序不变。

回归使用真实 Windows 文件句柄：`TestDiagnosticCleanupWaitsForWindowsFileHandleRelease`、`TestDiagnosticCleanupDeadlineRetainsWindowsError`；并验证 `TestCloseDiagnosticReturnsPoolSlotExactlyOnce`、`TestCloseDiagnosticRejectsOrdinaryBrowserWithoutClosingIt`、启动取消、真实 Chrome 首文档与临时目录清理。相关 Chrome 用例连续 count=5 通过，最终全量及补充 race 也通过。

### 2.5 目录说明归档后的分发

沿用工作区的 `docs/package_structure/` 归档位置，修正上层入口、分册和历史审查的相对链接。原脚手架只遍历顶层，迁移后会漏分发目录分册，因此 embed 显式包含该目录，new/upgrade 共用嵌入清单并保留相对路径。仍不分发 issues/superpowers，不删除业务文档；missing-only 保留已存在内容，写入拒绝链接路径。

`TestScaffoldAndUpgradePreserveStructureManuals` 验证新建、missing-only、旧 docs 布局与 docs/framework 布局升级、导航链接和重复升级无漂移。此处只同步框架文档分发，不修改业务模板或运行逻辑。

## 3. 关键回归覆盖

| 约束 | 代表性测试 |
| --- | --- |
| 框架声明副本与注册原子性 | TestRegistryOwnsNestedDeclarations；TestPlanSitesOwnsDeclarationAndDownloadSnapshots；TestEntryCallbacksReceiveFreshIndependentSnapshots；TestAppSiteUsesEngineOwnedSnapshots；TestRegisterSitesRejectsInheritedBreakerBeforePublishingBatch |
| HTTP 实际排空与共享资源 | TestHTTPShutdownReportsLiveHandlerAfterConnectionClose；TestHTTPShutdownWaitsForHijackedHandlerAndClosesAdmission；TestShutdownRunRetainsDatabaseUntilAllUsersDrain |
| 取消先广播、统一期限与失败清理 | TestRuntimeDrainSignalsAllComponentsBeforeWaiting；TestRuntimeDrainTimeoutStillStopsIdentitiesAndCanBeRetried；TestRuntimeDrainBoundsSamplerWait；TestRunInitializationFailureStopsAcquiredRuntime |
| 日志消费者所有权 | TestMessageListenersStopWithAppContext；TestMessageListenersWaitForInFlightMiddlewareLog |
| 重载和 SQL 取消 | TestWhitelistReloaderReportsUndrainedReadAndDiscardsLateResult；TestWhitelistReloaderWaitsForInflightPublication；TestStoreAndCachedStringListReaderPassContextToSQL；TestCachedStringListReaderInvalidationSupersedesInflightRead |
| 页面和路由兼容 | TestBuiltinPagesPreserveKeysMetadataAndResourceOrder；TestAllBuiltinAPIsRemainBehindHostGuard；TestBuiltinTaskFlowPreservesPublicResourceOverrides；功能包的任务表/审计测试 |

依赖守卫限制 App 的独立前端资源及越界依赖，并明确允许 sitedecl 依赖既有 Engine 注册协议类型。守卫不以文件数衡量架构，也不代替并发、SQL 和浏览器行为验证。

## 4. 验证结果与边界

所有 Go 命令使用 GOWORK=off。隔离 MySQL 用测试助手从本地配置读取连接设置，只操作它新建的测试库，没有迁移业务数据库。Chrome 使用本机安装，页面使用合成 fixture；没有执行真实站点验收或部署。

| 批次 | 结果 |
| --- | --- |
| 最终 `go test ./... -count=1 -json`，启用 Chrome 与隔离 MySQL | 76 个有测试的包通过，2817 个测试/子测试通过，6 项按环境跳过，0 失败 |
| 全仓 `CGO_ENABLED=1 go test -race ./... -count=1 -json` | 76 个有测试的包通过，2789 个测试/子测试通过，27 项跳过，0 失败/数据竞争；该批次未启用 Chrome |
| 全仓 race 后追加的诊断清理和 App 用例 | 启用 Chrome，针对 pkg/browser、internal/browserdiagnostics、internal/app 补跑 race，27 个测试/子测试通过，0 跳过/失败 |
| 脚手架文档分发的后续修改 | cmd/papa 全包普通测试与最终 race 均通过（28.058s / 28.264s） |
| 静态与构建 | 最终全仓 vet/build、gofmt、diff --check 通过；新增诊断清理代码另通过 Linux 两包交叉构建；目录计数、文件索引与文档相对链接核对通过 |

全量普通测试的 6 项跳过为：两个未配置专用连接变量的 auth/frameworkdata MySQL 用例、Windows 下的 Unix 文件权限用例、两个未配置 FFmpeg 的用例、另需独立浏览器变量的 TestLivePoolTempDirCleanup。全仓 race 的其余跳过主要是未启用 Chrome；这些功能已在普通全量和定向浏览器批次验证。跳过不计入通过，也不宣称本次验收了所有外部环境。

全量原始结果位于本地 `.review-tmp/app-structure-review/`：`full-tests-complete.jsonl`、`full-race.jsonl`、`final-affected-race.jsonl`。此前两轮失败记录也保留，未覆盖成通过结果。最后的目录分册归档配套改动在全量之后单独验证 cmd/papa；不将这部分说成已经包含在先前的全量 race 中。

保留所有既有工作区改动；本轮只编辑工作区文件，没有 Git 索引或历史写操作，没有提交。
