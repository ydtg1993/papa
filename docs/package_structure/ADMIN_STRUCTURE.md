# Admin 目录结构与职责细则

> 面向：维护 Papa 后台、增加后台功能和审查包边界的开发者。
> 总入口：[Papa 目录说明书](README.md)。
> 配套：[总体架构规则](../ARCHITECTURE.md)、[开发手册](../DEVELOPMENT.md)、[后台使用与 API](../MONITOR.md)。
> 本文按当前源码说明整个 `admin/`，作为 Papa 总体目录细则的 admin 分册。目录树省略测试文件；文件职责和测试归属在后文说明。

## 1. 总体定位

`admin/` 是框架的管理展示与操作入口：把框架能力适配成 HTTP、表格和页面，提供后台访问控制、配置存储适配、审计与系统采样。任务执行、调度、账号登录、浏览器诊断算法及报告生成仍由各自领域组件完成。

目录按功能归属组织。一个功能包可以同时包含 Go、JS、CSS 和 HTML，但必须通过 `static/`、`views/` 区分资源用途；Go 文件负责 HTTP/适配，前端文件负责该功能的交互。包边界看状态和行为归属，不以语言种类划分整个 admin。

- `server` 是兼容入口和装配层，不拥有功能页面源码或功能内部状态。
- `shell` 是页面外壳与资源组合层，不承担日志、报告、令牌等功能规则。
- 功能包拥有请求解析、响应映射、功能视图及控制器私有状态。
- 功能 Page() 提供默认页面元数据和资源；共享的 `admin/page.Descriptor` 仅是值契约，宿主决定是否挂载和排列顺序。
- 数据源、存储、鉴权基础能力按独立契约复用，不通过全局 `utils` 汇集。
- 宿主负责 HTTP 服务启动/关停、领域组件创建与注入；后台功能不得接收整个 App、Engine 或 Monitor 来任意取依赖。

## 2. 目录全览

```text
admin/
├── server/                  # Monitor 公开兼容入口、路由与组件装配
│   └── testdata/            # Node 行为测试、DOM 替身及合成 fixture
├── shell/                   # 页面外壳、资源组合与前端公共运行环境
│   ├── static/              # 会话、导航、请求、UI、主题等公共资源
│   │   └── fonts/           # 内嵌字体
│   └── views/               # template.html、pages.html、tables.html
├── page/                    # 页面描述的纯值契约，不依赖框架实现
├── access/                  # 来源白名单、请求鉴权门、发布及可排空重载器
├── auth/                    # 访问令牌验证、操作人上下文、计数缓存
├── tokenadmin/              # 访问令牌管理 API 与存储实现
│   ├── static/              # controller.js、secret.css
│   └── views/               # tokens.html
├── settings/                # 设置概览、显式关停
│   ├── static/              # controller.js
│   └── views/               # settings.html
├── monitoring/              # 监控快照与运行概览
│   ├── static/              # controller、workbench、visuals、visual-state 等
│   └── views/               # dashboard.html
├── queues/                  # 治理队列手动操作与展示
│   ├── static/              # controller、queue-governance、table 样式
│   └── views/               # queues.html
├── breakers/                # 熔断状态、控制及分析展示
│   └── static/              # controller.js、analysis.css、alerts.css
├── taskflow/                # 任务流策略 HTTP 与编辑器
│   └── static/              # taskflow.js、taskflow.css
├── tasktrace/               # 任务追踪与归档页下载
│   └── static/              # trace.js、trace.css
├── logs/                    # 日志列表、尾部读取、下载
│   ├── static/              # controller.js、logs.css
│   └── views/               # logs.html
├── reports/                 # 已生成报告的浏览与下载
│   ├── static/              # controller.js、reports.css
│   └── views/               # obs.html
├── accounts/                # 账号接入及受控远程操作 HTTP
│   └── static/              # accounts.js
├── accountpools/            # 账号池管理、任务补偿 HTTP
│   └── static/              # account_pools.js
├── runtimecontrol/          # 运行策略、历史、代理健康 HTTP 与存储适配
│   └── static/              # runtime_control.js
├── diagnostics/             # 浏览器与身份诊断 HTTP
│   └── static/              # rod_diagnostics.js
├── configstore/             # 注册式配置存储、缓存、版本与历史
├── gormsource/              # GORM → oao.Source 通用查询适配
├── frameworkdata/           # 框架表只读目录、脱敏与可用性展示
├── tasksource/              # 任务表格与任务操作适配
│   └── views/               # 内置任务工作台 workspace_header.html
├── sitesource/              # 站点快照 → 表格适配
├── oplog/                   # 操作审计记录及审计表格
├── sysinfo/                 # 系统/进程/目录采样
└── internal/
    └── httpjson/            # 仅 admin 内复用的 JSON 传输约定
```

