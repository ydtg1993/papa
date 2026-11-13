# Pkg 目录结构与职责细则

> 面向：复用 Papa 通用能力、增加抓取能力和审查资源所有权的开发者。
> 配套：[目录说明书总览](README.md)、[总体架构规则](../ARCHITECTURE.md)、[开发手册](../DEVELOPMENT.md)。
> 本文按当前源码说明整个 `pkg/`，覆盖 24 个 Go 包、113 个生产 `.go` 文件；测试按职责归类，目录树省略测试文件。文件清单与职责核对不代表浏览器、MySQL 或其他测试已执行。

## 1. 总体定位

`pkg/` 提供业务模块和框架都能复用的能力：账号与身份、浏览器和 HTTP、发现与校验、预算与出口、下载与代理、事件与诊断输出、临时资源及显式检查。它不接管 App 的组件装配，也不接管 Engine 的任务认领、终态、补偿和队列编排。

`pkg` 不是“所有工具都能互相依赖”的大包，也不意味着所有子包都是无状态叶子。账号、浏览器、预算、下载器各自拥有状态；适配器通过明确契约连接数据库、文件和网络。真实站点的 Provider、选择器、采集目标、业务模型和验收预期保留在独立业务模块。

| 规则 | 在本目录中的具体含义 |
| --- | --- |
| 先选状态拥有者，再选文件 | 登录轮次属于 `account`；登录过程属于 `account/onboarding`；浏览器进程属于 `browser`；身份绑定属于 `browser/identity/runtime`，不能因为调用发生在 App 中就把状态放回 App |
| 保留语义边界 | `Acquire` / `ReportFailure` / `PublishCandidate` / `Shutdown` 等入口表达操作，不导出锁和 map 让调用方拼事务 |
| 配置值和运行状态分开 | `breakerpolicy`、`account/contract` 是值与契约；`governance.Controller`、`account.Pools` 才拥有运行状态 |
| 显式区分凭据与诊断 | Session、PageState、Credentials 和捕获响应可能含秘密；公开快照、健康事件和导出样本各有数据边界，不能直接互换 |
| 生命周期随资源所有权走 | 创建后台循环或借用资源的组件负责取消、归还和排空；仅发出取消不代表回调、存储或页面已经停止使用依赖 |
| 单实例内协调 | 账号许可、路径锁、站点预算和身份作用域服务于一个 Papa 进程；优化围绕其并发、背压、恢复和资源预算 |

任务编排见 [Engine 分册](ENGINE_STRUCTURE.md)，宿主装配见 [Internal 分册](INTERNAL_STRUCTURE.md)，HTTP 与页面见 [Admin 分册](ADMIN_STRUCTURE.md)，离线观测分析见 [Obs 分册](OBS_STRUCTURE.md)，模型与核心值见 [根目录分册](ROOT_STRUCTURE.md)。

## 2. 目录全览

```text
pkg/
├── account/                  # 已登记账号认证、池协调、健康和用量
│   ├── contract/             # 无凭据持久化值与 CAS 契约
│   ├── gormstore/            # 账号池/管理状态的 GORM 适配
│   └── onboarding/           # Provider 登录过程、候选确认、受控远程浏览器
├── breakerpolicy/            # 熔断策略值、默认值和校验
├── browser/                  # Rod 进程池、受管页面、状态和网络观察
│   └── identity/             # 身份声明、原生 CDP 配置和实际观察
│       └── runtime/          # 身份持久化、绑定、验证和使用作用域
├── discovery/                # 链接/XML/robots 解析与有界分页判据
├── egress/                   # 目标地址策略、固定 IP 传输和浏览器网关
├── events/                   # 版本化事件、有界异步写入与 JSONL sink
├── extract/                  # 已解析记录的声明式校验
├── governance/               # 站点请求预算、进程资源许可与采样
├── htmlfetch/                # 有界 HTTP、HTML/JSON、会话与离线样本
├── loggers/                  # 分通道日志、轮转及诊断副本脱敏
├── middleware/              # 下载输出路径和共享路径锁契约
│   ├── filedown/             # 普通文件下载、分块与断点
│   ├── m3u8/                 # 播放列表、片段、密钥、合并与断点
│   └── proxy/                # 代理列表、健康、选择和传输反馈
├── notify/                   # Webhook 告警传输
├── redact/                   # 诊断副本的通用已知凭据脱敏
├── temporary/               # 宿主所有的 UUID 临时目录与恢复清理
└── testdb/                   # 独占创建的隔离测试数据库生命周期
```

`pkg/` 自身没有 Go 源文件；`middleware/`、`account/`、`browser/` 既是目录也是独立 Go 包，不能只统计叶子目录。下面各节文件名均相对于该节包目录。

## 3. 账号领域

使用契约见 [账号运行时](../ACCOUNT_RUNTIME.md)、[账号接入](../ACCOUNT_ONBOARDING.md) 和 [浏览器身份](../BROWSER_IDENTITY.md)。四个包分别承担认证协调、纯持久化契约、具体数据库适配和登录过程，不按“都与账号有关”合并成一个服务。

### 3.1 account：认证与账号池

| 文件或文件组 | 职责与公开入口 |
| --- | --- |
| `types.go` | `Key` 共享契约别名、`Credentials` / `Config` / `BrowserConfig` / `Status` / `Failure`；定义 `Manager`、账号 `entry`、许可及认证轮次，`NewManager` 构造注册表 |
| `manager.go` | 登记和 origin/header 校验、HTTP 认证轮次合并、检查与登录、版本发布、`Invalidate` / `Pause` / `Resume` / `Status`；`Do` / `FetchHTML` 执行账号请求 |
| `browser.go` | 账号浏览器许可、隔离页面、登录后 Cookie 与 token 桥接、浏览器状态导出及 `WithBrowser` |
| `store.go` | 凭据 `Store` / `StoredState`、加载保存、`Logout`；`FileStore` 使用调用方密钥加密文件，处理凭据与页面状态副本 |
| `candidate.go` | 分离候选凭据、检查和持久化；`RegisterCandidate` / `PublishCandidate` 发布成功验证的候选，`CandidatePublished` 通知池健康恢复 |
| `identity.go` | 认证状态的身份元数据、`SetIdentityManager` / `PinIdentity`、实际池上的 `ResolveIdentity`、分离候选的 `VerifyIdentity` 与无凭据诊断配置 |
| `pool_types.go` | 池共享契约别名、`PoolOptions`、只读快照、任务 `Attempt` 收据及上下文入口；定义池内部状态、写入门和 `Pools` |
| `pools.go` | `NewPools`、成员/当前账号管理、CAS 控制、`Acquire` / `ValidateAttempt` / `ReportFailure`；协调扫描、合并验证、切换、持久化重试及关停 |
| `health.go`、`health_verify.go` | 健康政策、控制版本、限长事件、`Readiness` / `CheckExisting` / `SetPolicy`；只检查或恢复模式、精确代次验证结算和备用账号预检 |
| `usage.go` | 同一账号跨池共享的操作用量、等待原因和登录间隔；`ReserveLogin` 为外部登录过程预留用量 |
| `pool_request.go`、`pool_capture.go` | 以任务 `Attempt` 固定身份的 `DoPool` / `FetchPoolHTML` / `WithPoolBrowser` / `NavigatePoolPage` 及请求/响应捕获；不在一次任务内任意换号 |
| `pool_references.go` | `ProtectReferences` 将任务引用新增与池删除串行协调；`GetIncludingDeleted` 提供显式重建所需墓碑版本 |
| `pool_store.go` | `MemoryPoolStore` 提供内存 CAS 与独立快照；GORM 模型及构造入口归 `account/gormstore` |

