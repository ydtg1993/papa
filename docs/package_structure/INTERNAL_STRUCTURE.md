# Internal 目录结构与职责细则

> 面向：维护 Papa 内部组件、应用装配和启停流程的开发者。
> 配套：[分册导航](README.md)、[总体架构规则](../ARCHITECTURE.md)、[开发手册](../DEVELOPMENT.md)、[根层分册](ROOT_STRUCTURE.md)、[Admin 分册](ADMIN_STRUCTURE.md)、[Engine 分册](ENGINE_STRUCTURE.md)、[Pkg 分册](PKG_STRUCTURE.md)、[Obs 分册](OBS_STRUCTURE.md)、[Cmd 分册](CMD_STRUCTURE.md)。
> 本文描述当前工作区实现。App 改造前的复现与判断保留在源码仓库 `docs/issues/2026-11-13-app-structure-review.md`，不作为当前缺陷清单或本次验证结论。

## 1. 范围、目录与依赖方向

`internal/` 是框架内部实现目录，不是单一业务层。这里既有应用组合根，也有声明规划、运行服务、持久化适配和底层执行设施。各包按自身契约协作；不能因都在 internal 下就互相读取可变字段或把完整 App / Engine 交给子组件。Papa 只面向单实例大单体，真实站点、业务模型配置和固定验收目标留在独立业务模块。

本册覆盖根层 `internal/` 的 **14 个包、67 个非测试 Go 文件、69 个测试 Go 文件**；`gormfake.go` 虽为非 `_test.go` 文件，仍是仅供仓内测试使用的支持代码。`engine/internal/` 的三个执行子包由 [Engine 分册](ENGINE_STRUCTURE.md) 维护。本目录当前没有非 Go 资源文件，也没有更深的子包。

```text
internal/
├── app/                 # 应用装配、公开扩展和生命周期
├── sitedecl/            # 冻结声明、规划与进程登记表
├── runtimecontrol/      # 运行策略校验、持久化核对和发布
├── browserdiagnostics/  # 身份操作、独立诊断和取消排空
├── reportrunner/        # 报表生成、定时触发和串行写入
├── breaker/             # 终态检测、检查点与恢复值契约
├── breakerstore/        # 熔断控制快照和历史的事务存储
├── database/            # MySQL 连接、显式迁移与 schema 查询
├── tracestore/          # Trace 文件发布、容量、读取与过期清理
├── taskstore/           # 任务认领、终态、释放和重投语义
├── gormfake/            # 仓内测试用 GORM/database/sql 假驱动
├── workerpool/          # 通用 worker、常规/加急队列和排空
├── msgqueue/            # 有界、非阻塞活动与错误通道
└── track/               # worker 活动消费和统计快照
```