`static/`、`views/` 和 `testdata/` 是资源目录，不是 Go 子包。并非所有功能都需要独立 HTML：部分页面由 JS 构造或挂入外壳已有容器，不应为目录对称而创建空文件。

## 3. 装配与页面外壳

### 3.1 server

| 文件 | 职责 |
| --- | --- |
| `doc.go` | 声明兼容门面、组件装配及集成测试边界 |
| `config.go` | `MonitorConfig`、`MonitorGetter`、Logger 与公开事件类型别名；保持原 API 契约 |
| `monitor.go` | `Monitor`、`NewMonitor`、`SetWhitelist`；持有功能组件并按所需依赖构造 |
| `routes.go` | `Register` 直接绑定功能 handler，并统一包裹鉴权；挂载兼容静态资源 |
| `middleware.go` | 公开 `Auth`、`NoCache`；扩展接口鉴权和静态缓存策略 |
| `assets.go` | 连接 shell 资源及任务流脚本/样式的兼容出口 |
| `extensions.go` | `ExtensionPage` 别名与 `RegisterExtensions` 兼容入口，实际扩展装配交给 shell |
| `visual_assets.go` | 检查所链接 oao 版本是否实际带有可选图表/动效资源，供页面装配选择 |
| `*_test.go`、`testdata/` | 公开入口、整页装配、旧 URL、统一鉴权、Node 与 Chrome 集成回归 |

新增功能时，这里只增加必要的构造参数映射、组件字段和路由挂载。白名单锁、日志游标、报告选择、模板缓存、领域事务都不放入 Monitor。只有承诺的公开 API 保留必要委派；不为旧单元测试保留私有转发函数，也不在 server 复制功能常量。

`TaskFlowScript` / `TaskFlowCSS` 是保留的公开可覆盖资源变量。App 在 Run 前注册任务流页面时继续读取这两个兼容出口；功能包的 `Page()` 提供默认值，不取消宿主已有的覆盖能力。

### 3.2 shell 的 Go 与模板

| 文件 | 职责 |
| --- | --- |
| `assets.go` | 显式列出资源来源与组合顺序，替换模板/CSS 片段，生成兼容资源；缺失片段或重复路径时报错 |
| `resource_fs.go` | 构造后只读的资源文件系统，支持独立读取副本、目录读取和文件定位 |
| `renderer.go` | 页面模板解析与缓存、渲染选项和 HTML 响应 |
| `extensions.go` | 宿主扩展页面元数据快照、脚本和样式挂载 |
| `views/template.html` | 页面骨架、资源引用、功能片段插槽及首帧初始化 |
| `views/pages.html`、`views/tables.html` | 通用扩展页面容器与表格容器 |

### 3.3 shell 的前端文件

| 文件 | 职责 |
| --- | --- |
| `runtime.js` | `MonitorShell` 工厂注册、依赖解析、实例化及 mount/unmount/reset/destroy 生命周期 |
| `session.js` | 公共会话状态及会话代次查询/变更 |
| `requests.js` | 公共请求工具及请求结果有效性协作 |
| `navigation.js`、`navigation-controller.js` | 导航支持逻辑、活动模块与站点选择，通过语义接口通知功能 |
| `login-refresh.js` | 登录后的刷新协调 |
| `table-adapter.js`、`table-links.js` | oao 表格接入、表格导航和关联跳转 |
| `extensions.js` | 前端扩展页面接入 |
| `ui.js`、`download.js` | 通用交互工具与下载协作 |
| `theme.js`、`appearance.js` | 主题及外观行为 |
| `startup.js` | 启动绑定与公共初始化编排 |
| `mo.css`、`layout.css`、`fonts/` | 共享视觉基础、布局、功能 CSS 组合插槽与字体 |

