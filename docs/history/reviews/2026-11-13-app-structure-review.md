# internal/app 目录与职责复查（2026-11-13）

<!-- version-scope -->
> **适用版本/阶段：Papa v4 演进过程中的历史工作区快照（具体基线以正文为准；未记录精确发布版的条目不推定版本号）。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 当前使用方式以 [v4 手册导航](../../README.md) 为准。
<!-- /version-scope -->


> 后续状态：用户已批准按本审查实施；修复、追加发现与验证结果见 [App 状态所有权与关停收口记录](2026-11-13-app-ownership-implementation.md)。下文保留实施前的审查证据，当前目录以 INTERNAL_STRUCTURE.md 为准。

本轮按“状态所有权、语义接口、依赖方向、取消与排空、测试归属”检查当前 App 及直接相邻组件。只更新文档和建立临时验证材料，没有修改生产代码或正式测试，没有执行 Git 写操作。现有实现不因本文出现建议而视为已修复。

逐文件说明、完整测试归属、当前与建议布局见 [INTERNAL_STRUCTURE.md](../../package_structure/INTERNAL_STRUCTURE.md)。

## 结论

App 已有真实拆分：声明校验/规划、运行策略事务、身份诊断状态和报表执行分别有拥有者，HTTP 与前端大多在对应 admin 功能包。App 持有这些组件、负责创建和启停是合理职责。

剩余重点不是再建几层目录，而是声明可变别名、HTTP 与运行组件的排空结果、App 自建 goroutine 的生命周期，以及仍在装配层的功能展示和审计语义。正确性问题应先于文件合并处理。

## A. 已复现的状态与生命周期缺口

### A1. 站点声明有重复状态与可变别名

位置：`internal/app/app.go` 的 `sites`；`internal/app/spec.go` 的 `RegisterSites` / `Site`；`internal/sitedecl/types.go` 的 `snapshot`；`registry.go` 的 `RegisterSite` / `Sites`；`plan.go` 的 `entryFunc`。

三个相关但不同的交接点：

1. App 原样保存并返回 `core.Site`，Headers map、RestrictedKeywords slice 与输入共享；Engine 的 `siteCatalog` 在写入和查询时会复制字段。修改 `App.Site` 返回值后，App 和原声明改变，Engine 保持登记时值，查询与实际执行配置会分叉。
2. `Sites()` 仅复制最外层 `[]SiteSpec`。修改结果中的 Headers、RestrictedKeywords、Stages 或 Breaker，下一次读取登记表也发生变化，与“调用方改它不会影响登记表”的注释不符。下载覆盖层的嵌套指针也没有隔离。
3. `entryFunc` 捕获浅拷贝 Site。规划后修改原始 Headers 会改变入口回调收到的值；入口回调写 map 又会反向改动原声明。

临时观察型测试均成功复现：

- `TestReviewRegistryNestedAliases`
- `TestReviewAppSiteAliasesDivergeFromEngine`
- `TestReviewEntryCallbackSharesDeclaration`

影响与限制：已验证顺序修改下的别名和分叉；未用 race 复现并发 map 读写。互斥锁只包围登记切片读写，无法保护已经返回给调用方的嵌套引用。没有声称 App 的修改会越过 Engine 的复制边界改变实际执行配置。

建议：实际登记状态以 Engine 的站点目录为唯一来源，保留 App.Site 公开入口并委派；声明登记和入口交付明确冻结/复制范围。框架声明的 map/slice/配置指针需要隔离，业务 Fetcher 实例保留明确的共享约定，不能用反射通用深拷贝处理。

### A2. HTTP 未排空没有进入共享资源关闭条件

位置：`internal/app/http.go:328` 的 `shutdownHTTP`；`internal/app/lifecycle.go:48` 调用点及 `:88` 关库条件。

`http.Server.Shutdown` 超时后调用 `Close`，方法不返回排空结果。真实 HTTP 连接的临时测试确认：客户端被断开后，服务端 handler 仍在执行，App 却已经结束 HTTP 关停步骤。Run 只用 Engine、业务钩子和 runtime 三个结果决定是否关闭共享 DB/浏览器池。

`TestReviewHTTPShutdownDoesNotDrainHandler` 以正确排空行为为预期，当前失败原文：

```text
shutdownHTTP returned while an HTTP handler still owns shared resources; Run receives no not-drained result
```

这也不能仅归为业务扩展不遵守取消：内置白名单保存经过 `admin/access.Handler.Whitelist → SaveWhitelist → configstore.Store.Save`，该窄接口和 SQL 路径没有传递请求 context。

建议：HTTP 生命周期报告是否排空；App 把该结果纳入共享资源关闭条件。强制断开连接与 handler 退出必须区分。无需改变公开 Run/Shutdown API。

证据范围：handler 存活由真实 HTTP 连接动态确认，随后关库判断由源码确认；本轮未跑完整 Run 与真实 MySQL 的关闭竞争，未声称已经复现数据丢失。

