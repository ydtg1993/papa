# 根目录与基础包职责细则

> 面向：维护公开入口、配置、跨包契约、数据库模型及仓库配套文件的开发者。
> 总入口：[Papa 目录说明书](README.md)。规则见 [ARCHITECTURE.md](../ARCHITECTURE.md)，开发命令见 [DEVELOPMENT.md](../DEVELOPMENT.md)。

## 1. 范围与定位

本册覆盖根包 `papa`、`config`、`core`、`models`、`docs` 五个 Go 包，以及仓库级构建、文档和开发工具目录。App 的实现位于 [Internal 分册](INTERNAL_STRUCTURE.md#app)，并不是根包的第二份运行状态；Engine 的任务执行见 [Engine 分册](ENGINE_STRUCTURE.md)。

```text
papa/
├── papa.go                     # 应用/配置/抓取能力公开门面
├── task.go                     # Engine、Task、Fetcher 与任务流类型门面
├── errors.go                   # 错误、告警和通知契约门面
├── architecture_test.go         # 跨包依赖与资源归属守卫
├── papa_test.go                 # 根公开 API 的回归
├── config/                     # YAML 解码、默认值、校验
├── core/                       # 标准库依赖的跨包值与协议
├── models/                     # 框架表、索引、字段与 GORM hook
├── docs/                       # 使用手册、目录说明书、嵌入分发
├── go.mod / go.sum             # 模块路径与依赖
├── Makefile                    # 构建、测试与开发命令
└── LICENSE.txt                 # 项目许可证
```

## 2. 根包 papa：公开门面

| 文件 | 职责 | 实现归属及维护边界 |
| --- | --- | --- |
| `papa.go` | App/Option/Page/Router、声明和配置别名；New、RegisterSite/Sites/CheckSites；HTTP 会话、账号、请求头、URL/身份及下载覆盖入口 | 创建/启停归 internal/app，声明归 sitedecl，能力实现归各 pkg。根包不再维护登记表、锁或第二份配置缓存 |
| `task.go` | Engine、Task、Fetcher、StageConfig、BreakerProbe 和任务流类型别名 | 执行协议仍由 engine 拥有；Fetcher 签名含 Engine，不为追求“叶子包”把强耦合实现移入 core |
| `errors.go` | NoRetry、SiteChallenge、FailureCode、告警事件和 Notifier 的兼容入口 | 错误分类和值协议归 core，通知传输归 pkg/notify；根包不实现重试、熔断或补偿调度 |

类型别名暴露的是原类型，不能通过移动内部文件改变公开方法集。已有函数变量形式的入口也应保留兼容；添加门面不意味着其底层服务可以由根包复制状态。业务模块可以通过公开入口使用框架，不把真实站点声明、模型或固定验收目标加入框架根包。

## 3. config：配置文件边界

| 文件 | 职责 | 不应承担的工作 |
| --- | --- | --- |
| `config.go` | Config 及各配置段；Load 的 YAML/mapstructure 解码；HTTP 超时、SQL 日志等值读取；公开 BreakerPolicy 别名 | 不创建数据库、浏览器或后台服务，不执行运行期策略 CAS |
| `validate.go` | 未知键检测与拼写提示、默认值、配置值域规则；Validate 在启动校验失败时 panic | 不替各能力库的 Go API 构造函数校验所有直接调用，不把真实站点规则加入公共配置 |
| `duration.go` | DurationRange 文本解析、固定/区间判断和区间随机取值 | 不拥有 ticker 或延迟队列，调度时机由使用者决定 |
| `runtime.go` | 显式运行覆盖声明、ProxyHealthPatch.Apply、组合治理/代理配置后的校验 | 不拥有运行策略 revision、发布锁或生效快照；这些归 internal/runtimecontrol 和能力组件 |
| `business.go` | BusinessSection 将 business 子段解码到调用方类型，拒绝该类型未声明的键 | 不理解业务段的含义，不为业务字段补默认值或安排数据库/模型 |

Config 是调用者持有的配置对象，含 map、指针等可变成员，不是自动并发安全的运行状态容器。组件接收所需配置或快照，并自行拥有运行状态。Load 的读取/解码错误与 Validate 的 panic 契约应分别处理；普通启动不因为读到配置就执行迁移。

允许使用 `pkg/breakerpolicy`、`pkg/egress`、`pkg/governance`、`pkg/middleware/proxy` 的声明/校验能力，不能反向依赖 App 或把 internal/breaker 的算法实现引入公开配置。具体配置字典见 [CORE_CONFIG.md](../CORE_CONFIG.md)。

## 4. core：跨包值与协议

core 只依赖标准库。这里可以有值方法、错误包装、上下文传值与纯转换，不持有 Engine、GORM、浏览器池或进程级执行循环。领域专用存储契约不因为“也会跨包”就搬来，例如账号契约仍归 pkg/account/contract。

| 文件 | 职责 | 边界或容易混淆的语义 |
| --- | --- | --- |
| `alert.go` | AlertLevel、TaskError、AlertEvent、Notifier 接口 | 只声明告警值和交付接口，不拥有发送队列或重试 |
| `errors.go` | NoRetryError、WrapNoRetry/Kind、RestrictedPageError、Retryable、ErrorKind | 定义错误分类；是否提交另一轮任务由执行/治理组件决定 |
| `challenge.go` | 显式站点挑战的错误类型、哨兵与有限证据 | 与账号凭据失效分开，不把响应体、Cookie 或令牌塞进错误值 |
| `failure_code.go` | FailureCodeError、WithFailureCode 的稳定诊断码 | 补充规则标识，不自行改变原错误的重试和补偿策略 |
| `compensation.go` | CompensationPolicy/Mode 与包装、提取函数 | 表达后续任务轮次条件，不创建任务、不执行补偿 SQL |
| `site.go` | Site 站点快照结构 | 结构含 Headers/RestrictedKeywords；冻结和返回副本由 sitedecl/siteCatalog 的交接点负责 |
| `stage.go` | StageKey 拼接站点/阶段作用域键 | 不登记阶段或查询运行池 |
| `stat.go` | Stage/Global/Worker/Queue/Site/Stop/Breaker 快照及 TraceStep 值 | 是共享视图契约，计数与采样状态仍归生产这些值的组件 |
| `request.go` | WithHeaders/HeadersFrom、ApplyHeaders、WithProxyURL/ProxyURLFrom | 上下文请求头使用副本；ApplyHeaders 明确原地修改调用方 map；不发 HTTP 请求 |
| `url.go` | ResolveURL、SameHost、IsSubdomainOf 的链接解析与域名判断 | URL 语义工具不替代 pkg/egress 的 DNS/IP 出口校验 |
| `siteurl.go` | ResolveSiteURL 将任务地址的路径/query 绑定到当前站点根地址 | 用于任务换域名回钉；不应当作任意外链跟随器 |
| `identity.go` | PageIdentity、TaskKey 的兼容性显式键工具 | 有意忽略 host 的路径/query 标识；不是当前持久任务身份的另一份默认算法 |
| `task_identity.go` | TaskIdentity、NormalizeTaskIdentity、CanonicalTaskURL | 站点/阶段/类型/值组成版本化身份；显式资源 ID 优先，URL 回退保留 origin，不随意删除 query |

新增跨包类型先确认是否有两个以上组件需要同一语义、是否不依赖具体实现。不要把业务实体、执行任务对象或 HTTP handler 放进 core。原始值含 map/slice 时仍须明确复制契约，位于 core 不自动意味着不可变。

## 5. models：持久结构与写入约束

models 描述框架拥有的表、字段、索引和局部 GORM hook。它依赖 GORM/JSON 类型，不能称为零依赖纯 DTO 包。迁移清单和 schema 检查归 internal/database；任务认领/结算归 internal/taskstore 和 Engine；运行策略事务归 internal/runtimecontrol 与存储适配器。

| 文件 | 模型 / 内容 | 数据语义与使用者 |
| --- | --- | --- |
| `crawler_task.go` | CrawlerTask、TaskStatus、RepeatableStatus；BeforeCreate；兼容哈希工具 | 任务身份、轮次、状态、延迟、账号关联和补偿字段；创建时规范身份并补诊断哈希，不在模型内启动执行 |
| `task_timing.go` | CrawlerTask.BeforeSave | 新任务的相对延迟使用数据库 UTC 时钟换算 NotBefore；更新不重复施加延迟，失败阻止本次插入 |
| `utc_time.go` | papa_utc GORM 序列化注册与转换 | 新时间字段的 UTC 墙上时间编码；不能把驱动时区转换与此规则混用 |
| `task_flow.go` | TaskFlow / crawler_task_flows | 动态任务流策略及其持久状态，事务与调度由 Engine 拥有 |
| `task_trace.go` | TaskTrace、TraceStatus / crawler_task_trace | 尝试内步骤、耗时、错误和可选数据；写入/保留期由 traceService 管理 |
| `breaker_incident.go` | BreakerIncident / crawler_breaker_incidents | 可恢复的事件/报告快照、revision 与 active；不是直接用来累计每次任务失败的计数器 |
| `breaker_history.go` | BreakerHistory / crawler_breaker_history | 事件内按序追加过程，事件 ID + 序号支撑重试核对；控制状态仍以事件快照为准 |
| `browser_identity.go` | BrowserProfile、BrowserIdentityBinding | 档案版本与绑定 CAS 结构；绑定 revision 不等同于账号版本或账号池选择版本 |
| `config.go` | Config / crawler_config | 框架运行配置的键值与版本；缓存、CAS 和历史事务由 admin/configstore 管理 |
| `runtime_policy_history.go` | RuntimePolicyHistory | 运行策略变更记录，与策略保存的事务关系由 configstore 保证 |
| `access_token.go` | AccessToken / crawler_access_token | 操作者、令牌哈希、启用状态；明文令牌不持久化，鉴权和管理归 admin/auth、tokenadmin |
| `operation_log.go` | OperationLog / crawler_operation_log | 后台动作审计行；队列、写入和关闭归 admin/oplog |

models 不列出框架所有可能使用的表：账号 GORM 适配器和 onboarding 存储也拥有自己的结构与迁移入口，详见 [Pkg 分册](PKG_STRUCTURE.md)。不能仅扫描本目录推断完整迁移清单。业务模型继续放独立业务模块，经公开 WithModels/UseModels 接入显式迁移。

## 6. docs 与仓库配套目录

| 路径 | 所有者与职责 | 放置规则 |
| --- | --- | --- |
| `docs/embed.go` | docs 包的嵌入清单；顶层 Markdown + package_structure/*.md | 只声明可分发框架手册，复制/升级操作归 cmd/papa；不在这里读取业务项目或写盘 |
| `docs/package_structure/` | 当前目录、包与文件职责说明书 | 全部职责明细集中本目录；README 做索引，各分册维护所属包，App 并入 Internal |
| `docs/README.md` | 使用手册阅读导航 | 指向说明书总入口；不再复制整份包文件职责表 |
| `docs/ARCHITECTURE.md` | 长期依赖、状态、生命周期和变更约束 | 保留跨组件规则；具体目录和文件清单以说明书为准 |
| `docs/DEVELOPMENT.md` | 构建、测试、平台说明与开发流程 | 包结构章节提供说明书索引；不维护第二份逐文件职责清单 |
| `docs/*.md` 其它手册 | 配置、CLI、账号、抓取、队列、身份、观测等使用契约 | 用法/配置项归相应手册；职责分册链接过去，避免抄一份参数字典 |
| `docs/issues/` | 历史审查、实施记录、验证边界与变更记录 | 记录当时事实，不冒充当前目录；不随框架手册分发 |
| `docs/superpowers/` | 设计与实施计划的历史材料 | 不作为另一套当前包说明，也不随框架手册分发 |
| `go.mod` / `go.sum` | Papa /v4 模块和依赖约束/校验 | 框架模块独立验证使用 GOWORK=off，不能依赖父工作区掩盖缺失依赖 |
| `Makefile` | build/cli/test/race/vet/clean 开发入口 | POSIX shell 语法；Windows 对应方式见开发手册，不将站点验收接线写入框架目标 |
| `LICENSE.txt` | 项目许可证 | 不属于运行包或配置 |
| `.claude/`、`.codex/`、`.idea/` | 当前工作区可见的助手/编辑器配置 | 不属于框架运行 API；不能把业务数据或可复用能力实现放进去 |
| `.review-tmp/` | 本地审查材料和验证产物 | 不属于正式说明书或生产源码；当前实现与验证结论写入对应正式文档 |

说明书会随 `papa new` / `upgrade` 保留 `package_structure/` 相对层级。历史记录只在源码仓库存在；分发副本的读者应按说明书查看当前职责，不依赖未分发的历史记录理解主契约。

## 7. 测试归属与新增内容

| 测试位置 | 验证目标 |
| --- | --- |
| 根 `papa_test.go` | 公开门面的类型与函数兼容 |
| 根 `architecture_test.go` | 依赖方向、App/server 前端资源归属及特定叶子契约约束；不证明锁序、SQL 或深副本正确 |
| config 的 load/business/duration/validate 等测试 | 解码、未知键、默认值、覆盖、校验确定性和配置领域规则 |
| core 的 identity/task_identity/request/errors 等测试 | 作用域身份、URL 语义、请求头副本、错误分类与补偿协议 |
| models 的 crawler_task_index/column_default/utc_time 等测试 | 表字段与索引契约、零值/GORM 默认、UTC 序列化；模型通过不等于真实数据库迁移已验收 |
| internal/database 与存储/Engine 的测试 | 显式迁移、SQL 方言、CAS、事务与真实库行为，由实际拥有者验证 |
| cmd/papa 的 docs_structure_test.go | 手册新建/升级分发、导航、保留项目内容与目录层级 |

新增公开入口先确定实现拥有者；新增配置先区分启动声明和运行状态；新增模型同时核对迁移清单及存储协议；新增手册同时维护说明书索引与分发链接。运行逻辑不因文档分册增加而机械新建包。