共享会话和导航可以由 shell 拥有；某个报告的选中项、日志请求计数、令牌编辑表单等必须留在功能控制器内。兼容全局函数只用于维持已有调用方式，不得把私有状态全部导出为可写 getter/setter。

### 3.4 server 测试组织

- `helpers_test.go`：Monitor 构造、请求发送、JSON 解码和通用测试替身。
- `access_routes_test.go`、`settings_routes_test.go`、`logs_routes_test.go`、`trace_routes_test.go`、`breaker_routes_test.go`、`queue_routes_test.go`、`monitor_routes_test.go`：通过真实路由验证装配、身份传播和响应契约。
- `page_contract_test.go` 及现有页面/资源测试：验证组合后的 HTML、JS、CSS 和后端响应配合；完整页面 Chrome 与跨控制器 Node 测试留在这里。
- 白名单解析和来源 IP 单元测试在 `access/parsing_test.go`；归档路径单元测试在 `tasktrace/path_test.go`；日志尾部算法测试在 `logs/handler_test.go`。单元测试直接验证所属功能，不借道 Monitor。

## 4. 功能包职责明细

通用文件规则：`doc.go` 描述包边界；`assets.go` 提供嵌入资源及相关默认页面描述，不承载请求或领域规则；`handler.go` 负责 HTTP 适配。独立 `page.go` 也可以提供 Page()；`audit.go` 定义本功能的操作日志值。部分既有包直接在 handler 中嵌入 Script，实际入口以下表为准。

| 包 | Go 文件分工与主要接口 | 前端与状态归属 | 边界：交给谁处理 |
| --- | --- | --- | --- |
| `access` | handler.go：请求身份、白名单校验/保存/快照；reload.go：LoadWhitelist、带 context 的读取与 WhitelistReloader.Start/Cancel/Stop | 拥有白名单判定锁和重载 goroutine/cancel/done；不提供专属页面 | 令牌算法归 auth，令牌增删归 tokenadmin；宿主注入读口及发布回调，依据真实排空结果关闭依赖 |
| `settings` | `handler.go`：`Settings`、`Shutdown`；读取白名单快照与配置状态，确认后调用关停回调 | controller 与 settings 视图负责设置页操作 | 不拥有白名单锁，不执行进程退出流程，不处理运行策略事务 |
| `monitoring` | `handler.go`：`Snapshot`、`StageData`；投影统计信息和实例标识 | controller 管概览；workbench 管工作台；visual-state 管图形状态；visuals 管可视化；sites/stages 样式归此包 | 只读统计，实际执行状态属于 engine；系统采样归 sysinfo |
| `queues` | handler.go：Error/Repeat 和 QueueProcessEvent；audit.go：事件到操作日志值的转换 | controller、queue-governance 与 queues 视图管理队列操作展示；table.css 是队列相关表格样式 | 重投、持久化、并发约束通过运行回调执行；任务表行操作归 tasksource |
| `breakers` | handler.go：暂停/恢复/强制恢复；analysis.go：HTTP 投影；audit.go：动作、作用域和审计文案 | controller 管熔断交互，analysis/alerts 样式管理分析与告警呈现 | 检测器、恢复流程、持久状态归 engine 及对应领域组件，不能在 handler 重写状态机 |
| `taskflow` | handler.go：策略校验/CAS/操作事件；assets.go：Script/CSS 和 Page()；audit.go：操作日志值 | taskflow.js/css 管策略编辑与页面生命周期 | 调度、持久化及版本更新通过注入契约完成 |
| `tasktrace` | `handler.go`：`Trace`、`Page`、`ResolveArchivedFile`；追踪查询及归档路径检查 | trace.js/css 管任务追踪抽屉与交互 | 追踪记录生成归执行链；这里只读取并安全下载 |
| `logs` | `handler.go`：`List`、`Tail`、`Download`；文件名、读取范围、ZIP 与下载边界 | controller、logs.html/css 拥有文件选择、请求代次和重置逻辑 | 日志生产和轮转归日志组件；只注入日志目录 |
| `reports` | `handler.go`：`List`、`File`；报告目录枚举、摘要读取、产物大小/路径限制 | controller、obs.html、reports.css 拥有报告/文件选择和请求代次 | 报告生成与调度归 obs 和 reportrunner；不在 GET 中生成报告 |
| `tokenadmin` | `api.go`：`API.Register` 与操作事件；`store.go`：Store 契约和 DBStore；`mem.go`：MemStore；支持创建、启停、删除、轮换 | controller、tokens.html、secret.css 管令牌页面与一次性明文展示 | 访问令牌与爬虫站点账号不同；验证基础能力归 auth |
| `accounts` | handler.go：Service/Accounts/Remote 和 Script；page.go：默认页面描述 | accounts.js 管账号接入和远程操作 UI | 登录过程、候选身份、凭据与远程资源生命周期归 `pkg/account/onboarding` 等服务 |
| `accountpools` | handler.go：依赖契约/Script；pools.go：池操作；compensation.go：补偿 HTTP；page.go：默认页面描述 | account_pools.js 管池页面、补偿预览与结果展示 | 池状态归 `pkg/account.Pools`；策略引用协调归运行策略服务；任务补偿执行归 engine |
| `runtimecontrol` | handler.go：策略/历史/远程 HTTP；proxy_health.go：代理健康；store.go：StoreAdapter；types.go：契约别名；page.go：默认页面描述 | runtime_control.js 管运行治理页面和共享前端能力 | 保存事务、策略生效及回滚协调归 `internal/runtimecontrol`；持久化适配使用 configstore |
| `diagnostics` | identity.go：身份 HTTP、服务契约和 View 站点导航适配；rod.go：Rod HTTP；assets.go：Script；page.go：默认页面描述 | rod_diagnostics.js 管诊断页面 | 诊断执行、取消和浏览器资源清理由 `internal/browserdiagnostics` 负责 |