| 包 | 非测试 / 测试文件数 | 主要入口与详述 |
| --- | --- | --- |
| `internal/app` | 21 / 33 | `NewApp`、`Run`、`Shutdown`、`Use*`，见 [App](#app) |
| `internal/sitedecl` | 5 / 5 | `RegisterSite`、`Sites`、`CheckSites`、`PlanSites`，见[声明](#sitedecl) |
| `internal/runtimecontrol` | 4 / 2 | `Service.Initialize/Read/Save/HistoryAction/DeletePool`，见[运行策略](#runtimecontrol) |
| `internal/browserdiagnostics` | 7 / 4 | `Service.Configure/Initialize/Apply/Diagnose/Rod/Shutdown`，见[诊断](#browserdiagnostics) |
| `internal/reportrunner` | 5 / 3 | `Runner.Start/Emit/Status/Close/Generate/Periodic/Exit`，见[报表](#reportrunner) |
| `internal/breaker` | 4 / 4 | `Detector.Observe/Snapshot/Checkpoint/Restore`，见[检测](#breaker) |
| `internal/breakerstore` | 3 / 4 | `Store.Save/LoadRuntime/List/HistoryForWindow`，见[熔断存储](#breakerstore) |
| `internal/database` | 5 / 7 | `NewDB`、`Migrate`、`Upsert`、schema 查询，见[数据库](#database) |
| `internal/tracestore` | 4 / 1 | `New/Write/List/Cleanup`，见文末 tracestore 章节 |
| `internal/taskstore` | 3 / 2 | `Claim/Complete/Release/RetryTask/RetryFailed/AdvanceRepeat`，见[任务存储](#taskstore) |
| `internal/gormfake` | 1 / 0 | `New`、`Open`、`MustOpen`、`Set*`，见[测试假库](#gormfake) |
| `internal/workerpool` | 3 / 2 | `NewWorkerPool`、`Start`、`Submit/SubmitUrgent`、`Stop`，见[执行池](#workerpool) |
| `internal/msgqueue` | 1 / 1 | `NewMsgQueue`、`SendActivity/SendError`，见[消息通道](#msgqueue) |
| `internal/track` | 1 / 1 | `NewStatsQueue`、`Start/Stop`、统计查询，见[统计](#track) |

依赖方向按职责判断：App 组合其它组件；Engine 使用声明落地后的执行参数和持久化/执行设施；admin 只把 HTTP 适配到服务语义入口。`runtimecontrol`、`reportrunner`、`taskstore` 不反向依赖 App、Engine、admin 或 HTTP。`sitedecl` 保留公开 Engine 注册协议的类型依赖，但不操作运行实例。`browserdiagnostics` 可以为本地合成 fixture 使用 HTTP，不依赖后台适配器。底层 `workerpool → msgqueue`、`track → workerpool` 不导入 Engine。

<a id="app"></a>

## 2. App：应用组合根

### 2.1 定位与职责

`internal/app` 是单实例应用的组合根：构造组件、注入依赖、登记业务扩展、连接后台入口、安排启动和排空顺序。App 可以持有服务实例，但不复制服务拥有的版本、锁、领域状态或事务规则。

声明规划归 `internal/sitedecl`，已登记站点归 Engine，运行策略事务归 `internal/runtimecontrol`，身份诊断归 `internal/browserdiagnostics`，报表执行归 `internal/reportrunner`。后台功能的 handler、页面描述、资源和审计事件语义归对应 `admin/*` 包。App 负责组合这些能力及保留当前公开入口。

同包文件按职责平铺。文件数量、行数和目录深度不是完成标准，不创建没有独立契约的 `app/service`、`app/manager` 或 `app/utils` 来容纳原有逻辑。

`resilience_mysql_test.go` 负责 App 组合根的真实 MySQL/子进程集成验收：从 NewApp、站点声明、Run 到 Shutdown 和取消收尾，覆盖两个站点、同名 catalog/detail 阶段及派生任务。只测试组件装配与跨组件契约，不把 SQL 状态转换或调度状态挪入 App；测试表、配置、HTTP 路由及资源只属于合成夹具。覆盖项包括 HTTP 在途请求、业务收尾 hook、运行采样/消息监听、审计、事件/报告及空浏览器池的关闭门控；父进程独占测试库和临时目录，并在强杀旧子进程后才启动恢复进程。逐次执行时间核对延迟约束，超时返回后由存活 handler 实测 DB 写入；父进程核验测试库与临时目录均已清理。运行方式见 [开发手册](../DEVELOPMENT.md#组件验证入口)。

### 2.2 当前物理目录

`lifecycle_load_mysql_test.go` 负责活跃 worker 正常关停、持续饱和的四组背压/无饥饿验证及故意断言失败后的 App/进程/文件/测试库清理。复用独占库与产物校验，采样器由测试自身管理；成功任务取消后结算规则归 Engine 内部 execution，不移入 App。

当前 `internal/app` 有 **21 个生产 Go 文件、33 个测试 Go 文件**，没有子目录，没有独立 JS/CSS/HTML 文件。

```text
internal/app/
├── doc.go                       # 组合根包契约与允许职责
├── app.go                       # 容器、选项和当前公开字段
├── bootstrap.go                 # 构造、迁移、事件流装配
├── lifecycle.go                 # Run / Shutdown、清理编排、日志监听取消与等待
├── shutdown.go                  # 业务关停钩子
├── schema_check.go              # 启动期 schema 提示
├── spec.go                      # 声明门面与 Engine 登记
├── http.go                      # 后台依赖、HTTP 启停、白名单组件接线
├── http_drain.go                # HTTP handler 准入及真实返回计数
├── admin_routes.go              # 内置页面选择、handler 构造和受保护路由
├── admin_audit.go               # 操作者、审计 sink、令牌缓存失效接线
├── pages.go                     # 表格/页面/资源/路由扩展登记
├── page_ownership.go            # 页面归属引用和无环校验
├── router.go                    # 业务路由分组与中间件
├── data_tables.go               # 选择内置任务工作台表
├── observation.go               # 归档与信号报告并列出口及共用关闭期限
├── reports.go                   # 报告组件静态值装配
├── runtime_policy.go            # 运行组件装配、采样生命周期和排空编排
├── accounts.go                  # 公开账号接入扩展
├── browser_identity.go          # 身份服务构造、初始化和生命周期接线
├── rod_diagnostics.go           # 公开 RodDiagnostics 工厂
└── *_test.go                    # 测试文件；逐组归属见 §2.6

internal/sitedecl/               # 冻结声明、规划和进程登记表
internal/runtimecontrol/         # 运行策略状态与事务
internal/browserdiagnostics/     # 身份/诊断操作、取消及排空
internal/reportrunner/            # 报告生成、串行写入和定时生命周期
admin/page/                     # 功能页默认描述的纯值契约
admin/access/                   # 白名单判定与拥有生命周期的重载器
admin/*/                        # 功能 HTTP、页面资源、默认描述和审计事件
```

`runtime_history.go`、`proxy_health_api.go`、`account_pools_api.go`、`account_compensation.go`、`rod_diagnostics_view.go` 五个私有转发文件已删除，其路由/资源接线由 `admin_routes.go` 承接。公开 `RodDiagnostics(*App)` 工厂继续保留。

### 2.3 文件与相邻包的职责明细

#### 2.3.1 容器、启动和退出

| 文件 | 当前职责与入口 | 维护边界 |
| --- | --- | --- |
| `doc.go` | 单实例组合根定位、扩展登记时机和依赖约束 | 功能展示与领域事务不回流到 App |
| `app.go` | `App`、`Option`、`WithConfigPath`、`WithModels`、`UseModels`；服务引用和启动前登记项 | 保留公开 Config/Logger/DB/BrowserPool/Engine；不再保存同义 `sites` 状态 |
| `bootstrap.go` | `NewApp` 创建配置、日志、DB、Engine、共享 configStore 和事件流；`Migrate`、事件适配 | 普通启动不自动迁移；事件落盘归 `pkg/events`，迁移实现归 `internal/database` |
| `lifecycle.go` | `Run` / `Shutdown`、统一失败清理、`shutdownRun`、日志监听及等待 | 编排启停并汇总排空结果；不实现各组件的内部领域算法 |
| `shutdown.go` | `UseShutdown`；逆序执行业务收尾，处理错误/panic/超时 | 超时不表示业务 goroutine 已终止，共享资源必须保留 |
| `schema_check.go` | `checkFrameworkSchema` 适配框架表/索引探测和缺项提示 | 不新增隐式迁移或独立 schema 服务 |
| `reports.go` | UseReportInputLimits 装配预算与 Runner 静态配置构造 | 不借 DB/Engine 查询回调，不注册在线分析扩展 |
| `observation.go` | 报告与归档两个有限消费者的并列出口 | 仅持值事件出口；共享退出期限由生命周期编排 |

#### 2.3.2 后台装配与公开扩展

| 文件 | 当前职责与入口 | 维护边界 |
| --- | --- | --- |
| `http.go` | `httpServer` 创建 Monitor、oao、令牌服务、白名单重载器、审计与 HTTP Server；`shutdownHTTP` 返回排空结果 | 连接功能回调，不定义内置页 HTML 或功能审计文案；HTTP handler 与白名单重载共用本次 HTTP 关停 context |
| `http_drain.go` | `requestDrain` 拥有 closing、pending、done；拒绝新的 handler、等待已进入的 handler 真正返回 | 不包装 ResponseWriter，保留 Hijacker/Flusher 等可选接口；关闭连接不等于 handler 排空 |
| `admin_routes.go` | `registerBuiltinPages` 选择默认页面；`mountAdminRoutes` 构造功能 handler 并直接挂载方法 | 每条 API 仍由统一 guard 包裹；不另建逐请求私有转发方法 |
| `admin_audit.go` | `runtimeAudit`、`operatorOf`、`tokenWriteHook` | 连接请求操作者、时间、sink 和缓存失效；事件的功能含义由所属 admin 包给出 |
| `pages.go` | `Page`、`UseTables` / `UseRouter` / `UsePage` / `UseScript` / `UseCSS` 及文件版本，扩展挂载 | 业务脚本是公开扩展输入；`UsePage`、挂载和 shell 均持有自己的导航元数据副本 |
| `page_ownership.go` | `Page.Owns` 引用、重复/自引用及循环校验 | 只校验导航归属，不改变授权；前端解释归 `admin/shell` |
| `router.go` | Router、Middleware、分组、方法与中间件链、`NoAuth` | 保持公开路由和默认鉴权语义，不添加功能 SQL/状态规则 |
| `data_tables.go` | 调用 `tasksource.WorkspaceTable` 并选择内置表 | 工作台 Label、Header 和嵌入 HTML 归 `admin/tasksource`；旧 `Table` 的通用默认值保持不变 |

`admin/page.Descriptor` 仅包含页面 Key、Label、Group、Section、Script、CSS，不依赖 App、server、shell 或其它框架实现。accounts、accountpools、runtimecontrol、diagnostics 的 `page.go` 和 taskflow 的 `assets.go` 提供各自 `Page()` 默认值。App 决定是否挂载和导航顺序。

内置脚本继续按“诊断 → 运行治理 → 账号池 → 账号管理 → 可选任务流”顺序追加，业务先登记的脚本仍在前面。任务流保留 `server.TaskFlowScript` / `TaskFlowCSS` 公开变量在 Run 前的覆盖行为；不能只改为读取功能包默认值而丢掉这项扩展能力。

#### 2.3.3 声明规划与 Engine 落地

`spec.go` 保留 SiteSpec/StageSpec/BreakerSpec/RetrySpec 别名，以及 RegisterSite/Sites/CheckSites/DescribeDownloadOverlays 入口。`App.RegisterSites` 先完成整批校验与规划，再登记到 Engine；`applyStagePlan` 只应用计划与记录日志。`App.Site` 委派 `Engine.Site`，Engine 的 siteCatalog 是已登记站点的唯一来源。

| `internal/sitedecl` 文件 | 职责与快照边界 |
| --- | --- |
| `doc.go` | 声明包定位及允许依赖 |
| `types.go` | 声明类型、站点快照、熔断默认值/覆盖转换；站点 Headers 和 RestrictedKeywords 在转换时复制 |
| `registry.go` | 进程登记表及顺序；写入和 `Sites()` 返回时复制框架拥有的嵌套值 |
| `plan.go` | 批量校验、阶段默认值、重试/延迟、最终有效熔断配置及入口选择；计划和每次入口回调各持独立快照 |
| `overlay.go` | 下载器覆盖项的离线描述与自查 |

复制范围包含 Headers、RestrictedKeywords、Breaker/Policy、Stages，以及站点/阶段下载覆盖中的标量指针。M3U8/FileDown 的复制由各自 Overlay.Clone 统一提供，Engine 的下载覆盖登记复用它们。Fetcher 是有自身身份与生命周期的业务对象，继续共享，不做反射深拷贝。

规划阶段不操作 Engine 实例、不连库，但 `StageSpec` 使用 `engine.Fetcher`，`StagePlan` 含 `engine.StageConfig` 和入口提交回调。这是现有公开注册协议的类型依赖，不能称为完全不依赖 Engine 的纯值包。最终有效 breaker 配置在规划时校验，包含继承的全局默认值；无效批次在登记任何站点/阶段前返回错误。

#### 2.3.4 运行服务和后台适配

| 文件 / 包 | 实际职责 |
| --- | --- |
| `runtime_policy.go` | 装配运行策略、账号池、身份服务和健康事件；持有采样 cancel/done；编排运行组件取消与共享截止时间内的排空 |
| `accounts.go` | `UseAccountOnboarding` 检查公开接入服务与 Engine 账号管理器一致，并接入请求上下文工厂 |
| `browser_identity.go` | 构造窄 Options、初始化身份管理器、接入 Engine/账号管理器、调用诊断服务关停；公开 `BrowserIdentities` 保留 |
| `rod_diagnostics.go` | 公开 RodDiagnostics handler 工厂；协议适配归 `admin/diagnostics` |
| `admin/diagnostics` | handler 解析 HTTP，`View` 补充站点导航数据；身份服务视图和账号池视图共用这条适配 |
| `admin/breakers`、`admin/queues`、`admin/taskflow` | 各自 `audit.go` 定义操作日志值和动作语义；App 只把事件连接到 Recorder |

身份诊断使用的受管浏览器，其进程与临时 profile 清理归 `pkg/browser`：`diagnostic_startup.go` 管关闭幂等及结果，`diagnostic_cleanup.go` 管拥有目录的有界删除，平台文件仅处理进程终止和可重试的系统错误。`internal/browserdiagnostics` 通过 `CloseDiagnostic` 收集真实清理错误；App 不实现 Windows 文件锁重试或直接操作诊断临时目录。普通 `Browser.Close` 和池锁序保持原有协议。独立 Rod 启动检查仍在 `internal/browserdiagnostics/rod.go` 中拥有自己的 launcher、日志和临时目录；两条诊断路径的具体资源归属见 §4.2。

`browserIdentityState` 是诊断 Service 的别名；closing、pending、取消上下文、诊断互斥与报告仍由该 Service 拥有。Configure 用 sync.Once 绑定依赖，App 不在每次请求中重新改服务配置。

### 2.4 状态所有权与依赖

| 状态 / 资源 | 唯一拥有者 | App 的责任 |
| --- | --- | --- |
| 启动扩展：模型、页、表、路由、报告扩展、收尾钩子 | App | Run/迁移前登记；移交或复制，不承诺运行期任意修改 |
| 进程级冻结声明 | `internal/sitedecl` | 消费副本并应用计划 |
| 已登记站点 | Engine siteCatalog | 通过 `App.Site` 委派查询，不保留第二份站点表 |
| 阶段、任务、暂停、熔断和执行排空 | Engine 及内部组件 | 登记、启动、Stop，不重写内部锁序/持久化规则 |
| 策略 revision/document/effective/uncertain 与发布锁 | `internal/runtimecontrol.Service` | 注入存储、Engine 的窄运行接口和账号池 |
| 账号池 / 登录流程 | `account.Pools` / `onboarding.Service` | 先通知所有组件停止，再汇总排空结果 |
| 身份诊断准入、在途计数与报告 | `internal/browserdiagnostics.Service` | 初始化、接线并检查 Shutdown 结果 |
| 资源采样循环 / 错误日志消费者 | App 的 runtimeState / messageListenerState | 持有 cancel/done；取消后等待实际退出 |
| 报告入口、单写者聚合与固定 writer | `internal/reportrunner.Runner` | 与归档共用 2 秒关闭期限；不借业务资源 |
| 白名单判定与周期重载 | `admin/access.Handler` / `WhitelistReloader` | 注入读口和发布回调，收集 Stop 结果 |
| 配置读写、TTL 缓存与失效代次 | `admin/configstore` | 共用 Store；本进程保存成功后 Invalidate |
| HTTP 服务 / handler 准入与在途计数 | `http.Server` / App 私有 requestDrain | 关闭准入并等待，区分连接关闭和 handler 返回 |
| 事件 sink / 审计队列 / 系统采样 | events / oplog / sysinfo 组件 | 安排启停及共享依赖关闭顺序 |
| 共享 DB 和浏览器资源 | App 创建并接线，多组件使用 | 只有所跟踪的使用者均报告排空才主动关闭 |

依赖由组合根指向声明规划、Engine、运行服务和 admin 适配器。功能包不得反向导入 App/server；服务不得为了取依赖持有完整 App。窄接口和回调必须表达明确查询/操作，不能用任意字段访问集合绕过边界。

### 2.5 启动、取消与排空协议

NewApp 构造时已创建 Engine；NewEngine 会读库预热活跃任务并启动延迟 dispatcher，不能把全部后台活动都归到 Run。正常 Run 启动顺序：运行服务初始化 → run_started/报告循环 → 浏览器/HTML/下载器 → 熔断运行期/阶段 → 日志消费者 → HTTP。`Run` 安装统一 defer：先取消运行 context，再走 `shutdownRun`；运行组件初始化或后续启动失败也会清理已取得的资源。未成功进入运行阶段时不生成 run_stopped/退出报告。

关停顺序：HTTP → 业务收尾钩子 → Engine.Stop → 运行组件 → 日志监听等待 → 代理 → 结束信号/在线报告及归档共享期限关闭 → 审计 → 条件关闭浏览器与 DB。观测超时不参与业务依赖关闭门控。

- `shutdownHTTP` 先关闭 requestDrain 准入并取消白名单重载，再让 Server、handler 计数和 reloader 共用本次 HTTP 截止时间。包括仍在执行的 hijacked handler；Server.Close 不能把这类请求视为完成。超时后仍继续其它组件清理，但共享资源关闭条件保留 false。
- `drainRuntime` 先对 onboarding、账号池和身份诊断调用 BeginShutdown，并取消采样；随后所有等待使用同一个 context。一个组件超时不会跳过其它组件的停止和结果收集。BeginShutdown 的生命周期门只能短时持锁，不能等待业务存储或回调。
- onboarding/lifecycle.go 用独立生命周期锁保护准入与 WaitGroup.Add，资料事务仍使用原事务锁。Start 的入口计数保留到新 workflow 取得计数之后。账号池的唯一完成流程等待协调器/worker 退出，再按原 Manager 锁摘除；drained 包含这一过程，调用方仅在所给 context 内等结果。
- `WhitelistReloader` 独立持有 goroutine/cancel/done。ContextStringListReader 经 TTL 缓存、Store、GORM 传到 SQL；只提供带 context 的读取入口。普通读取失败继续记错并放宽；取消/截止时间错误不发布空白名单。忽略取消的读端或未返回的发布回调使 Stop 返回 false，依赖保持打开。
- TTL 缓存读取与 Invalidate 用失效代次协调；失效期间完成的旧读重新回源，不把旧值或旧错误交给发布者。读取失败不缓存，正常 TTL 与本地保存后的即时失效语义不变。
- 引擎错误与中间件错误消费者都响应 App context，并由同一完成信号追踪；不会为让消费者退出而关闭 Engine 的公开错误通道。正在执行的日志写入也必须返回后才报告排空。

App 主动关闭共享 DB/浏览器的条件是 HTTP（含重载）、业务钩子、Engine、运行组件和日志监听全部排空。返回 false 可能表示仍有执行者，也可能只是 Engine 中留有 spill/delay/未确认任务或最终熔断写入失败；App 对这些情况统一保守保留 DB/浏览器。不能通过强制关闭连接或另起 goroutine 后立即返回来伪装成排空。这里的统一截止时间分别属于 HTTP 关停和运行组件关停，不表示整个 Run 的所有清理共用一个总预算。

这些条件不等于 App 汇总了所有资源的成功关闭：代理 Close、事件 sink 关闭和审计 Close 不参与上述五项布尔合取；run_stopped 的 drained/stopStats 来自 Engine.Stop，也不是 HTTP、业务钩子和身份服务的全应用排空结果。requestDrain 只跟踪 ServeHTTP 返回；handler 自行启动并在返回后继续运行的 goroutine，须由业务收尾钩子或所属服务另行管理。Shutdown 只发出取消信号，没有等待 Run 清理完毕的返回契约。

### 2.6 测试归属与验证边界

#### 2.6.1 App 的 30 个测试文件

| 文件 | 验证职责 |
| --- | --- |
| `app_test.go`、`app_extra_test.go` | 选项、公开页面/资源/路由登记、挂载和操作者适配 |
| `admin_routes_test.go` | 内置页面 key、导航和资源顺序；统一 guard；TaskFlow 公开资源覆盖 |
| `spec_test.go`、`spec_validation_test.go`、`check_test.go` | 声明到 Engine 的装配、副本隔离、applyStagePlan 错误传播、无效批次不落地 |
| `router_test.go`、`page_ownership_test.go`、`page_sections_test.go` | 路由/中间件鉴权、导航引用和真实 HTTP 元数据 |
| `schema_check_test.go`、`data_tables_test.go` | schema 提示、内置任务表选择与版本化动作 |
| `http_drain_test.go`、`shutdown_resources_test.go` | 连接强关后仍活跃的 handler、hijack、拒绝新请求、白名单未排空时保留数据库，以及全部退出后的关闭 |
| `runtime_lifecycle_test.go`、`shutdown_test.go` | 运行组件取消/等待、采样和日志消费者、初始化失败清理；业务钩子顺序、错误、panic 和超时 |
| `events_test.go` | 事件流配置和输出 |
| `report_interval_test.go`、`report_mode_test.go`、`report_on_exit_test.go`、`report_sections_test.go` | App 在线信号报告配置、定时/退出和禁用开关 |
| `token_hook_test.go` | 令牌写入后的缓存失效与失败保留 |
| `runtime_policy_test.go`、`runtime_history_test.go` | HTTP → 策略服务 → 持久化/Engine 的集成 |
| `runtime_ui_browser_test.go` | 真实运行治理资源的浏览器集成 |
| `accounts_test.go`、`account_pools_api_test.go` | 账号接入一致性、账号/池 HTTP 与脱敏集成 |
| `accounts_browser_test.go`、`account_operations_ui_test.go` | 账号输入、迟到响应、池策略、补偿的跨组件浏览器集成 |
| `runtime_test_helpers_test.go` | fake DB、请求和运行环境辅助 |

#### 2.6.2 所属组件的行为测试

| 所在包 / 文件 | 关键覆盖 |
| --- | --- |
| `internal/sitedecl/registry_test.go`、`plan_test.go`、`check_test.go` | 嵌套声明副本、计划/入口回调隔离、有效继承 breaker 校验；纯声明算法不借道 App |
| `admin/access/reload_lifecycle_test.go`、`reload_test.go` | 单次启动、取消与重试等待、不响应取消的读端、迟到结果、发布回调排空、既有放宽/日志/TTL 语义 |
| `admin/configstore/string_list_context_test.go`、`context_sql_test.go` | 取消不污染缓存、失效覆盖在途旧读；经实际 database/sql 驱动证明 context 传播 |
| `admin/tasksource/workspace_test.go` | 工作台展示和动作保持一致，同时保留旧 Table 默认值 |
| `admin/accounts/frontend_test.go`、`admin/runtimecontrol/frontend_test.go` | 原 App 的纯脚本文本断言归所属功能包 |
| `admin/breakers/audit_test.go`、`admin/queues/audit_test.go`、`admin/taskflow/audit_test.go` | 整站/阶段/强制解除语义、部分失败计数、动作对象和审计身份保持一致 |
| `admin/diagnostics/identity_test.go` | 身份视图的站点导航适配 |
| `pkg/account/onboarding/shutdown_test.go`、`pkg/account/pools_shutdown_test.go` | 资料 Get/SaveCAS/List 卡住时仍能取消与有界等待；Manager 持锁发布候选时，协调器摘除也纳入排空结果 |
| `pkg/browser/diagnostic_cleanup_windows_test.go`、`diagnostic_startup_test.go` | 真实文件锁延迟释放、持续锁定的截止时间和原始错误、重复关闭、池名额归还与启动取消；真实 Chrome 回归归浏览器与诊断服务包 |
| `pkg/account/onboarding`、`pkg/account`、`internal/browserdiagnostics`、`pkg/browser/identity/runtime` 其它测试 | 各组件的准入、操作状态、取消与排空；App 保留组合行为测试 |
| `internal/reportrunner` | 固定聚合/writer、有限队列与有界关闭，不持 DB 或任意扩展回调 |

#### 2.6.3 依赖守卫及证明范围

`architecture_test.go::TestPackageResponsibilityBoundaries` 检查：

- admin 不导入 App；功能包不反向导入 server；server 不拥有生产 JS/CSS/HTML。
- App 不拥有独立 JS/CSS/**HTML**；Go 字符串中的前端源码不在这个扩展名守卫的识别范围内。
- sitedecl 不导入 App、admin 或 net/http，允许既有 Engine 注册协议类型依赖。
- admin/page 不导入框架实现；值契约不得成为新的装配层。
- runtimecontrol/reportrunner/taskstore 不导入 HTTP/admin/App/Engine；reportrunner 不导入整份应用 config、GORM、obs/analyze、obs/pattern 或历史输入加载器。
- browserdiagnostics 不导入 admin/App；engine/internal 不导入父 engine/admin/App。

守卫不能证明深副本、真实排空、SQL 方言正确或浏览器布局。行为回归分别覆盖这些协议；fake 与合成 SQL driver 不代替 MySQL 现场验证，浏览器用例是否运行取决于实际测试环境。本分册说明实现和覆盖位置，不把新增测试文件或守卫通过表述为全量测试已经通过；执行批次与限制写在 `docs/issues/` 的验证记录。

### 2.7 后续开发的放置规则

| 需求 | 首选位置与约束 |
| --- | --- |
| 声明字段、默认值和校验 | `internal/sitedecl`；输入/返回/入口交付明确复制范围，Fetcher 仍按业务对象处理 |
| 执行、任务持久化和熔断 | Engine 或既有组件；保持原子性、锁序、背压和 Stop 协议 |
| 后台功能 API、页面或默认表视图 | 对应 `admin/<feature>`，同时拥有资源、描述和测试；App 只选择、注入与挂载 |
| 功能审计动作语义 | 对应 admin 功能包；App 适配身份、时间和 sink |
| 运行策略、诊断或报告执行 | 对应已有 internal 服务；通过语义接口协作 |
| 公开业务扩展 | App 的 Use* 门面；真实站点、模型与验收接线留在独立业务模块 |
| 新后台循环/共享资源使用者 | 先明确 owning component、取消入口、等待结果和未排空后的依赖保留，再加入 App 关闭条件 |
| 新包或职责文件 | 写清输入、输出、状态拥有者、允许依赖、关停方式与测试归属，同步本分册及 DEVELOPMENT |

重构须同时保留公开 API、路由、页面 key 和资源顺序，检查状态所有权与失败路径的实际行为。目录整理不能替代这些契约。

<a id="sitedecl"></a>

## 3. Sitedecl：声明登记、验证与规划

`internal/sitedecl` 拥有启动声明的值快照和注册计划，输入为 `SiteSpec` / `StageSpec` 与全局默认配置，输出为校验结果、`Plan` / `StagePlan` 和离线覆盖说明。登记实际站点、构造 worker、提交入口任务的时机仍归 App / Engine；本包不持有运行中的 Engine、不连库，也不拥有业务 Fetcher 的生命周期。

| 文件 | 职责、入口与边界 |
| --- | --- |
| `doc.go` | 定义声明包定位及不依赖 App、HTTP、admin 的约束 |
| `types.go` | `SiteSpec`、`StageSpec`、`BreakerSpec`、`RetrySpec`；转换 `core.Site`、合并全局/站点熔断配置；站点 Headers 和 RestrictedKeywords 独立复制 |
| `registry.go` | `RegisterSite` / `Sites` 的进程登记表、登记顺序、站点键校验；`cloneSpec` 复制框架拥有的嵌套值 |
| `plan.go` | `CheckSites` / `PlanSites` 共用批量校验与推导；检查站点/阶段键、worker/队列尺寸、轮询限制、只读覆盖、Delay/Backoff、最终有效熔断配置；选择入口回调并输出启动提示 |
| `overlay.go` | `DescribeDownloadOverlays` 只描述站点/阶段显式写过的下载覆盖，不创建下载器或执行请求 |

`registeredMu` 只保护进程登记表与返回副本；`RegisterSite` 按调用顺序追加冻结值，`Sites` 再复制返回。运行计划不修改登记表。复制覆盖 Headers、RestrictedKeywords、Breaker/Policy、Stages、M3U8/FileDown 覆盖中的标量指针；下载覆盖复用各自 `Overlay.Clone`，Fetcher 保持同一个业务对象。

`Plan.Site`、下载覆盖和入口闭包分别拥有基线副本，每次入口调用再次复制站点值，业务入口修改参数不会污染后续调用。`PlanSites` 返回的错误可能伴随部分推导值；调用方必须先检查整批错误再登记，App 已按此顺序装配。`CheckSites` 不意味着资源可启动或真实站点可采集。

依赖包括 `config`、`core`、`internal/breaker`、下载覆盖包和 `engine` 的 `Fetcher`、`StageConfig`、`SiteDownloads`、`EntrySubmitter` 协议。阶段唯一键是 `site:stage`；`Entry` 只控制启动入口，整站暂停由 `StartPaused` 表达，不让声明规划承担运行暂停状态。登记和规划不启动后台循环，无独立 Stop；登记扩展在应用运行前完成。

测试归属：`registry_test.go` 验证登记顺序与嵌套副本；`plan_test.go` 验证参数、默认值、站点内阶段唯一性、入口政策和回调隔离；`check_test.go` 验证错误汇总、继承后熔断参数与覆盖描述；`spec_test.go` 验证轮询/只读覆盖声明和熔断继承转换；`helpers_test.go` 提供合成 Fetcher。代表回归为 `TestRegistryOwnsNestedDeclarations`、`TestEntryCallbacksReceiveFreshIndependentSnapshots`、`TestCheckSitesValidatesEffectiveInheritedBreaker`。落地顺序由 App 的集成测试继续验证。

## 4. 运行服务

<a id="runtimecontrol"></a>

### 4.1 Runtimecontrol：运行策略事务与账号池引用

`internal/runtimecontrol` 的输入是带 revision 的策略文档或历史恢复请求，输出是领域 `Result`、快照和 `Event`。HTTP 状态、请求解析、审计落点和 GORM 配置适配属于 `admin/runtimecontrol`；本服务只通过 `Runtime` 和 `Store` 接口调用执行能力及持久化。

| 文件 | 职责、入口与边界 |
| --- | --- |
| `doc.go` | 策略校验、CAS 核对、发布和账号池删除共同边界的包契约 |
| `document.go` | `Document`、`Decode`；只接受一个 JSON 对象，拒绝未知字段和不支持的 schema_version |
| `service.go` | `Runtime` / `Store`、`Result` / `Event` 和 `Service`；Initialize、Read、Save、Acknowledge、History、HistoryAction、DeletePool，统一版本核对与发布 |
| `history.go` | `Changes` 计算叶子字段差异；`PatchHistory` 按选择路径恢复，保留未选字段、区分删除与继承，并保留 JSON 数值精度 |

`Service.mu` 统一拥有 revision、document、effective、uncertain 和 pools 引用。锁覆盖校验、持久化 CAS、持久结果核对、运行配置发布，以及检查账号池引用后删除的操作；不能把删除拆成锁外“先查引用、后删池”。策略读口在同一发布锁内采集相关治理/代理/账号池快照，返回文档和有效配置的独立副本，避免版本和快照错配。

`Save` 先复制输入、检查版本和全部有效参数，再调用 Store 保存；写入确认丢失时用独立的五秒核对 context 读取一次，核对成功后发布，不重放写入。无法确认时保持 uncertain，后续读/写/删除先核对持久版本；拒绝版本倒退或同版本内容改变，未核对时不确认远程等待解除。历史恢复先计算选择性补丁，再通过带原 revision 的 `Save` 检查期间修改。

依赖允许 `config` 的运行配置值、`core`、`models`、`account`、`governance`、`proxy`，不导入 App、Engine、admin、net/http。`Runtime` 提供解析和安装治理配置、查询站点/代理的明确操作；不能扩成访问 Engine 任意字段的接口。Service 不启动后台 goroutine，没有独立 Stop；请求 context 传给 Store，外层 HTTP 排空先于共享存储关闭，采样循环由 App 管理。

测试：`service_test.go` 覆盖确认丢失不重写、uncertain 核对、版本倒退、文档副本、同版本快照、账号池引用与发布互斥；`history_test.go` 覆盖字段选择、删除恢复继承、非法路径及 duration 精度。关键测试包括 `TestLostAcknowledgementIsConfirmedWithoutReplayingWrite`、`TestPoolDeletionCannotCrossPolicyPersistenceAndPublication`、`TestPolicySnapshotStaysWithItsRevision`。HTTP 集成和真实存储适配验证分别在 App/admin，不由这里的内存 Store 替代。

<a id="browserdiagnostics"></a>

### 4.2 Browserdiagnostics：身份操作与独立浏览器检查

`internal/browserdiagnostics` 负责浏览器身份管理操作、独立启动检查、本地合成页一致性检查，以及所有这些操作的准入、取消与排空。`Options` / `Resources` 仅提供站点存在查询、浏览器配置、账号/浏览器池/代理资源和日志回调；后台路径、页面及 HTTP 状态归 `admin/diagnostics`。

| 文件 | 职责、入口与边界 |
| --- | --- |
| `doc.go` | 独立诊断、合成 fixture、报告和生命周期的包契约 |
| `options.go` | `Options` / `Resources`；Configure 一次绑定，Initialize/SetManager 启动接线，Diagnose/Rod 公共入口、共享诊断互斥与超时 |
| `service.go` | `Service` 状态、身份解析/验证回调、View、Begin、BeginShutdown、Shutdown；公共池与账号主体走各自明确资源接口 |
| `actions.go` | `Input`、`InvalidInput`、`ActionResult`；Apply 校验站点/主体并执行 profile、stage、cancel、reverify、diagnose；返回 Attempted 供适配层判断审计语义 |
| `identity_diagnostic.go` | 身份报告值、本地 loopback fixture、首请求头和首脚本观测、与解析身份比较、有限历史摘要及环境变化判断 |
| `rod.go` | 独立 launcher、about:blank 检查、环境采集、32 KiB 日志缓冲、随机调试端口、独立临时 profile 和本次执行的清理 |
| `report.go` | `RodReport` / `RodStep`、步骤失败后的跳过规则、耗时、错误/URL/显式秘密脱敏及报告文本上限 |

`Service.once` 绑定启动依赖，manager 在启动期接入；`diagnostic` 互斥共享于 Rod 与身份诊断，忙时立即拒绝。`mu` 保护 closing、pending、changed、取消 context 和最近 128 份身份诊断摘要；`Begin` 同时注册操作并组合调用方/服务取消，完成回调必须按协议调用一次。`View` 和 `Apply` 即使没有 HTTP 调用者，也独立计入生命周期。

`BeginShutdown` 先拒绝新操作，取消服务操作并通知身份 Manager 关闭；`Shutdown(ctx)` 等待 pending 归零，再等待 Manager。截止后返回 false，不能当作仍运行的浏览器、存储或回调已经退出。身份 Manager 的绑定和候选状态归 `pkg/browser/identity/runtime`，服务只持有它并负责这一层生命周期接线。

资源分两条路径维护：身份一致性检查创建随机路径的 loopback 服务、独立临时根目录和单浏览器诊断池，限制出口为该精确 origin，不加载账号凭据或登录回调；受管浏览器的 `CloseDiagnostic` 真实清理错误并入报告。fixture 自身的互斥锁保护首请求头，返回副本。Rod 启动检查则直接持有本次 launcher、Rod 连接、日志和临时 profile，在 defer 中关闭/终止并记录目录删除结果；不会删除配置中的用户 profile。不得把 Rod 路径误写成全部由浏览器池清理。

本包依赖浏览器/身份/账号/代理/出口包、BrowserConfig、Rod 与环境采样库；其中 net/http 只服务合成 fixture，不是后台业务 handler。检查结论限于报告中的受管主文档、环境和能力范围，不能据此声称目标站认证、实际出口信誉或所有浏览器上下文已验证。

测试：`actions_test.go` 验证脱离 HTTP 的输入和 CAS 操作；`lifecycle_test.go` 验证共享准入、取消、排空和独立 View 生命周期；`rod_test.go` 验证启动失败、临时目录清理与脱敏；`identity_test.go` 验证 fixture 边界、高熵身份字段及真实浏览器首文档/清理。真实 Chrome 回归是否执行取决于环境，不能由合成输入单测推断。

<a id="reportrunner"></a>

### 4.3 Reportrunner：定时与退出报表

collector 独占单调前进的接收日桶；旧日迟到信号只计入运行累计并单列 late，不使当前日回退。同日窗口覆盖最早已计入信号。超长或已截断的 site/stage 不以截短键合并为精确维度，显式计 overflow。

`internal/reportrunner` 只接收静态 Config/Inputs 和有限值事件；不接收 DB、App/Engine、Logger、业务 Section/Pattern 或查询回调，不扫描历史 JSONL/日志。离线分析仍归 obs；在线使用 obs 的报告值和 report 渲染。

| 文件 | 职责、入口与边界 |
| --- | --- |
| `doc.go` | 信号辅助服务契约 |
| `runner.go` | Config/Options/Runner；准入、固定聚合消费者、周期/退出请求、Close deadline 与纯原子 Status 健康查询 |
| `aggregate.go` | 单写者计数、有限 site/stage 维度、最后快照与失败样本、partial/unavailable 来源、unknown 报告 |
| `tables.go` | 从已有有限聚合值构建表格 |
| `writer.go` | 固定 writer；自有文件写入、持久目录游标每轮最多续扫 128 项的保留清理，panic/错误健康记录 |

入口队列最多 1024，维度最多 256，并受 NewWithLimits 的 MaxEvents/MaxDedupKeys/MaxRetainedBytes 缩小；其余离线扫描/spool 预算不适用。聚合不保存完整任务生命周期。writer 最多一个在写和一个待写快照，新请求合并；磁盘阻塞不产生替代 worker。有限指标分别说明 received、task_finished/task_failed 信号，不冒充 DB 的独立任务数。Status 只读取原子健康计数，不等待消费者或 writer；archive_dropped 独立记录归档丢弃，不与报告入口丢弃混算。目录游标和句柄由 writer 独占并关闭，有限续扫避免无关项持续遮挡后续过期目录。

Start 在 enabled 时启动固定消费者和 writer，interval 仅决定周期触发。Close(ctx) 关闭入口、请求末次输出并在 deadline 内等待；App 与归档共用最长 2 秒期限。超时不等待任意文件 I/O，不阻止业务资源按自己的真实排空状态关闭；同一实例停止后不可重启。Stop 是自带 2 秒预算的便利入口。在线固定 snapshot / unknown，source=signals，不据缺失池数推断空闲；report.mode/timeout/thresholds 仅服务离线分析。

`aggregate_boundary_test.go` 验证午夜/同日乱序窗口及维度不混算，`writer_boundary_test.go` 验证目录游标 EOF/退出的实际句柄释放。`runner_test.go` 覆盖洪水丢弃、维度预算、阻塞 writer 的退出期限与不增加 worker、panic、并发 Emit/Close、输入分离和来源口径。`report_resources_test.go` 验证 App 预算装配；App 的 report_* 与 observation_test 验证开关、周期/退出和归档并列接线。组件单测不证明物理零影响或生产长稳。

## 5. 熔断检测与持久化

<a id="breaker"></a>

### 5.1 Breaker：有界终态检测和恢复值

`internal/breaker` 以终态任务轮次 `Outcome` 为输入，输出 `Decision`、证据与检查点；它不处理单次 HTTP 尝试，不拥有 worker 闸门、暂停表或恢复调度。公开可配置策略契约在 `pkg/breakerpolicy`，config 不反向依赖这个实现包。

| 文件 | 职责、入口与边界 |
| --- | --- |
| `config.go` | Config、EffectiveConfig、Validate；统计窗口和启用阈值校验 |
| `policy.go` | Policy 别名以及 EffectivePolicy/ValidatePolicy 委派，复用公开策略契约 |
| `detector.go` | Outcome/Group/Decision、Detector；按任务轮次去重、窗口/epoch 处理、快/混合/硬阈值与容量保护，Snapshot/Evidence |
| `recovery.go` | Recovery、PendingFailure、StageCheck 等控制值；Detector 的 Checkpoint/Restore 及原时间/失败身份恢复 |

`Detector.mu` 保护失败/成功样本、epoch、时间边界、容量溢出。失败记录容量耗尽进入保护，不静默驱逐失败造成少计；成功样本单独有界，不抵消失败。Evidence 从计数所用记录投影，不维护第二份证据缓存；调用方若需组合多个快照，仍须持有自己的结果/准入协调锁。

Recovery 是由 Engine 的熔断协调锁保护的控制值，不属于 Detector 的暂停状态；显示截断/脱敏不能改变恢复身份、覆盖或准入。Checkpoint 复制精确失败轮次和原始时间、去掉错误展示字段，不保留成功证明；Restore 重置 epoch 后恢复仍有效失败，配置容量缩小时不会悄悄丢掉已知失败。Engine 负责全站恢复、重新放行、结果持久确认和何时清零，详见 [Engine 分册](ENGINE_STRUCTURE.md)。

包只依赖标准库和 `pkg/breakerpolicy`；没有 DB、后台循环或 Stop。`config_test.go` 验证参数和极短窗口；`detector_test.go` 验证轮次去重、独立任务、窗口/epoch、容量保护与各规则；`recovery_test.go` 验证恢复身份/原到期时间和缩小容量；`purity_test.go::TestBreakerStaysPure` 防止重新导入 workerpool 或 core 监控 DTO。纯算法测试不证明 Engine 暂停和存储事务。

<a id="breakerstore"></a>

### 5.2 Breakerstore：控制快照与追加历史

`internal/breakerstore` 负责 GORM 持久化，把恢复控制数据、检测窗口和展示报告配成同一 `Snapshot` 保存，并把待确认历史追加到同一事务。展示报告用于查看；恢复准入必须依赖 Control/Window，不能从截断后的报告反推。

| 文件 | 职责、入口与边界 |
| --- | --- |
| `snapshot.go` | Snapshot；Clean 复制控制/窗口/历史并清理展示文本；ValidateCurrent 拒绝缺失控制版本或精确窗口的格式 |
| `store.go` | Store/New、Save 的行锁与 revision 约束、LoadRuntime、List、解码时元数据匹配和容量限制 |
| `history.go` | appendHistory 在当前事务内追加；HistoryForWindow 返回窗口前仍活动的起始状态和窗口内转换；HistoryText 渲染单条历史 |

Store 只持有借用的 `*gorm.DB`，没有内存状态锁或独立生命周期。`Save(ctx)` 在事务中锁定原记录，拒绝较旧/冲突 revision；相同 revision 且规范化 JSON 内容相同允许幂等保存。控制更新和历史追加一起提交或回滚，调用方负责可见重试，不在 Store 内启动后台重放。

快照有 16 MiB payload 上限，恢复加载有数量预算；LoadRuntime 包含活动记录及每站最新关闭记录，以保留人工解除后的失败窗口。多条活动记录交由 Engine 判断，不在此静默选一条。历史按事件/sequence 去重，查询不拼接可变的最新报告；展示时间线的 128 条界限不得裁剪尚未确认的历史写入。缺少控制版本或精确窗口的快照明确拒绝，不从展示数据推断控制状态。

允许依赖 `internal/breaker` 控制值、`models`、`pkg/incident`、GORM；不拥有 Engine、worker、HTTP 或共享 DB 的关闭责任。操作 context 传入 GORM，宿主等待调用者返回后再关闭 DB。

`store_test.go` 包含元数据不一致拒绝，以及 `TestBreakerStoreMySQLLifecycleRestartAndRevision` 的真实 MySQL 生命周期/重启/revision 回归；`history_test.go` 包含 `TestBreakerHistoryMySQLAppendRetryAndHistoricalDay` 和 `TestBreakerHistoryMySQLFailureRollsBackControl`，验证历史重试、历史日期与回滚。是否运行真实库用例取决于测试环境，不能只凭文件存在声称事务已现场验证。

## 6. 数据库、任务存储与测试适配

<a id="database"></a>

### 6.1 Database：连接、显式迁移与通用写入辅助

`internal/database` 是框架 MySQL/GORM 适配层：建立连接与连接池、列出框架模型、执行显式迁移、查询 schema 和提供通用 Upsert。业务模型从宿主显式传入，不在这里登记真实站点或业务数据库配置。

| 文件 | 职责、入口与边界 |
| --- | --- |
| `db.go` | NewDB 仅接受 mysql；按配置设置连接池及 SQL 日志级别/参数隐藏；AutoMigrate 依次处理模型并传播错误 |
| `migrate.go` | Model、FrameworkModels、AllFrameworkModels、Migrate；迁移和启动检查共享模型目录，可选功能按配置选择，额外模型由宿主传入 |
| `schema_version.go` | CheckTaskSchema 在启动/显式迁移前只读验证当前 v4 任务身份列、旧索引及数据；拒绝旧格式，不做回填；身份数据只查首个非法行，正常库仍需全表检查 |
| `schema_check.go` | ExistingTables / HasIndexNamed 提供启动提示与迁移判断 |
| `upsert.go` | Upsert 构造冲突更新，必要时按冲突列回读主键；columnList/backfillPrimaryKey 处理 GORM 列元数据 |

NewDB 成功后将 GORM/底层 sql.DB 生命周期交给 App/宿主；本包不启动 goroutine、不关闭调用方传入的数据库，也没有共享可变目录缓存。GORM/数据库拥有连接池及事务，局部 schema 解析缓存只用于当前 Upsert。

普通启动只检查，不自动迁移。Migrate 写入前拒绝非 v4 任务身份格式，随后同步当前框架/额外模型；不转换旧任务、不删除旧索引或业务数据。

`AllFrameworkModels` 是包括关闭功能的只读检查目录；实际迁移用 `FrameworkModels`。ExistingTables 查询当前库的基础表集合，调用方据需要核对所关注名称；HasIndexNamed 才按具体表/索引过滤。Upsert 不拥有业务事务，调用方应传入适当的 DB/transaction/context；写入成功后的回读错误也必须传播。

依赖 `config`、`core`、`models`、账号/登录模型目录和 GORM/MySQL，允许在适配层引用模型，不让运行控制算法反向依赖本包装配。使用手册见 [CLI](../CLI.md)、[数据落地指南](../DATA_LANDING_GUIDE.md)；迁移职责以本节和现有显式命令为准。

测试：`db_test.go` 验证日志配置和模型开关；`taskflow_models_test.go` 验证任务流可选模型；`model_comment_test.go` 检查模型列注释；`schema_check_test.go` 验证查询、错误传播与 nil 保护；`upsert_test.go` 验证插入/冲突回填、写入和回读失败、参数错误、连接失败及迁移错误。`schema_version_test.go` 验证旧格式拒绝、只读和首个非法行查询边界；`v4_mysql_test.go` 通过 PAPA_V4_TEST_DSN 在独占随机测试库验证新库/重复迁移、旧表拒绝、任务状态 CAS，额外设置 PAPA_V4_SCALE_TEST=1 测量 1 万/10 万/100 万行启动校验；建库失败不接管，结束后核验删库。fake/SQL 断言不能代替这些真实 MySQL 测试，Engine 的身份与补偿真库回归补充执行层验证。

<a id="taskstore"></a>

### 6.2 Taskstore：任务状态转移的 SQL 语义

`internal/taskstore` 用明确函数收口任务认领、终态确认、释放、失败重投和轮询推进，接收 `*gorm.DB`、任务 ID、观察到的版本和值更新，返回 error 或带 Error/RowsAffected 的 GORM 结果。它不决定任务何时调度，不持有 Engine，不实现业务 handler。

| 文件 | 职责、入口与边界 |
| --- | --- |
| `doc.go` | 任务持久化语义包契约 |
| `pending.go` | PendingWindow、LoadSchedulingBatch、CountPending；库时钟/补偿/keyset 查询、批量回灌读取及关停计数；不拥有扫描 goroutine、游标或内存成员 |
| `store.go` | RunVersion / ErrChanged；CompensationScope、ClaimScope、RetryScope、TerminalScope、RepeatScope；Claim、Complete、Release、RetryTask、RetryFailed、AdvanceRepeat，以及内部 Insert/IncrementRetry |

没有包级可变状态、锁、后台循环或 Stop。数据库行的 status、repeat、reprocess 和 compensation_version 构成操作条件；调用方提供带 context/事务的 DB，并检查错误与命中行数。不能由 admin、治理队列或 Engine 的另一个入口重新实现一套不同的重投版本规则。

Claim 只认领到期、满足补偿准入、尚 pending 且不属于任务流的行；任务流自己的行锁/事务由 Engine 的 taskFlowService 协调。Complete 用 status=processing 与准确执行轮次更新，未命中时回读相同轮次/终态以确认丢失的 UPDATE 应答；不能因此重复追加结果或覆盖新轮次。RetryTask 固定操作者观察到的状态和版本，RetryFailed/AdvanceRepeat 用补偿版本避免并发领取同一条；重投与轮询分别推进自己的代次。

Insert 供提交持久化使用；IncrementRetry 将错误交给执行协调器处理。已删除 Task 上的旧状态/重试方法；终态写入继续通过版本条件保护。

依赖仅 `core`、`models`、GORM 和标准库，不导入 HTTP/admin/App/Engine。`store_test.go` 的 `TestClaimAndReleaseFences`、`TestRetryAndRepeatOwnGenerationTransitions`、`TestTerminalScopeFencesExactRound` 验证 SQL 条件与代次表达式；确认丢失、并发执行和实际持久化行为还由 Engine 的集成/真实库测试负责。

`pending_test.go` 用 DryRun 验证生产查询的库时钟、补偿、keyset 和限额谓词；实际投递及原子性由 Engine 隔离 MySQL 回归验证。

<a id="gormfake"></a>

### 6.3 Gormfake：仓内测试用数据库驱动

`internal/gormfake/gormfake.go` 是本包唯一文件。它提供 `New`、`Open`、`MustOpen`，将配置的行、计数、RowsAffected、LastInsertId 和错误包装成 GORM/MySQL 方言的 database/sql 连接；`Written` 提供归一化语句与参数供断言。包位于 internal 是为了限定仓内测试用途，不能用作业务存储实现。

`DB.mu` 保护 Set* 配置、错误注入和语句记录。`SetRow` 接收的 map 仍是调用方提供的引用，测试应通过 Set* 接口更新，不并发改底层 map。Open 创建 sql.DB，不连接真实 MySQL，关闭自动 ping 和默认事务；测试拥有返回连接并负责清理。

QueryContext 仅区分 count 和固定行，不解析 WHERE；ExecContext 返回配置的影响行数。默认不支持事务，`EnableTransactionStatements` 只让 Begin/Commit/Rollback 调用成立，不模拟回滚、隔离或持久化；方法也不模拟阻塞查询的 context 取消。真实 SQL 方言、事务、取消传播和数据竞争必须用相应测试设施验证。

本包依赖标准库和 GORM/MySQL，没有 App/Engine 状态，没有自有 goroutine/Stop，也没有同目录 `_test.go`。消费者测试遍布 admin、App 等包，不能把“假驱动返回成功”当作真实数据库操作已经完成；Engine 的专用假库另有字段和行为模型，归 Engine 分册维护。

## 7. 通用执行与统计设施

<a id="workerpool"></a>

### 7.1 Workerpool：双队列、有界准入与真实排空结果

`internal/workerpool` 是泛型执行池，只要求任务满足 `Tasker.Unique()`，handler 接收 context 并返回 error。它不懂任务表、站点、账号或熔断原因；数据库溢出、站点暂停状态及执行持久化由 Engine 通过明确接口协调。

| 文件 | 职责、入口与边界 |
| --- | --- |
| `task.go` | Tasker 和 TaskHandler 的最小任务/执行契约 |
| `activity.go` | ActivityType、Activity；任务开始/结束、worker 停止的观测事件 |
| `workerpool.go` | WorkerPool、ValidateConfig、NewWorkerPool、Start、Gate/SetGate、Submit/SubmitUrgent、Stop、Stats、QueueDepths、Activities/Errors；handler panic 转错误后继续工作 |

池拥有常规/加急队列、stopCh、worker WaitGroup、stopOnce 和首次 Stop 的结果；`mu` 保护 stopped，并使“检查准入+发送”与停止写锁互斥。submitted/completed/failed 用原子计数，活动队列只用于观测。Gate 必须在 Start 前安装，Start 按宿主生命周期调用；它没有重复 Start 的 once 保护。

常规提交在高水位返回 ErrQueueFull，由上层保存未入队责任；加急队列优先消费，达到其高水位则退回常规队列。两条队列都不 close；Stop 持写锁关准入并关闭 stopCh，然后 worker 排尽已接收任务，停机排空不再受运行期 Gate 阻挡。context 传给 handler，但 worker 不因 context 已取消就丢掉排队存量，宿主仍必须调用 Stop。

Stop 等待给定 timeout 后按 submitted-completed-failed 返回 drained 与尚未完成数量，后者包含排队任务。重复 Stop 返回首次结果，不是重新采样后的新结论；超时不会终止 Go goroutine 或业务 handler，外层不能在仍有任务时关闭其 DB/浏览器依赖。暂停/取消后的任务是否执行业务由 Engine handler 的准入和持久化规则共同决定，不能仅看池层排空理解业务结果。

仅依赖标准库与 `internal/msgqueue`。`config_test.go` 验证非法尺寸；`workerpool_test.go` 验证水位、加急优先/回退、Submit 与 Stop 并发、panic 后 worker 存活、取消后双队列排空、暂停 Gate、超时与幂等结果。代表测试为 `TestStopDrainsBothQueuesEvenWhenCtxCancelled`、`TestStopReportsInFlightOnTimeout`、`TestStopWhilePausedStillDrains`。

<a id="msgqueue"></a>

### 7.2 Msgqueue：可丢弃的观测消息

`internal/msgqueue/queue.go` 是本包唯一生产文件。`MsgQueue[T]` 拥有固定容量的活动与错误 channel；NewMsgQueue 将非正容量调整为 1，SendActivity/SendError 始终非阻塞。活动通道满时尽力向错误通道写丢弃提示；错误通道也满时直接放弃，不能把它当作可靠任务投递或持久化确认通道。

并发同步来自 channel，本包没有额外锁、goroutine、取消 context 或 Close API。Errors/Activities 只返回只读通道；消费者随自己的 context/Stop 退出，不要求生产者关通道来唤醒。依赖只有标准库 fmt，不反向依赖 workerpool 或 Engine。`queue_test.go` 验证容量、错误发送不阻塞、活动丢弃提示和返回的真实通道。

<a id="track"></a>

### 7.3 Track：活动统计快照

`internal/track/stats.go` 是本包唯一生产文件，定义 WorkerStat、GlobalStats 和 StatsQueue。`NewStatsQueue` 绑定借用的 workerpool，Start 启动消费者，只在 ActivityTaskEnd 时累计每 worker/全局任务数、失败数、总/最小/最大/平均耗时；它不更改池的调度或计数。

`StatsQueue.mu` 保护 workerStats map 和 globalStats，所有 Get* 查询都返回值副本或新 map。stopCh/stopOnce 提供独立且幂等的停止信号，便于池空闲回收时停止消费者；Start 也监听宿主 context。Stop 只发送退出信号，没有 done/Wait 接口，不能描述为等待已完全排空，也不会关闭借用的 workerpool。宿主负责避免重复 Start。

依赖只含标准库和 `internal/workerpool`。活动来自可能丢弃消息的 msgqueue，因此这些是观测统计；执行池自身的原子计数和 Engine 的持久化状态才承担各自完成判定，不能用活动统计替代它们。`stats_test.go` 验证单 worker/全局累计、未知 worker 查询及真实池活动消费。

## 8. 维护时的跨包核对

新增状态或后台循环前，明确是哪个组件持锁、谁负责拒绝新操作、如何取消、如何等待和超时后保留哪些依赖。当前几种停止契约有意不同：App/诊断汇总真实等待结果，reportrunner 仅在调用方 deadline 内等自有 writer，workerpool 返回首次停止时的任务完成快照，track 只发停止信号，msgqueue 没有停止入口。不能用同一个“都已关闭”描述抹平这些差异。

新增持久化逻辑先找语义拥有者：任务转移归 taskstore；熔断控制快照及历史事务归 breakerstore；策略 CAS/核对/发布归 runtimecontrol，具体配置存储适配归 admin/configstore；当前 schema 建表只走显式 database.Migrate。App 只组织时序，不复制这些算法。

本册的文件数与逐文件索引用于发现漏写职责，不是机械拆包指标。代码变化需同步对应章节与[开发手册](../DEVELOPMENT.md)，公开扩展以当前 v4 契约为准。架构守卫、合成 fixture、假库、浏览器和真实 MySQL 测试各有证明范围；本文是源码职责说明，不宣称本轮执行了这些测试，也不把目录说明当作无缺陷证明。


## tracestore：Trace 文件持久化

| 文件 | 职责 |
| --- | --- |
| `store.go` | Options/Batch、版本化记录、Store 状态与可取消串行门、根目录约束 |
| `files.go` | 首次有限目录盘点、容量核算、暂存/同步/关闭/重命名发布、受限读取与格式验证 |
| `read.go` | 持久发布顺序选择最新执行、合并尝试、末 500 步截断 |
| `cleanup.go` | 按记录时间清理，逐文件释放串行门；汇总损坏错误并继续清理其它记录 |
| `store_test.go` | 发布、取消、配额、重启顺序、损坏、清理和路径边界回归 |

仅依赖标准库及 core Trace 值类型，不依赖 Engine/App/GORM。Store 独占专用目录，拥有文件索引、总字节数及发布序号；它的串行门协调写入/查询/清理，不持有 Engine 调度锁。首次盘点仅扫描文件名和尺寸，后续写入不重新扫盘；实际步骤按文件大小限制读取。

路径为分片/task-ID/identity-hash/execution-digest/发布序号.json。标准 identity_hash 直接作为目录名；非标准标识散列后使用。版本化 JSON 保存原执行 UUID、任务身份、尝试号、记录时间和截断状态。任务 ID 查询不依赖数据库或第二套索引。崩溃遗留 `.pending` 不展示，保守保留并计入预算；不自动递归删除未知目录。

App 只装配 Engine 查询回调。常规 FrameworkModels/Migrate/启动探测均排除 Trace；旧 TaskTrace 模型及 CheckTraceSchema 声明已经删除。

NewApp 在创建 Engine 前调用 database.CheckTaskSchema；拒绝旧 schema 或校验失败时关闭新建 DB 连接并返回错误。缺少尚未创建的任务表允许后续显式迁移；旧库不靠日志提示继续启动。
