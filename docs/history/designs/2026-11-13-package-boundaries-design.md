# Papa 包职责与内部结构重整设计

<!-- version-scope -->
> **适用版本/阶段：Papa v4 演进过程中的历史工作区快照（具体基线以正文为准；未记录精确发布版的条目不推定版本号）。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 本文为设计/计划记录，不能仅凭计划步骤或复选框认定已实现或已验收。 当前使用方式以 [v4 手册导航](../../README.md) 为准。
<!-- /version-scope -->


日期：2026-11-13

状态：用户已于 2026-11-13 确认；全部实施完成，普通测试、全量 race、vet、build 与业务兼容构建通过。实际组件边界及验证记录见配套实施计划。

## 1. 目标与约束

将 Papa 从依赖 App / Engine 共享内部状态的组织方式，逐步调整为由明确职责的组件组成的单实例大单体。每个组件能够说明自己拥有的状态、允许的依赖、调用入口和关闭责任。

保持公开 API、已有路由、JSON 字段、配置键、表结构、任务身份与轮次语义兼容。此次不加入真实站点业务，不改为多实例或微服务，不执行任何 Git 写操作。已有工作区修改是迁移基线，不能覆盖或撤销。

当前已存在未提交的熔断结算、持久化、stage、task 修改及测试；涉及这些文件的阶段必须先读取当时完整 diff，验证基线，再做结构调整。

## 2. 方案选择

1. **推荐：兼容门面 + 按职责逐段迁移。** 先后台和应用服务，再任务存储，最后引擎状态组件；每阶段单独可编译、测试和审查。需要少量兼容转发，但可控制行为变化。
2. 仅按文件类型搬目录：变更小，却保留所有共享状态和混合职责，不能满足此次目标。
3. 一次重写 App / Engine 和公开 API：可自由设计，但业务迁移和并发回归范围过大，不采用。

## 3. 目标职责与依赖

以下是迁移完成后的职责规划，不是当前目录现状；不要一次生成空包。

| 位置 | 拥有的职责 | 不允许承担的职责 |
| --- | --- | --- |
| 根包 papa | 稳定入口、兼容类型及构造函数 | 算法、HTTP handler、状态存储 |
| internal/app | 创建组件、注册业务、启动与关闭编排 | 页面脚本、策略事务、诊断算法、具体 HTTP 处理 |
| admin/server | 后台外壳、公共资源、鉴权接入与挂载 | 账号/策略领域规则 |
| admin/accounts | 账号、账号池及任务补偿 HTTP 适配和资源 | 账号池状态机、直接管理 App 的锁 |
| admin/runtimecontrol | 采集策略、历史、代理健康 HTTP 适配和资源 | 持久策略 CAS、核对和发布协议 |
| admin/diagnostics | Rod、身份诊断 HTTP 适配和资源 | Chromium 执行过程、身份比较算法 |
| internal/runtimecontrol | 策略版本、校验、保存、核对、发布及账号池引用协调 | HTTP 状态码、JS、页面布局 |
| internal/browserdiagnostics | 隔离浏览器启动检查、合成 fixture、身份观测比较 | 后台鉴权和页面注册 |
| internal/reportrunner | 定时报表及退出报表的串行运行、取消和排空 | HTTP handler、任务执行 |
| engine | 公开 Engine / Task 契约、兼容门面与执行编排 | 不断新增的独立子系统内部状态 |
| internal/taskstore | 任务认领、终态、释放、重投等持久化语义与模型转换 | worker 调度、HTTP、整个 Engine 的引用 |
| engine/internal/scheduler | 活池、延迟、溢出回灌、调度生命周期 | DB 状态迁移、账号认证 |
| engine/internal/execution | 单次执行及异常收尾协议 | 后台与资源具体实现 |
| engine/internal/breakerruntime | 熔断准入、恢复生命周期、报告写入协调 | HTTP、通用任务 CRUD |
| engine/internal/fetchruntime | 抓取资源选择、账号/身份/浏览器与下载器接线 | 队列、后台、任务终态 |
| pkg/account | 认证、账号池、健康与使用计量语义 | GORM 存储适配细节 |
| pkg/account/gormstore | 账号池 GORM 存储适配与模型 | 账号选择和认证策略 |