独立 handler 本身并不等于已安装访问控制：宿主挂载 accounts、accountpools、runtimecontrol、diagnostics 等接口时仍需统一包裹鉴权。`Monitor.Auth` 对所有路径执行令牌校验；Monitor 自有页面的 Wrap 为加载登录壳保留页面路径规则，不能拿该豁免套用到业务接口。

App 的 `admin_routes.go` 直接挂载功能 handler 方法并应用统一 guard。`admin_audit.go` 负责操作者/时间/Recorder 接线；功能包决定 action、table、note 等语义，不能把这些分支重新堆回 HTTP 服务构造函数。身份 View 的站点导航由 diagnostics.Handler 补充，账号池通过同一适配复用。

## 5. 基础能力与数据适配包

| 包 | 文件分工 | 负责什么 / 不负责什么 |
| --- | --- | --- |
| `page` | descriptor.go：Key、Label、Group、Section、Script、CSS | 共享纯值，不导入 App/server/shell 或其它框架实现，不拥有页面状态 |
| `auth` | `auth.go`：生成/哈希/提取/验证令牌，Verifier/Confirmer，操作人 context；`token_counter.go`：计数缓存、失效和相关验证适配 | 验证“谁在操作”；不管理页面、白名单或账号登录过程。缺少有效令牌不能自动变成匿名管理权限 |
| `configstore` | configstore.go：Spec、读写、LoadContext/LoadStringsContext；cache.go：JSON 缓存；string_list_cache.go：context 列表读口、TTL 与失效代次；sites.go：站点投影；versioned.go/history.go：版本与历史 | context 传到 SQL；失效期间的旧读不能回填缓存。保存配置并保证版本/历史事务，功能语义与运行生效归功能/领域服务 |
| `gormsource` | `gormsource.go`：Config、Source、List，过滤/排序/分页查询转换 | 把已声明的表格列和查询转成 GORM 查询；不决定业务表目录，不执行任务状态变更 |
| `frameworkdata` | `tables.go`：框架表目录；`source.go`：只读数据源；`sanitize.go`：敏感字段处理；`status.go`：表状态接口挂载 | 只展示明确列入目录的 Papa 表；不按前缀自动发现业务表，不负责迁移 |
| `tasksource` | table.go：TaskActions 与通用 Table；workspace.go / views/workspace_header.html：内置 WorkspaceTable 的 Label/Header | 保留旧 Table 默认值及动作能力；展示归本包，不另写认领、重投、补偿状态机 |
| `sitesource` | `table.go`：站点快照内存数据源与表格定义 | 展示传入的站点快照；不创建引擎站点或执行抓取 |
| `oplog` | `oplog.go`：Recorder、Event、Record/RecordEvent、Close、审计 Table | 拥有审计缓冲/写入与关闭排空；宿主负责创建和关闭。记录操作结果，不决定操作是否获准或成功 |
| `sysinfo` | `sysinfo.go`：Collector、Start、Snapshot、DirSize | 拥有采样周期与最新系统快照，通过 context 结束；不拥有 HTTP 路由或应用生命周期 |
| `internal/httpjson` | `json.go`：Strict、Write；`doc.go`：包用途 | admin 内部的严格 JSON 解码和响应格式辅助；请求体大小限制仍由调用方负责，不接收功能状态 |