### A3. 一个运行组件超时会跳过其它组件的关停

位置：`internal/app/runtime_policy.go:73` 的 `drainRuntime`；onboarding 失败在 `:79` 返回，pools 失败在 `:88` 返回，身份服务关停在两者之后。

身份诊断服务使用独立 Background 生命周期，Run 的 context 取消不能替代它自身的 Shutdown。临时测试用真实 onboarding.Service 和阻塞的合成 Provider 触发超时，随后确认身份操作 context 未取消，服务仍接受新的 Begin。

`TestReviewRuntimeTimeoutStillStopsIdentityAdmission` 当前失败原文：

```text
onboarding timeout prevented cancellation of in-flight identity operations
onboarding timeout left identity admission open after drainRuntime returned
```

建议：App 应确保所有组件都收到停止准入/取消信号，再按本轮排空截止时间汇总结果；一个组件未排空不能阻止其它组件进入关停。需要区分“开始停止”和“等待完成”，不能只把 return false 改成继续无限等待。

证据范围：动态验证 onboarding 失败分支；账号池失败分支的相同遗漏来自源码。`s.wg.Wait()` 不带超时选择也是源码事实，但当前 sampler 使用带 context 的采样；没有实测证据表明它导致超预算，不单列为已复现缺陷。

### A4. 白名单重载既不可取消读，又没有等待登记

位置：`internal/app/http.go:44` 的 `reloadWhitelistLoop`、`:256` 的裸 goroutine 启动；`admin/access/reload.go`；`admin/configstore/string_list_cache.go` 的 StringListReader；`configstore.go` 的 Load。

循环只在 select 处响应取消，进入 LoadStrings 后没有 context。临时测试等待实际 30 秒 ticker，阻塞合成 reader，再取消运行 context，循环仍停留在读操作内；App 也没有等待该循环的退出结果。

`TestReviewWhitelistReloadCancellationDrainsRead` 当前失败原文：

```text
run context was canceled, but whitelist reload remains inside its non-cancelable reader; no App drain accounts for this goroutine
```

建议：可在已有 `admin/access` 内建立重载生命周期，或先用同包明确组件封装。context 应贯穿读口和存储适配器，App 收集 Stop/Wait 结果。仅迁移循环文件不能修复关库竞态。

证据范围：阻塞发生于合成 reader；真实 SQL 没有故障注入。白名单的读失败语义已在 admin/access，本轮没有提出修改该语义或 TTL 默认值。

### A5. 引擎错误日志监听在 App 取消后继续存活

位置：`internal/app/lifecycle.go:109–114`；`engine/lifecycle.go` 的 Errors；`engine/stage_registration.go` 的错误转发。

App 对 Engine.Errors 的常驻通道执行 range，不读取 ctx；Engine.Stop 停止转发器，但没有关闭该通道。临时测试在取消 context 并调用 Engine.Stop 后，goroutine 栈仍阻塞在 `lifecycle.go:111`。

`TestReviewEngineErrorListenerStopsWithApp` 当前失败原文：

```text
engine error listener survived cancellation and Engine.Stop
```

建议：App 自建监听负责取消与等待，无需为了这个循环建立新包，也不要擅自改变 Engine 的公开通道关闭契约。

### A6. 继承全局 breaker 默认值存在校验前置条件缺口

位置：`internal/sitedecl/plan.go` 的 planSite；`internal/app/spec.go` 的 RegisterSites。

显式 SiteSpec.Breaker 会经过校验，继承的默认配置未在同一位置校验。临时测试直接提供 `Enabled=true, Threshold=0` 的 Config：CheckSites 返回 nil，RegisterSites 已登记 App/Engine 站点，然后 SetSiteBreaker panic：

```text
crawler.breaker.enabled=true 时 threshold 必须 > 0，实得 0
```

`TestReviewInvalidInheritedBreakerPartiallyPublishes` 为观察型测试，通过意味着成功复现上述路径。

必须保留的限制：正常 `NewApp → config.Load → config.Validate` 会先拒绝这个全局配置。这是直接调用声明 API 传未校验 Config，或构造后改写公开 Config 的边界问题，不能描述为普通 YAML 启动缺陷。建议校验最终有效配置，不需要先引入通用注册事务。

此外，`TestRegisterSitesAppliesNothingWhenDeclarationIsBad` 和 `TestApplyStagePlanPropagatesAddStageError` 通过；现有 AddStage 的 coverage、worker、queue 错误条件在正常规划中已有对应校验。不能仅凭没有 rollback 就断言普通合法声明会部分落地。

## B. 职责归属和文件组织建议