已有 core、obs、internal/breaker、internal/breakerstore、pkg/browser 等继续复用，不为目录对称而搬迁。

依赖方向：App 装配后台、服务和引擎；后台通过消费者定义的窄接口调用服务；服务通过窄接口调用引擎能力及存储。服务不导入 App、HTTP 包或具体后台包。引擎内部组件不导入父 engine 包，也不接收整个 Engine。需要跨包的任务数据采用内部值类型和门面转换；不得把所有类型随意倒进 core。

运行策略服务使用自己的存储接口；现有 admin/configstore 可以先作为实现由 App 注入，不应为了复用而使服务依赖 admin。后续如抽出配置存储实现，原公开构造函数保留转发。

## 4. 后台与 App 迁移

第一阶段从 accounts.go/accounts.js 及 Rod 诊断切入，建立明确依赖的 handler 和功能内 static 资源。随后迁移 account_pools_api.go、account_compensation.go、runtime_policy.go、runtime_history.go、proxy_health_api.go、browser_identity.go 及对应脚本与测试。

HTTP 层只负责输入大小限制、解码、调用服务、错误到 HTTP 的映射及响应；服务返回领域结果，审计用显式操作人/请求元信息传递，不传 http.Request。

保留现有页面 key、路由、认证与白名单包装、静态 URL、脚本加载顺序及页面卸载清理。保留 custom.js 对外行为，资源由功能包嵌入后交给 App 装配。不同页面共享的 window.PapaRuntime 能力先明确由 runtimecontrol 提供，不能因移文件改变加载顺序。

App.UsePage、UseRouter、UseScript 等既有公开扩展保留；App 中仅保留注册及委派，不能以包迁移为由删除业务扩展点。

运行策略服务拥有 revision、uncertain、effective、document 及其锁。策略更新与账号池删除目前共享互斥区，迁移后统一由服务完成引用检查和删除协调，不能拆成“先查询引用再无锁删除”。原有 CAS 冲突、写入确认丢失、核对失败保持保守状态等行为必须保留。

诊断组件拥有临时服务/浏览器及取消清理；HTTP 适配不能直接控制组件内部锁。报表组件拥有 ticker、串行锁、取消和等待组，App 在关库前等待其排空。

## 5. 任务存储与引擎迁移

先抽取存储操作，再迁移运行组件，避免同时重写 SQL 和调度协议。

任务存储操作以 Claim、Complete、Release、Retry、AdvanceRepeat 等语义组织，参数显式包含任务身份和轮次条件；返回是否生效、版本冲突或错误。保留原条件更新和事务语义，不引入通用 CRUD 接口掩盖规则。

Task.Insert、UpdateStatus、IncRetry 作为已有公开兼容入口保留，委派到统一操作；执行路径使用能够返回错误的内部接口。IncRetry 的错误处理差异需单独测试，不能借结构迁移静默改变重试次数或行为。

执行组件负责处理取消、panic、终态持久化未确认、资源释放和事件顺序；只有确认落库后才能执行相应完成清理。业务 handler 不因状态写入重试而再次执行。

调度组件拥有活池、延迟堆、溢出队列及其锁，关闭时明确停止接收、取消等待、排空和报告未完成项。熔断组件保留既有 epoch / trial / round 隔离、晚结果拒绝和恢复预算。

拆锁必须先记录现有跨状态原子性；可以先在同包引入独立结构体并委派，证明状态边界后再移入子包。禁止只暴露字段或大量回调来模拟旧的全对象访问。

## 6. 公开 API 与适配器

根包与 engine 的公开签名保持兼容；internal/app 的导出字段因为类型别名已暴露给业务，不能直接删除或改类型。新能力不再增加公开可变字段。