## 6. 依赖与状态所有权

下面表示职责调用方向，不是逐条 Go import 清单：

```text
宿主 internal/app / 公开应用入口
  ├─ 创建领域服务、存储和采样/审计组件
  ├─ admin/server ──> access、settings、monitoring、queues、breakers 等
  │                └─ shell ──> 功能包的嵌入资源
  ├─ 挂载 accounts / accountpools / runtimecontrol / diagnostics
  └─ 注册表格与数据源

功能 handler ──> 注入的服务/回调 ──> 领域执行与持久化
功能前端     ──> shell 公共能力 + 明确的功能查询/操作接口
```

1. 功能包不得反向导入 `admin/server`。资源由 shell 组合，功能包也不应为了获取自己的资源而反向依赖 shell。
2. 并非功能包之间完全零依赖：例如 monitoring 使用 breakers 的状态整理，queues/settings 使用 access 的辅助能力。复用必须指向明确能力，不能通过对方读取整个运行对象。
3. `runtimecontrol.Runtime`、`accountpools.Runtime` 当前确实提供多个运行查询/操作，其中一些返回具体框架类型；它们是现有适配契约，不代表允许继续扩大到全部引擎功能。
4. 状态、锁、版本和生命周期必须同属明确拥有者。HTTP 层不得另存一份领域状态，再靠定时同步“保持一致”。
5. 前端 reset/unmount 后，之前发出的 HTTP、正文读取及错误回调不得更新新视图。destroy 还应释放注册的监听和资源；使用 lifecycle 的有效性检查和清理接口。
6. 公开兼容 API、JSON 响应、资源 URL 与私有实现分开维护。改包位置不能顺带改变鉴权、错误码、版本冲突和关停行为。