| 状态或资源 | 拥有者与约束 |
| --- | --- |
| 账号登记与关联服务 | `Manager.mu` 保护 `entries`、池和身份服务引用；Manager 不创建浏览器进程，不启动后台循环 |
| 账号凭据、认证轮次与版本 | 每个 `entry.mu` 保护当前凭据、HTTP/浏览器代次、暂停、检查时间及失败状态；账号并发许可由 `slots` 持有，浏览器操作另由 `browserSlot` 串行化 |
| 凭据存储调用 | 账号锁串行保护 Store 调用；Store、认证回调不得重入同账号的协调路径。凭据回调拿到的是本轮候选，返回后不能保留并继续修改 |
| 池选择与共享管理状态 | `Pools.mu` 拥有池 map、账号管理状态、选择版本、待验证与通知状态；锁序为 `Pools.mu` → `entry.mu`，网络和 Store 调用不在 `Pools.mu` 内执行 |
| 池管理写入与引用保护 | 可取消的 `writes` 门串行化需要跨读写完成的管理操作和引用检查；业务/Engine 通过引用回调与 `ProtectReferences` 协作，账号包不查询任务表 |
| 后台验证 | `Pools` 拥有取消上下文、扫描循环、验证容量、worker 等待及摘除完成信号；监听器通过有界队列接收无凭据事件，不能用慢回调阻塞调度 |
| 文件凭据 | `FileStore.mu` 保护该实例的文件操作；加密密钥、目录权限和目录独占由宿主负责，不能把明文 Session 或 PageState 当日志写出 |

`Attempt` 是带选择/管理/凭据版本的身份收据，不是长期占用账号的许可；每次实际交换仍取得 Manager 许可。`NeedsVerify`、用量暂停、固定代理不可用和人工验证分别进入既有健康判定，不通过新建第三份“可用”状态覆盖认证状态。池管理和凭据存储分开，持久池状态不含登录秘密。加载管理状态要求非零 `Generation` 和 `LastControlVersion`，控制版本不能超过当前 `Version`；缺失字段不会从旧版本回填。

`NewPools` 读取持久状态、附着到 Manager 后启动协调器。`BeginShutdown` 停止准入并取消验证；`Shutdown(ctx)` 的完成还包含 worker 结束及从 Manager 摘除。超时后调用方须保留浏览器和 Store，后续可继续等待。`Close` 使用无超时等待，不能拿它代替宿主的有界关停策略。

测试归属：`manager_test.go`、`store_test.go`、`candidate_test.go` 验证认证与存储；`browser_test.go`、`browser_concurrency_test.go` 验证浏览器账号隔离及串行；`pools_test.go`、`pool_store_test.go`、`pool_references_test.go`、`health_test.go`、`control_version_test.go` 验证池、用量及版本；`pools_shutdown_test.go` 验证包括 Manager 摘除在内的关停期限；`identity_test.go`、`identity_challenge_test.go` 验证身份与认证交界。

### 3.2 account/contract：无凭据值契约

| 文件 | 职责 |
| --- | --- |
| `doc.go` | 声明纯值与 CAS 契约的依赖边界 |
| `types.go` | `Key` / `PoolKey` / `Transport`、池配置和选择历史、账号管理政策/用量/事件、`PoolStore` 与冲突错误 |

此包只依赖标准库，没有锁、后台循环、凭据、GORM 或协调器。值的运行所有权属于调用方；进入协调器或 Store 时由相应实现建立副本及版本规则。新增跨“账号协调—池存储”共用的无凭据值放这里；HTTP、登录算法和数据库模型不放这里。当前没有同目录测试，契约实现由 `account/gormstore/contract_test.go` 和根目录架构守卫覆盖相应边界。

### 3.3 account/gormstore：池状态存储适配

| 文件 | 职责 |
| --- | --- |
| `doc.go` | 声明只依赖 `account/contract` 与 GORM，不反向依赖账号协调器 |
| `pool_store.go` | `PoolRecord` / `ManagementRecord`、显式迁移模型 `Models`、`NewGORMPoolStore`、加载及 `SavePoolCAS` / `SaveManagementCAS`，将数据库重复键/版本冲突映射为契约错误 |

Store 持有宿主注入的 `*gorm.DB`，不拥有连接关闭、不启动循环、不自动建表。持久版本竞争由条件写入完成，不复制 Pools 的选择或健康规则。宿主直接导入 `account/gormstore` 使用模型、迁移清单和构造函数；`account` 不转发这些适配器入口。适配器只依赖 contract，不反向依赖协调器。

`contract_test.go` 检查 Store 实现契约；`mysql_test.go` 负责真实 MySQL CAS。内存 Store 测试不能代替数据库语义，数据库用例的环境要求见第 10 节。

### 3.4 account/onboarding：登录过程

| 文件 | 职责 |
| --- | --- |
| `types.go` | `Provider` / `Flow`、方法与输入字段、无凭据 `Profile` / `Progress` / `Identity`、资料 `Store` 和服务 `Options` |
| `service.go` | `New`、Provider 登记、草稿/恢复、`Start` / `Get` / `Act`、候选检查发布、加密秘密引用和有界健康事件投递；将通过验证的结果交给 account |
| `lifecycle.go` | 准入计数、取消关联、`BeginShutdown` / `Shutdown` / `Close`；生命周期锁不跨资料事务、外部 Provider 或 Store 调用 |
| `remote.go` | `RemoteOptions`、`NewRemote`、受控页面和截图/输入协议；借用进程并创建隔离上下文，验证操作者、过程、页面和允许 origin；可信 Provider 才能使用原始页面回调 |
| `store.go` | 无凭据 `ProfileRecord`、显式 `Models`、GORM 与内存资料 Store、CAS 和写入结果核对；不承担凭据加密 |