账号 GORM 适配器抽取时，避免 account 导入子包、子包又导入 account 的循环：先将必要存储契约放到无反向依赖的独立契约位置，再以类型别名/转发保留原入口；只有依赖关系通过验证后才迁移实现。

config.BreakerPolicy 从算法实现类型中解耦时，需核对所有公开别名和赋值兼容性，不能只复制一个同形结构体。此项作为最后阶段独立审查，不与熔断状态机迁移混做。

## 7. 分阶段交付与验收

| 阶段 | 交付物 | 必须证明 |
| --- | --- | --- |
| A | 后台功能包、诊断组件、App 委派 | 路由/鉴权/页面/诊断取消清理行为不变；功能可脱离 App 测试 |
| B | 运行策略服务、报表组件 | 无 HTTP 依赖也能执行策略更新；CAS/不确定核对/池引用协调/关停顺序不变 |
| C | 任务语义存储入口 | 普通执行、后台操作和治理队列复用状态规则，真实 SQL 条件与事务验收 |
| D | Engine 状态组件 | 不引用整个 Engine；状态与锁有明确拥有者；并发及崩溃恢复回归通过 |
| E | 账号存储适配和公开契约整理 | 公开 API 编译兼容、无循环依赖、文档符合实际 |

每阶段迁移相关原有测试，新增的是接口边界与行为回归，不能为了测试变绿删除原有保护。测试包迁移后避免依赖原包私有状态；App 仅保留装配集成测试。

使用 GOWORK=off 执行 go test ./...、go vet ./...、go build ./...；并发迁移执行 race。先记录现有失败/跳过与环境原因，再区分新回归。JS 使用现有 Node 行为回归和合成 Chromium 验证，不把静态文本断言等同于交互验证。

存储阶段使用隔离 MySQL 测试验证 CAS、事务和重启，不对业务数据库迁移。环境缺失必须明确记录未验证范围。公开 API 用合成调用方及现有业务模块的只读构建检查验证，不能自动改业务模块。

加入有意义的架构守卫：后台功能包禁止导入 internal/app；内部运行服务禁止导入 net/http 与 admin；引擎内部组件禁止导入父 engine。守卫检查 import 依赖，不机械限制文件数和行数。

每阶段同步 docs/architecture/DEVELOPMENT.md 的实际结构、docs/operations/MONITOR.md 的模块维护说明；公开用法若不变，不制造迁移说明。每个新包用 doc.go 说明职责与依赖。

## 8. 完成标准

全部阶段达到各自验收后才可宣称整体重构完成。仅移动 JS、拆 app.go 或增加目录不算完成。每次交付报告已迁移范围、验证结果、原有失败、剩余阶段和兼容性限制。


## 实施中的边界细化

为保持现有 Task / Fetcher / Trace 类型及跨暂停和结算事务，单次执行与熔断运行协调器采用设计允许的同包组件：`attemptExecution`、`attemptPersistence`、`breakerCoordinator`、`spillQueue`。这些组件不保存 Engine 引用。Engine 通过明确调用编排跨组件的事务；只有公开 FetchHandler 调用以临时闭包适配原有 API。

独立子包 `engine/internal/execution` 负责持久化重试和未确认状态，`scheduler` 负责活池及延迟队列，`fetchruntime` 负责资源注册/选择/归还与下载器解析。未建立空的 `engine/internal/breakerruntime`，也未复制 Task/Trace 造一套平行公开类型。

账号接入与账号池/补偿分别归属 `admin/accounts` 和 `admin/accountpools`，共享鉴权与 JSON 协议；每包只消费其功能所需接口。公开后台接口的内部值通过公开类型别名暴露，外部调用方可实现服务接口而无需导入 internal。

执行重试计数的确认策略采用保守方案：内部 incrementExecutionRetry 检查一次性 UPDATE 错误；失败或确认丢失时记录未确认任务并退出，不重放加法、不调用下次 handler。旧公开 Task.IncRetry 仍保持原内存递增语义。新增 before-write 与 lost-ack 回归分别验证此边界；未声称自动重试非幂等计数写入。