白名单重载不保留完整 App 或 Monitor，只接受 ContextStringListReader 和发布回调。普通读失败仍记错并按不限制处理；取消/超时不发布空白名单。Stop 使用宿主给的 context 返回真实排空结果，读端不响应取消时返回 false。HTTP 服务自身及 handler 计数由 App 管理，包含 hijack，和重载结果共同参与共享 DB/浏览器关闭判断，详见 [Internal 分册的 App 章节](INTERNAL_STRUCTURE.md#app)。

## 7. 源文件与浏览器资源地址

| 浏览器入口/产物 | 源码所有者与组合方式 |
| --- | --- |
| `/monitor` HTML | shell 外壳 + monitoring/settings/queues/logs/reports/tokenadmin 等功能视图片段 |
| `/static/mo.js` | shell 按显式顺序组合 runtime、公共控制器及功能控制器；源码中没有对应的万能 mo.js |
| `/static/mo.css` | shell 的共享 CSS 与功能 CSS 插槽组合，功能样式应修改其所属包 |
| 既有 workbench/visuals/queue-governance/trace 等静态 URL | shell 显式别名映射到所属包，不按同名文件自动猜测 |
| `/static/features/<owner>/<file>` | 参与 shell 组合的功能资源的明确归属路径 |
| accounts/accountpools/runtimecontrol/diagnostics 的 Script | 功能包自行 embed，并通过 Page() 提供默认描述；App 保留原先追加顺序和页面 key，并非所有脚本都进 mo.js |

`go:embed` 解决部署资源随二进制分发的问题，不决定功能归属。增加资源时同时核对 embed 范围、shell 组合/宿主注册、模板引用和兼容 URL。不能把资源复制回 server 来解决引用路径问题。

公共导航与外观由 shell 维护，图表观测模型由 monitoring 维护：navigation.js 统一四类归属，空/未知 Section 回退 data，Group 只在分类内分组；template.html 组合功能根并在 CSS 加载前恢复合法外观值。theme.js、appearance.js 更新根属性、设置控件和浏览器本地偏好，不重新挂载业务页；显示选择以当前 DOM 状态为准，存储写入失败不能让旧值反复覆盖临时选择。

layout.css 负责共享导航、响应式与外观覆盖，在基础样式之后、业务 custom.css 之前加载；oao 通用图表、motion 与表格生命周期实现仍属于独立 oao 模块。monitoring 的 visual-state.js 保存有界观测历史，按真实时间去重、断线和重置；visuals.js/css 根据折叠和页面可见性管理图表，恢复可见仅重绘已接收快照；workbench.js 限制关注项展示并保留队列/暂停详情入口。

数据采样与视图状态分开：sysinfo 在锁外完成 CPU/内存/磁盘采样，以短锁发布可用性、时间和目录快照，页面轮询不驱动系统采样。frameworkdata 从 GORM schema 获取 JSON 列清单交给只读数据源，递归脱敏不能只依赖 `_json` 后缀，任务流的 policy 列也属于该边界。各项的 Node、系统采样和真实页面验证方式见开发手册；目录分工本身不代替这些验证。

## 8. 新增与修改功能的放置规则

| 需求 | 首选位置 |
| --- | --- |
| 改日志读取、下载或日志选择交互 | `logs/` |
| 给已生成报告增加摘要展示 | `reports/`；若改变报告生成内容则改报告领域组件 |
| 增加运行策略参数与保存规则 | `runtimecontrol/` 做 HTTP/UI；`internal/runtimecontrol` 做领域规则；按需扩展 configstore |
| 增加任务表按钮 | `tasksource/` 声明动作，领域执行通过 TaskActions；追踪详情交给 tasktrace |
| 新增访问令牌管理操作 | `tokenadmin/`；验证算法变化才进入 auth |
| 公共会话、导航、表格容器能力 | `shell/`；先确认至少是公共职责，而非单一页面的便利函数 |
| 独立的新框架管理功能 | 新增语义明确的 `admin/<feature>/`，自行拥有 handler、资源与测试，server/宿主仅负责接线 |
| 某个真实网站的专属后台 | 独立业务模块，通过公开扩展 API 接入，不放进 Papa admin |

新增包或重要文件时必须说明：输入依赖、输出接口、状态所有者、取消/关停方式，以及哪些领域规则不属于本包。先按职责拆文件；只有出现独立契约和生命周期时再拆 Go 子包，不按文件数量机械拆分。

## 9. 测试与文档维护

| 测试位置 | 验证目标 |
| --- | --- |
| 各功能包 `*_test.go` | 请求校验、错误映射、存储适配、文件边界、独立状态与功能行为 |
| access 的 reload_lifecycle_test.go / configstore 的 string_list_context_test.go、context_sql_test.go | 重载取消/排空、迟到结果、TTL 失效并发、GORM 到实际 database/sql 调用的 context 传播 |
| tasksource/workspace_test.go / accounts/frontend_test.go / diagnostics/identity_test.go | 默认工作台展示与旧 Table 兼容、账号脚本文本契约、身份导航视图适配 |
| `shell/*_test.go` | 模板/资源装配、缺失与重复资源、只读 FS、字体 range |
| `server/*_test.go` | 公开兼容入口、统一鉴权、旧 URL、页面装配、Node/Chrome 集成 |
| `server/testdata/` | 测试脚本、辅助 DOM 和合成数据；这里的 JS 是测试输入，不能供生产页面引用 |
| 根目录 `architecture_test.go` | server 不存生产页面资源、功能包不反向依赖 server/App；admin/page 值契约不导入框架实现 |

改动公共资源装配需要验证真实页面，不仅检查 JS 语法；改动异步控制器需要覆盖会话变化、切页、reset/unmount 与迟到结果；改动文件下载需要验证路径限制及响应行为。按改动范围运行功能测试、后台集成测试和必要的完整测试。

本文维护当前目录与长期归属规则；接口字段、使用示例和路由细节维护在 MONITOR 及专项手册；实施过程和测试批次记录放在 `docs/issues/`。各职责分册统一放在 `docs/package_structure/`，并沿用“目录—职责—文件—状态—依赖—测试”的组织方式，避免重复维护第二份接口手册。