`Service.mu` 拥有 Provider、过程目录、账号所有权及资料事务；`process.mu` 拥有单次过程的进度、版本、命令、Flow、远程资源及暂存引用；`lifecycleMu` 仅保护准入与 `WaitGroup.Add`。`active` 占位持续到秘密和浏览器清理结束，不能在公开进度先变为结束时就允许第二次同账号登录。

`New` 启动事件投递 worker；登录过程由服务上下文和 TTL 管理，远程会话拥有自己的取消、页面集合及关闭等待。Provider 的 `Start` / `Next` / `Verify` 须响应 context；`Config` 提供运行认证配置，不执行登录。`SaveDraft` 不向 Manager 登记账号，`Load` 只登记已验证资料，不执行 DDL。资料只保存不透明秘密引用，`Options.Secrets` 必须由宿主提供加密存储。

`Shutdown(ctx)` 等待已准入操作、过程及清理，也等待正在执行的事件回调；超时不应被解释为资源可以关闭。事件队列容量为 64，溢出丢弃，取消时 worker 可以直接退出，不承诺投递完队列中的通知。`Remote.Done()` 返回取消信号，不是隔离上下文已销毁、进程已归还的完成信号；`Remote.Close()` 才取消并等待内部结束，且没有调用方期限参数。数据库连接和浏览器池仍由宿主所有。HTTP/前端分别在 `admin/accounts`、`admin/accountpools`；真实站点协议及 Provider 实现在业务模块。测试在 `service_test.go`、`store_test.go`、`remote_test.go`、`shutdown_test.go`，最后一个文件专门验证被资料操作阻塞时关停仍受调用方期限约束。

## 4. 浏览器与身份

### 4.1 browser：进程池与受管页面

| 文件或文件组 | 职责与公开入口 |
| --- | --- |
| `pool.go` | `PoolConfig`、二进制来源解析、`NewPool`、代理/直连容量、懒创建、Get/Put/With 借用、空闲回收、默认头热更和 Close |
| `rod.go` | `Browser`、单实例关闭与使用计量；`PageOptions`、受管页面初始化、`NewPageWithOptions` / `WithPageOptions` / `WithNavigatedPage` / `FetchOnce` |
| `stats.go` | 活实例登记、空闲/保留/借用状态、`Stats` 独立生命周期快照；统计受管实例，不统计 Chromium 子进程数量 |
| `temp_dir.go` | `ResolveTempDir` / `PrepareTempDir` / `NewTempProfile`，验证实际可写的配置根和创建独占 profile；`TempDir` 返回池实际使用的共享根；Pool 不删除该根，只由实例关闭清理独占 profile |
| `navigation.go`、`wait.go` | 已有受管页的 `ConfigurePage` / `NavigatePage`、等待条件及 `WaitError`；页面创建时确定的拦截规则不在导航时随意替换 |
| `block.go` | `BlockOptions` 与单页面资源拦截 |
| `isolation.go` | `WithIsolatedBrowser` / `WithDirectIsolatedBrowser`，在借入的进程中创建独立浏览器上下文并负责回收；不继承共享凭据 |
| `state.go`、`state_navigation.go` | `PageState` 校验、导出、恢复和加密读写；`WithStatePage` 在首次请求/站点脚本前恢复 Cookie 与同源 LocalStorage，并管理初始化监听的生命周期 |
| `session.go` | `CopyCookiesToSession`，从受管页面将指定 URL 适用的 Cookie 桥接到 HTTP Session |
| `capture.go`、`response.go` | 有界请求/响应捕获、已有页适配、失败现场和取消处理；返回独立快照，不直接保存凭据或归档业务数据 |
| `record.go` | `RecordNetwork` 记录当前 page target 的有界网络观察；`NetworkEntry.WriteSample` 通过 htmlfetch 样本策略导出 |
| `challenge.go` | 独立 Network 事件观察明确的站点挑战协议证据；不再安装一套与其他功能争用的 Fetch handler |
| `governance.go` | 浏览器资源许可和受管页面请求预算；页面自己的拦截、反馈、策略变更观察和关闭清理 |
| `egress.go`、`proxy_health.go` | 固定安全启动参数和导航 scheme 校验；冻结实例出口、固定代理准入/反馈、借用代次及不含代理地址的失败信息 |
| `identity.go` | 页面身份登记、冲突校验、首次导航前应用、`ResolveIdentity` / `ManagedIdentity` / `ObserveIdentity`；生成无凭据诊断配置 |
| `diagnostic_startup.go` | `NewDiagnosticPool`、受限二进制获取/进程启动/CDP 连接、诊断进程所有权和 `CloseDiagnostic`；复用普通受管页初始化 |
| `diagnostic_startup_unix.go`、`diagnostic_startup_windows.go` | 平台进程配置、进程树停止及允许重试的清理错误判定 |
| `diagnostic_cleanup.go` | 清理本次创建的诊断 profile；Windows 句柄释放窗口的重试共享原关停期限，不另开无限等待 |

| 状态或资源 | 拥有者与生命周期 |
| --- | --- |
| 空闲实例和借用容量 | `Pool.mu` / `cond` 保护通道收发与关闭；原子存活计数覆盖借出实例；`statsMu` 单独保护实例状态目录 |
| 热更值和回收循环 | headers 使用原子副本发布，maxIdle 使用原子时长；`reaperMu` 管理回收协程的停止通道。池大小是固定容量上限，不是常驻实例数 |
| 进程、网关与借用许可 | 普通 `Browser` 拥有 launcher、网关、关闭标记及本次借用的资源归还函数；`Close` 幂等并对 launcher 收尾等待设上限 |
| 隔离上下文 | 隔离 Browser 借用父进程及其网关，只销毁自己的上下文；外层 With 完成后归还父实例 |
| 页面身份登记 | 包内 `identityPages` 使用 `sync.Map`，键为 target/session 值；单页 `identityPageState.mu` 保护快照、adapter 和头；浏览器事件在 target 关闭/分离时移除登记 |
| 页面网络预算 | `pageGovernance` 自有锁、在途交换与策略观察；页面生命周期结束时释放请求资源，不能将其状态放在全局页面 map 或宿主 App |
| 捕获与记录 | 每次操作自己的 channel、锁、监听和取消；回调结束或失败后收尾，调用方负责返回快照的后续用途 |
| 诊断启动 | `diagnosticProcess` 拥有进程退出信号、临时 profile 和有界关闭；诊断资源不能作为普通任务浏览器随意复用 |