| 位置 | 源码事实 | 建议与边界 |
| --- | --- | --- |
| `http.go` 的内置 UsePage | page key/默认文案与资源绑定在 App，JS 内又依赖同一 page key | 默认描述符与资源随 admin 功能；App 决定启用、次序和应用级导航覆盖，不能让功能反向依赖 App.Page |
| `data_tables.go:13–14` | tasksource.Table 返回后，App 另写 Label 与 Header HTML | 任务表默认展示契约回到 `admin/tasksource`；App 继续选择挂载的表 |
| `http.go` 的 OnBreakerResume/Pause/ForceResume 等 | 装配层决定功能审计 action、note 和 stage/site 文案分支 | 功能拥有动作语义，同包审计适配负责 operator/time/sink；普通字段转换或缓存失效接线本身可以在 App |
| `runtime_policy.go` / `browser_identity.go` | 生命周期与 handler 构造、API 转发同文件 | 先按运行组件装配与后台绑定分文件，服务算法本来就在外部，不应再造服务壳 |
| `runtime_history.go`、`proxy_health_api.go`、`account_compensation.go`、`rod_diagnostics_view.go` | 每个仅承载私有转发或脚本引用 | 合并到实际路由绑定；`rod_diagnostics.go` 的公开 RodDiagnostics 不能作为冗余入口删除 |
| `accounts_test.go` | `TestAccountsFrontendKeepsConfirmationAndControlledInput` 仅检查脚本文本 | 该用例归 admin 功能；App 的账号管理器一致性、HTTP/浏览器跨组件测试保留 |
| `http.go` 的白名单说明 | 仍用多实例后台解释重载 | 清理旧架构前提；单实例内运维修改持久配置仍可解释重载需要 |

以下不作为缺陷：App 持有 browserdiagnostics.Service；Configure 被多个门面调用；initRuntime 对多个组件接线；App 通过只读回调提供报告输入；21 个生产文件和 25 个测试文件在同一层。诊断 Configure 使用 sync.Once，锁/取消/在途状态在服务内；报表 Runner 也拥有自己的循环与生成锁。

## C. 验证记录

在 Papa 目录、`GOWORK=off` 下运行：

```powershell
$env:GOWORK = 'off'
go test ./internal/app ./internal/sitedecl ./internal/runtimecontrol ./internal/browserdiagnostics ./internal/reportrunner ./admin/access -count=1
go test . -run '^TestPackageResponsibilityBoundaries$' -count=1
```

6 个相关包及架构守卫通过。相关包 JSON 日志统计为 154 个通过的测试/子测试条目、5 个跳过条目。跳过的是未配置浏览器路径的真实 Chromium 用例：

- TestIdentityDiagnosticRealBrowserFirstDocumentAndCleanup
- TestAccountOperationsUIRealBrowserPolicyAndPartialCompensation
- TestAccountsUIRealBrowserDraftLoginIdentityAndSecretFields
- TestAccountsUIRealBrowserNullFieldsAndLateResponseOwnership
- TestRuntimeUIRealBrowserDraftSaveConflictAndNavigation

还执行了四包筛选回归，覆盖业务收尾、策略重启、身份 View/Shutdown、白名单加载与缓存：

```powershell
go test ./internal/app ./internal/browserdiagnostics ./admin/access ./admin/configstore -run 'TestShutdownExtensions|TestRuntimePolicyRestart|TestServiceViewCancelsAndDrainsWithoutHTTP|TestIdentityAPIShutdown|TestLoadWhitelist|TestCachedStringListReader' -count=1
```

以上均通过，说明已存在的正常路径/局部关停覆盖仍有效，不代表 A 节的 App 跨组件失败路径正确。

临时验证没有进入正式测试目录，使用 Go overlay 注入：

```powershell
# 四个观察型测试：预期复现当前缺口，因此通过不代表修复
go test -overlay ../.review-tmp/app-structure-review/decl/overlay.json ./internal/app -run '^TestReview|^TestRegisterSitesAppliesNothingWhenDeclarationIsBad$|^TestApplyStagePlanPropagatesAddStageError$' -count=1 -v

# 四个正确性预期测试：当前均失败，失败含义见 A2–A5
go test -overlay .review-tmp/app-structure-review/lifecycle/overlay.json ./internal/app -run '^TestReview(HTTPShutdown|EngineErrorListener|RuntimeTimeout)' -count=1 -v
go test -overlay .review-tmp/app-structure-review/lifecycle/overlay.json ./internal/app -run '^TestReviewWhitelistReloadCancellationDrainsRead$' -count=1 -v
```

本地证据：声明测试位于工作区根 `.review-tmp/app-structure-review/decl/`；生命周期测试与日志位于 Papa 的 `.review-tmp/app-structure-review/lifecycle/`；包测试 JSON 位于 Papa 的 `.review-tmp/app-structure-review/package-tests.jsonl`。这些是本轮审查材料，不是已合入的正式回归。

本轮没有跑全仓测试、race、真实浏览器或真实 MySQL，也没有声称这些缺口由本次目录重构首次引入。审查结论基于当前实现和合成复现；下一轮实现修复后需把关键复现转为正式行为测试，补充完整 App 关停与相关并发验证。