`Pool.Close` 关闭准入并处理空闲实例；已借出的实例会在归还时销毁，不能把池关闭当作借用回调已经排空。上层须先停止并等待依赖浏览器的任务、诊断、账号验证和登录过程，再关闭池。直接 `NewPage` 的调用方负责关页；With 类入口限定回调作用域并执行清理。

浏览器基础包依赖 Rod、`identity`、`egress`、`governance`、`htmlfetch` 和 `middleware/proxy`；不依赖账号协调器、Engine 或后台。身份持久化不放进 `browser`，浏览器诊断报告算法及宿主操作生命周期见 `internal/browserdiagnostics`。现有公开原始 Rod 入口不等于所有自行创建的 page/popup/worker 都自动具备受管页面行为。

测试归属：`pool_test.go`、`stats_test.go`、`close_test.go`、`with_test.go`、`temp_dir_test.go` 验证资源；`browser_extra_test.go`、`wait_test.go`、`block_test.go`、`fetch_once_test.go`、`navigate_error_test.go` 验证页面；`state_test.go`、`state_navigation_test.go`、`isolation_test.go` 验证状态和隔离；`capture_test.go`、`capture_review_test.go`、`response_test.go`、`record_test.go`、`challenge_test.go` 验证观察；`governance_test.go`、`governance_rate_limit_test.go`、`egress_test.go`、`proxy_health_test.go` 验证网络交界；`identity_test.go`、`diagnostic_startup_test.go`、`diagnostic_cleanup_windows_test.go` 验证身份和诊断资源。

### 4.2 browser/identity：声明、原生配置与观察

| 文件 | 职责 |
| --- | --- |
| `profile.go` | 版本化 `Profile` / `Reference` / `ResolvedProfile`、运行身份值、Clone/Validate、不可兼容与冲突错误 |
| `cdp.go` | `Resolve` 从实际 Chromium 冻结原生身份、`CheckRuntime` 检查环境是否漂移、`Apply` 在导航前配置当前页和未来文档 |
| `observation.go` | `Observation` 及子观测值、首脚本探针 `FirstScript`、`Observe` 读取实际浏览器环境并保留不可用信息 |

此包没有 GORM、账号状态、进程池和持久绑定；Profile 是不可变版本声明，调用方修改自己的副本再提交新版本。它通过传入的 Rod 对象执行 CDP，不拥有该对象的关闭责任。受管登记状态由 `browser` 提供，`Observe` 不自行推断“已登记”。`profile_test.go` 验证值规则；实际页面初始化与观测交界在父包 `identity_test.go` 验证。

### 4.3 browser/identity/runtime：绑定与作用域

| 文件 | 职责 |
| --- | --- |
| `types.go` | `Subject`、`Binding`、Store CAS 契约、Resolver/Verifier 回调、快照和错误；主体明确区分 account/session |
| `manager.go` | `NewManager`、`Stage` / `Cancel` / `Reverify`、`Acquire` / `Pin` / `WithSession` / `WithProfile`、Scope 快照和释放、关停准入与排空 |
| `memory.go` | `MemoryStore` 的版本追加、绑定 CAS、锁内状态及独立查询副本 |
| `gorm.go` | `GORMStore` 适配 `models.BrowserProfile` / `BrowserIdentityBinding`，校验持久行并实现追加与 CAS，不自动建表 |

`Manager.mu` 拥有主体绑定、正在验证标记、已解析身份、引用计数和变更通知；独立的 `lifecycleMu` 保护准入/活动数/关闭，不能被慢 Store 或验证事务拖住关停期限。`Scope` 拥有本次固定身份及释放动作，调用方必须关闭；引用未释放时不能切换现用身份。`Reverify` 在不占用旧作用域的条件下验证候选，经绑定 CAS 后才调用候选发布回调；重启后的 pending 不绕过认证自动生效。

`Close` 拒绝新增并取消操作；`Shutdown(ctx)` 还等待作用域、回调和持久化收尾，返回 false 时宿主仍须保留依赖。Manager 不创建浏览器、不关闭注入的 Store/DB；通过 Resolve/Verify 回调连接 account 或匿名会话。运行包可以依赖 `identity` 和框架模型，值包 `identity` 不反向依赖 runtime。测试为 `manager_test.go`、`gorm_test.go`；后者同时包含坏数据校验与显式 MySQL 用例。

## 5. HTTP、发现与记录校验

### 5.1 htmlfetch

| 文件或文件组 | 职责与公开入口 |
| --- | --- |
| `client.go` | `Config` / `Client`、默认值、`NewClient` / `SetConfig`、代理选择；`Fetch`、`Page` / `Document` 的 HTML 查询及 `NewRenderedPage` |
| `request.go` | 可重放请求体的 `Request`、独立响应元数据/有界 body 的 `Response`、`Do`；配置/上下文/单次请求头合成、重定向和响应体关闭 |
| `errors.go` | HTTP 状态和 body 超限错误、`StatusCode` 查询；错误类别不直接决定任务终态 |
| `session.go`、`session_state.go` | 账号/站点独立 Session、上下文挂载、Cookie 导入读取、完整有界快照和恢复、Cookie 原有域/path/顺序语义 |
| `challenge.go` | `DetectSiteChallenge` 与 transport，明确挑战响应在 CookieJar 更新/重定向前返回，不污染当前会话 |
| `restricted.go` | `RestrictedReason` 按标题与 HTML 可见正文识别默认受限提示；额外站点词由业务传入，不直接判任务失败 |
| `json.go` | `DecodeJSON` 保留数字精度并拒绝尾随值；`JSONBlocks` 解析选择器命中的 JSON，不执行 JavaScript |
| `incremental.go` | `Representation`、`ContentHash`、`FetchIncremental` / `FetchIncrementalWith`；精确表示的条件 GET 和变化结果，不隐式持久化或重试 |
| `sample.go` | `SamplePolicy`、`WriteSample` / `ReadSample`、`OfflineResponse`；先脱敏后输出的版本化离线样本，不包含可执行流程或凭据恢复功能 |

`Client.mu` 保护可热更请求配置，网络授权在构造时固定，SetConfig 不能扩大 private origin 授权。单次请求在复制的 HTTP client 上绑定 Session，避免把全局 CookieJar 变成跨账号容器；`Do` 在返回前关闭网络 body。`Session` 自有带锁 cookie jar，快照/恢复由它保留 Cookie 属性，持久化和加密交给调用方。

`Representation` 的 URL、账号、请求头与语言作用域由调用方保证；发现 ETag 不意味着允许跨账号复用。

样本默认不保留 body、header 和 query，允许字段仍可能敏感；业务负责站点特有脱敏与文件原子发布。账户抓取的原始 HTML、捕获 header/body 不应因方便调试而直接进入自动归档。

测试与同名能力放在 `client_test.go`、`request_test.go`、`session_state_test.go`、`challenge_test.go`、`restricted_test.go`、`incremental_test.go`、`sample_test.go`；HTML/JSON 到记录校验的跨包合成流程在 `pkg/extract/pipeline_test.go`。

### 5.2 discovery

| 文件 | 职责与状态 |
| --- | --- |
| `links.go` | `CanonicalURL` / `SameOrigin`、`ExtractLinks`；从 htmlfetch 页面提取有界链接，不提交任务 |
| `xml.go` | `ParseXML` 解析 sitemap urlset/index、RSS 和 Atom；只返回目标，不递归抓取索引或读取外部 XML 实体 |
| `robots.go` | `ParseRobots`、`Allowed`、`CrawlDelay` 解析已获取规则；HTTP 状态、缓存、跳转和延迟调度归调用方 |
| `pagination.go` | `PaginationGuard` / `Observe`；实例锁保护单次顺序遍历的 URL、正文 hash、业务 ID 集合和终止原因，限制页数/条数/循环 |

只有分页 guard 持有可变遍历状态，生命周期是一轮遍历，不作为全局任务队列或持久去重表。`Continue` 只允许抓下一页，不表示本页数据已验收。新增 sitemap 格式或通用链接规则放相应解析文件，真实站点分页参数和 ID 提取放业务模块。`discovery_test.go` 覆盖这些通用判据。

### 5.3 extract

| 文件 | 职责与入口 |
| --- | --- |
| `validate.go` | `FieldRule`、`NewValidator`、`Validate` 和 `Report`；编译 JSON Pointer/类型/长度/正则/批内唯一性规则，输出有界问题明细及完整问题计数 |

Validator 保存构造时编译的规则，单次 Validate 的行结果和唯一性集合属于该调用；没有数据库、网络、任务或后台生命周期。报告反映已解析记录质量，任务成功/失败、存储和重试由业务与 Engine 决定。`validate_test.go` 验证规则，`pipeline_test.go` 验证合成 HTTP/JSON/离线样本的组合；真实模型固定预期不能塞入这些框架测试。

## 6. 预算与网络出口

### 6.1 governance

| 文件 | 职责与公开入口 |
| --- | --- |
| `config.go` | 请求、自动节流、阶段和全局资源政策；带指针的补丁、默认值、校验、`MergePolicy` 与有效站点值 |
| `patch.go` | `ResourcePatch` / `MergeResources` / `PatchOfPolicy`，保留显式 false/0 并建立独立覆盖值 |
| `controller.go` | `Controller`、可注入 Clock、政策更新、站点请求许可、Retry-After、阶段/全局资源许可和公平调度 |
| `resources.go` | Metrics/Sampler 契约、`RunSampler`、资源压力滞回调节及独立 Snapshot |
| `sampler.go` | OS/cgroup 采样实现；采样器自己的锁和 CPU 基线，不依赖后台页面轮询 |
| `transport.go` | 站点/Controller 上下文、可重建的 `WrapTransport`，实际 HTTP 交换的准入和 body 完成反馈 |

`Controller.mu` 集中拥有站点 token、并发、等待队列、远端期限、节流值、阶段计数、全局许可与资源采样结果；访问器返回副本。`changed` 用于预算等待，`policyChanged` 只通知成功的政策更新，普通请求和采样不能放大政策观察频率。阶段许可与全局任务许可在一个原子申请中获得，避免等待阶段期间占住全局容量。

请求预算按站点跨阶段/账号共享，任务与浏览器资源许可按进程全局分配。`WaitTaskReady` 在认领前等待，实际请求仍须 `AcquireRequest`。返回的归还函数或反馈函数属于该次许可；HTTP 包装器在 RoundTrip 返回时立即观察状态码、Retry-After 和该阶段延迟，许可则在 body 读到结束/错误、Close 或请求 context 取消时归还；失败或无 body 时立即归还。因此取消后的许可释放不等于底层 body 已关闭，调用方仍须关闭响应。`RunSampler` 是调用方启动并持 context 取消的循环，Controller 不拥有 App 生命周期或政策数据库。

此包不依赖 Engine、配置存储或业务站点。持久运行政策与版本事务见 `internal/runtimecontrol`，Engine 只通过许可接口编排。测试在 `controller_test.go`、`rate_limit_test.go`、`transport_test.go`；受管浏览器适配的测试归 browser。使用说明见 [运行治理](../RUNTIME_CONTROL.md)。

### 6.2 egress

| 文件 | 职责与公开入口 |
| --- | --- |
| `policy.go` | `Config` 声明、构造时冻结授权的 `Policy`、`New` / `Normalize` / `Resolve`；精确 origin 授权、URL 主机歧义拒绝和 DNS 结果校验 |
| `ip.go` | `PublicIP` 的统一公网地址分类 |
| `http.go` | `Client` 复制受支持客户端/transport 链、固定已校验 IP 建连、`ValidateProxy`；保留治理和代理包装器的语义 |
| `proxy.go` | `NewProxy`、浏览器 loopback 网关、HTTP 转发、CONNECT 隧道、固定上游的观察回调和 Close |
| `proxy_failure.go` | 可归因的上游代理失败错误及上游拨号分类，不把目标业务失败自动当代理失败 |

Policy 在构造时建立私有授权 map，无全局可变授权。`PrivateOrigins` 授权非公网地址，`AllowedOrigins` 限制允许的目标集合，两者含义不同。URL 路径和查询原始字节不得为了诊断或规范化随意改写。

每个 `Proxy` 拥有本地 HTTP server、请求取消和受锁保护的 CONNECT 连接集合；Close 同时取消请求并关闭 hijack 隧道和空闲连接，但不通过 WaitGroup 等待所有 handler/观察回调退出，不能把它视为回调排空屏障。普通浏览器实例拥有其 Proxy，隔离上下文共享父进程的出口。宿主提供的上游代理选择和回调是显式配置，本包负责框架网络出口，不接管业务数据库或系统防火墙。

`egress_test.go`、`allowlist_test.go`、`transport_test.go`、`proxy_failure_test.go` 检查目标/传输；`integration_test.go` 从 HTML、下载器、通知入口验证策略；`proxy_health_integration_test.go` 验证代理反馈连接。实际浏览器和 ffmpeg 交界分别在 browser 与 m3u8。配置边界见 [核心配置](../CORE_CONFIG.md)。

### 6.3 breakerpolicy

| 文件 | 职责 |
| --- | --- |
| `doc.go` | 声明策略独立于熔断状态机 |
| `policy.go` | `Policy`、`EffectivePolicy`、`ValidatePolicy`，统一默认值、上下限和配置有效性 |

这是无运行状态、无锁、无生命周期的纯策略包，只依赖标准库；不能加入探针调度、报告写入或检测历史。状态机和存储见 `internal/breaker` / `internal/breakerstore`，跨任务编排见 Engine 分册。`policy_test.go` 核对配置与状态机使用同一策略契约、默认值和校验。恢复验证串行运行，策略不提供无效的 `ramp_concurrency` 配置。

## 7. 下载与代理

### 7.1 middleware：共享下载基础

| 文件 | 职责 |
| --- | --- |
| `resources.go` | DownloadLimits、DownloadBudget、限额错误、有界读取及共享内存/临时磁盘准入 |
| `checkpoint.go` | 单任务单写者；合并通知、定时快照、Close 最终 flush 并等待完成 |
| `err.go` | 仅声明 `Err.GetErrors()` 错误流契约 |
| `path.go` | `SanitizeOutputPath` 规范下载相对目录/文件名，拒绝向输出根外逃逸 |
| `output_locks.go` | `OutputLocks.Lock` 一次性获取一组输出/断点路径；相同路径或祖先后代冲突，等待可取消，返回显式 unlock |

OutputLocks 的 mutex、活动路径集合和广播通道属于该实例；不会持有半组路径锁再等待另一半。路径做绝对化/清理与 Windows 大小写处理，但不解析符号链接、硬链接别名。`With` 下载器共享锁实例；分别 `NewDownloader` 的实例不自动合并路径互斥。`path_test.go`、`output_locks_test.go` 验证此层，不复制具体下载协议。

### 7.2 middleware/filedown

| 文件 | 职责 |
| --- | --- |
| `resources.go` | 有界私有 v2 位图续传及文件下载资源预留；拒绝无版本 JSON、v1 与未知版本 |
| `config.go` | 普通文件下载 `Config` / `DefaultConfig`，包括请求、并发、分块、输出和网络配置 |
| `overlay.go` | `Overlay.Clone` / `Apply` / `Then` / `Fields` / `IsZero`，区分未覆盖和显式零值，复制标量指针 |
| `downloader.go` | `NewDownloader` / `With` / `Config` / `SetClient` / `Download`、上下文到请求选项、探测/Range 校验/分块/断点/输出；共享状态及错误流 |

### 7.3 middleware/m3u8

| 文件 | 职责 |
| --- | --- |
| `resources.go` | 播放列表/分片/响应预算；读写当前 v1/v2 续传格式，拒绝无版本 JSON 与未知版本，超限不当作坏状态重下 |
| `stream.go` | 非加密媒体有界流式读取、读取阶段限速和临时段发布 |
| `publication.go` | 文件产物暂存、权限位继承与发布；OutputSegments 不承诺整目录事务 |
| `config.go` | 媒体下载配置、`DownloadOptions` / `OptionsFromRequest`、合并后缀校验和默认值 |
| `overlay.go` | merge/segments/playlist 的 `OutputMode`、覆盖值 Clone/Apply/Then/Fields/IsZero；站点/阶段/任务可叠加独立配置 |
| `downloader.go` | 播放列表选择与解析、重定向后相对 URL、byterange、map/init、密钥、AES-128 解密、受限响应读取、片段/断点/合并及产物处理 |
| `ratelimiter.go` | `RateLimiter` / `NewRateLimiter` / `Wait`，单次下载使用的带取消带宽等待 |

| 状态 | 所有者与同步 | 生命周期 |
| --- | --- | --- |
| 内存/磁盘预留计数 | DownloadBudget.mu；释放闭包用 sync.Once | 计数短锁不跨磁盘清点；失败和完成归还预留 |
| 已登记续传根及清点准入 | DownloadBudget.diskGate | 可取消等待，串行登记/扫描；扫描后再取 mu 检查并预留，不自动删除可恢复文件 |
| 脏标记、首次写错误、定时器 | Checkpoint 写者 goroutine | Notify 合并；Close 用 once 关闭 stop 并等待 done，随后读最终错误 |
| 完成片集合与状态快照 | 各任务 resumeMu | 锁内只复制，编码/写盘由 Checkpoint 在锁外完成；最终 flush 完成后才能发布或清理 |

WithResources 返回独立限额快照，With 继承共享预算。默认值、磁盘保守计量和物理配额限制见 [资源手册](../RESOURCE_BUDGETS.md)。`middleware/resources_test.go`、filedown/m3u8 的 `resources_test.go` 覆盖边界、当前格式恢复、旧格式拒绝和预算释放；检查点合并及失败传播由父包回归验证。

两个下载器各自拥有 `state`，并非共享一个全局下载状态。状态内 `proxyMu` 保护代理 transport 缓存；连接池跨 With 实例复用，而每次取得的 client 持有本实例 Timeout/SegmentTimeout，`laborMu` 保护按来源/目录/文件组合标识的 labor 和引用数，labor 自己的 `resumeMu` 保护断点修改，`OutputLocks` 保护最终输出及断点路径。m3u8 的密钥缓存只属于单次下载的私有接收者副本，不能跨任务共享认证相关密钥。

`With` 返回独立配置实例，同时共享原实例的状态、路径互斥和错误消息队列；它会按新配置创建默认 HTTP client，不继承原实例通过 `SetClient` 注入的 client/transport。`Config` 返回网络授权列表副本。`Overlay.Clone` 保留 nil 与显式零值并复制指针，是站点声明/阶段覆盖快照的基础。`NewDownloader` 才建立另一组状态。`SetClient` 是客户端替换入口，不能因为 Download 支持并发就推断替换配置也有并发热更保证。

两个下载器用固定 worker 与无缓冲工作通道限制等待 goroutine，生产和消费均响应取消；返回前等待已进入的 worker 及其进度回调收尾。filedown 增量计算偏移及剩余片长，不预先展开整个分片列表，并避免 int64 边界相加溢出；断点索引、播放列表和响应受有限字节/条目额度限制；共享预留不等于精确进程 RSS 上限。filedown 将全部预期分片校验、流式合并到同目录唯一临时文件，既有普通文件的权限位在写入前复制到临时文件，避免 0600 产物被替换为 0644；新文件沿用 0644 与 umask。显式 Close 成功后再 Rename 发布；合并或发布失败保留旧产物与分片，成功后才清状态、删除分片。Windows 也在关闭临时文件后替换目标，目标被占用导致替换失败时不提前删除旧文件；不承诺断电持久性或复制旧 inode 的属主、ACL 等元数据。

Download 同步执行并使用传入 context 约束请求、片段 goroutine 与合并；调用方等待它返回，再处理任务终态、业务落库和文件发布。下载器没有统一公开 Shutdown，宿主通过任务取消/排空及错误流消费者生命周期接入；不应让 HTTP handler 直接占有其内部 labor 或断点锁。网络必须经过 egress，代理选择经过 proxy，消息队列复用 `internal/msgqueue`；这些是具体适配依赖，不意味着可反向依赖 App/Engine。

两个包的 `bounded_download_test.go` 覆盖代理覆盖层超时、传输复用、有界调度及取消时等待进度回调；filedown 另覆盖极端偏移、旧产物安全发布、POSIX 权限保留与发布失败后的断点重跑。

filedown 测试：`downloader_test.go`、`overlay_test.go`、`resume_test.go`、`regression_test.go`、`destination_test.go`。m3u8 测试：`downloader_test.go`、`overlay_test.go`、`resume_test.go`、`regression_test.go`、`destination_test.go`、`byterange_test.go`、`url_resolution_test.go`、`limits_test.go`、`remediation_test.go`、`egress_ffmpeg_test.go`。路径互斥放父包测试，真实 ffmpeg 用例须显式配置环境，不能用纯解析测试替代外部进程行为。

### 7.4 middleware/proxy

| 文件 | 职责 |
| --- | --- |
| `manager.go` | `NewManager` / `NewManagerWithNetwork`、代理 API 获取/整批替换、定时刷新、错误流与 Close |
| `health.go` | 健康政策、固定/列表代理记录、`Reference`、准入和反馈、冷却/探针、无地址快照、Enable/Disable CAS 与有界记录回收 |
| `transport.go` | `WrapTransport` / `WrapFixedTransport`、单次选择的上下文绑定、`RequestProxy`、可重建包装器及响应体完成反馈 |

Manager 的 RWMutex 拥有列表、轮转位置、健康记录和版本、探针数及变更通道；刷新使用独立取消上下文和 WaitGroup。非空代理 API 在构造时先刷新，再启动周期循环；Close 取消刷新、唤醒等待者并等待刷新结束，不中断已经交给消费者的请求。

每个 Reference 固定本次使用的代理与健康代次，由 `Report` / `ReportHTTP` / `Release` 中的一项结束，重复结束安全；不能把旧请求结果记到新健康代次。明确固定代理不应自动换出口；健康启用时的池耗尽不会自动退回直连。调用方使用返回错误的 `Acquire` 或 `NextChecked`，不得丢弃选择失败后自行回退直连。

代理 URL 仅用于构造传输，健康快照只给不透明 ID；目标 403/429、账号失败、取消等不能直接归为代理失败。此包不拥有任务重试或账号切换。`manager_test.go`、`health_test.go`、`transport_test.go` 分别验证列表生命周期、健康状态与交换反馈，浏览器固定出口反馈归 browser 测试。

## 8. 事件与诊断输出

### 8.1 events

| 文件 | 职责 |
| --- | --- |
| `event.go` | 版本化 `Event`、阶段/队列快照、类型常量和 `TruncateError`；事件 schema 增量演进，消费者忽略未知字段 |
| `emitter.go` | `Emitter` / `Closer` / `NopEmitter`、`NewEmitter`、序列/丢弃计数、有界非阻塞入队、单消费者 drain 和受 context 约束的 drain 等待 |
| `sink.go` | `NewJSONLSink`、诊断副本脱敏、逐行 JSON 序列化、写入锁与幂等关闭 |

Emitter 拥有 channel、序列原子计数和 drain goroutine，RWMutex 保护入队与关闭的互斥，满队列/关闭后入队/写失败进入丢弃计量。`Emit` 只复制 Event 结构体，不深拷贝其中 `Counts` / `Attrs` / `Stages` / `Queues` map；调用方应提交独立快照，入队后不得继续修改，sink 写入时的诊断副本处理不能消除这个共享窗口。

JSONLSink 拥有传入 `io.WriteCloser` 的关闭责任，自己的 mutex 保护 Write/Close。宿主先发送收尾事件再 `Close(ctx)`；context 只限制等待 drain 的阶段，drain 完成后同步执行的 sink/底层 writer.Close 没有期限参数，不能承诺整个 Close 严格有界。等待超时会追加一次 dropped 计量，不是精确的未写事件条数；此时不关闭仍可能被 drain 使用的 sink，须保留后续清理责任。事件记录不承担分析和报告，后者在 obs。测试分别在 `event_test.go`、`emitter_test.go`、`sink_test.go`、`redact_test.go`。

### 8.2 loggers、notify、redact

| 包 / 文件 | 职责、公开入口与生命周期 |
| --- | --- |
| `loggers/loggers.go` | `LoggerConfig` / `LoggerSet` / `NewLoggerSet` 创建分通道 logrus logger 与 lumberjack 轮转输出；格式化独立诊断副本、同步控制台 hook。包没有自己的 worker、统一 Close 或配置热更事务，轮转 writer 由 logger 输出持有 |
| `notify/webhook.go` | `Webhook` / `NewWebhook` / `Notify` 把 `core.AlertEvent` 脱敏后 POST；每次调用通过 egress、关闭响应和本次包装客户端的空闲连接，不保留重试队列或告警频控状态 |
| `redact/redact.go` | `SensitiveKey` / `URL` / `Text` / `JSON` 识别已知凭据形状并处理独立诊断副本；纯转换，无运行锁/服务生命周期 |

日志策略和事件触发由上层决定，Webhook 只是传输。通用脱敏不能识别所有自然语言、HTML、SQL 字面量和未知字段秘密；站点特有内容的允许保留策略仍在业务模块。不能把 redact 用于改写请求 URL、任务身份、签名参数或断点数据。logger 的原输入对象不应被格式化器修改。

`loggers/loggers_test.go`、`loggers/redact_test.go` 验证日志级别和输出副本；`notify/webhook_test.go`、`notify/redact_test.go` 验证请求与脱敏；`redact/redact_test.go` 验证通用形状、数字精度及 URL 字节保留。具体报告分析见 [Obs 分册](OBS_STRUCTURE.md)。

## 9. 临时资源与显式运维检查

| 包 / 文件 | 职责与公开入口 | 状态、资源和依赖边界 |
| --- | --- | --- |
| `temporary/files.go` | `Files` / `ValidID` / `Open` / `FilePath` / `Remove`；宿主指定根和 namespace、UUID 路径、链接/reparse point 拒绝及 `os.Root` 内操作 | 不拥有业务作业状态；父目录必须由宿主指定，调用方负责同 ID 写/读/删除串行和打开 root 的关闭 |
| `temporary/cleanup.go` | `CleanupStore` / `Cleaner.Cleanup` / `Recover`；先持久化清理意图，再删目录，最后事务性删业务数据并标记完成 | Cleaner 同步执行，没有后台线程或私有 ID 锁；业务先排空工作并提供存储事务。Recover 只恢复已授权且未完成的清理 |
| `testdb/schema.go` | `Executor` / `Create`；校验测试库命名、独占 CREATE 后返回清理函数 | 只有本次成功创建才取得删除所有权，不接管已存在数据库；清理闭包自有 mutex/完成标记与独立超时，不因测试 context 已取消而跳过清理 |

临时资源契约见 [临时资源](../TEMPORARY_RESOURCES.md)。Cleaner 不决定发布、终止任务或重新采集；testdb 不进入正常采集运行路径。框架不提供旧策略表盘点或迁移工具。

测试分别在 `temporary/files_test.go`、`temporary/cleanup_test.go`、`testdb/schema_test.go`。这些测试约束文件/数据库所有权及可恢复步骤。

## 10. 测试层次与环境归属

本册列出测试所在位置与验证责任，不报告当前工作区测试结果。生产源码的依赖边界还由根目录 `architecture_test.go` 的 `TestPackageResponsibilityBoundaries` 检查，其中包含 contract 标准库依赖和 gormstore 不反向依赖 account 的约束；守卫不能代替锁序、取消及持久化行为测试。

| 验证层 | 所在位置与运行前提 | 能说明什么 |
| --- | --- | --- |
| 纯值、协议与合成服务 | 各包同目录 `_test.go`；使用内存 Store、本地 HTTP 服务、受控时钟/响应等 | 核对当前包规则、复制边界、可取消等待和错误归类；不证明真实站点可采集 |
| 账号池 MySQL | `account/gormstore/mysql_test.go`，读取 `PAPA_RUNTIME_TEST_CONFIG` | 使用测试独占数据库验证实际 CAS 与模型兼容，不能由 MemoryPoolStore 代替 |
| 接入资料 MySQL | `account/onboarding/store_test.go`，读取 `PAPA_ACCOUNT_ONBOARDING_MYSQL_DSN` 或 `PAPA_RUNTIME_TEST_CONFIG` | 核对真实资料存储与 CAS；未配置时用例跳过 |
| 身份 MySQL | `browser/identity/runtime/gorm_test.go`，读取 `PAPA_IDENTITY_TEST_ADMIN_DSN` 或 `PAPA_RUNTIME_TEST_CONFIG` | 核对实际模型迁移、版本追加/绑定 CAS；不是普通应用启动自动迁移 |
| 实际浏览器 | browser、account、onboarding 的相关测试，按用例读取 `PAPA_EGRESS_BROWSER_BIN`、`PAPA_TEST_CHROME` 或探测本机浏览器 | 核对 Chromium 页面/上下文/进程和协议交界；不同用例的选择与跳过条件不同，需检查实际测试输出 |
| 显式临时目录浏览器验证 | `browser/temp_dir_test.go` 的 live 用例，要求 `CRAWLER_ROD_DIAGNOSTIC_LIVE=1` | 核对实际 profile 根、共享池隔离、关池后借出实例仍存活、归还后 profile 清理；普通路径测试不代替此验证 |
| 实际 ffmpeg | `middleware/m3u8/egress_ffmpeg_test.go`、`remediation_test.go`，读取 `PAPA_EGRESS_FFMPEG_BIN` | 核对本地合并及外部进程的网络限制，未配置时明确跳过 |
| 平台专用行为 | `browser/diagnostic_cleanup_windows_test.go`、temporary 的链接用例 | Windows 句柄/目录清理或本机链接权限相关；其他平台或跳过不代表该平台行为已验证 |

新增并发/关停行为，应在拥有状态的包中建立受控回归，再在真实边界补必要验证；上层 App/Engine 只验证装配、跨组件顺序和其承诺行为。修改文档不需要运行全部外部依赖测试，也不能把未运行的环境用例描述为已通过。

## 11. 新增功能的放置规则

| 要增加或修改的行为 | 首选归属 | 不应放入的位置 |
| --- | --- | --- |
| 通用认证状态、候选检查、池选择、健康或用量 | `account` 的对应语义文件；无凭据持久化值放 contract，SQL 放 gormstore | App、Engine、HTTP handler 里的另一套账号状态 |
| 登录步骤或远程交互的通用机制 | `account/onboarding`；真实站点 Provider 在业务模块 | browser 进程池、Papa 中的真实站点目录 |
| 浏览器创建、受管页初始化、页面等待/网络观察 | `browser`；按页面或进程生命周期归属状态 | 身份数据库 Store、后台页面资源 |
| 身份字段、原生 CDP 或观测项 | `browser/identity`；持久绑定/验证作用域放 runtime | identity 值包中的 GORM、账号轮次或 App 引用 |
| HTTP 请求/Session/条件响应/离线样本 | `htmlfetch` | Engine 内嵌 HTTP 客户端状态；样本中写入凭据 |
| 链接、XML、robots、通用分页停止判据 | `discovery` | 任务调度器、真实站点固定分页参数 |
| 已解析记录的通用字段校验 | `extract` | 框架内的业务模型与固定验收期望 |
| 请求预算和单实例资源调节 | `governance`；持久政策事务仍在运行控制服务 | 下载器/账号/后台各自复制一套站点预算 |
| 目标授权、地址解析或出口建连 | `egress` | 业务采集器里的框架全局可写授权表 |
| 分块/片段/断点/合并规则 | `middleware/filedown` 或 `middleware/m3u8`；共同路径互斥在 middleware | App、Engine 或大而全的下载 manager |
| 代理选择、健康代次或单次交换反馈 | `middleware/proxy`，浏览器专有接线在 browser | 账号池中自行实现代理健康表 |
| 事件契约/写入、日志格式、通知传输、诊断脱敏 | 分别进入 events、loggers、notify、redact | 在这些基础包中生成报告或控制任务重试 |
| 临时目录/恢复清理、测试库 | 分别进入 temporary、testdb | 框架内的业务删表事务或普通启动迁移 |

新增实现须同时交代四项：所属语义、状态及资源拥有者、最小公开接口、允许依赖。出现持久化/锁/版本/生命周期交界时先保持原子性和关停语义，再决定是否拆文件或拆包。公开入口按当前职责归属，实现在所属组件；不能通过把整个 App/Engine 或可写私有状态传进新包完成表面拆分。

### 页面创建失败的资源所有权

`browser/page_creation.go` 在 Chrome 返回 target ID 时即拥有清理责任；连接/初始化失败时用独立三秒清理 context 关闭部分创建的 target，保留原取消错误。它不改变正常导航或状态恢复策略。`page_creation_test.go` 用合成 CDP 复现取消后原上下文无法清理的缺陷；真实 Chrome 导航取消另由 state_navigation_test 验证。
