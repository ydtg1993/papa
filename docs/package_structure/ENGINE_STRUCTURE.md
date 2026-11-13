# Engine 目录结构与职责细则

> 面向：维护执行引擎、任务提交、调度及恢复协议的开发者。
> 总入口：[Papa 目录说明书](README.md)。
> 配套：[总体架构规则](../ARCHITECTURE.md)、[开发手册](../DEVELOPMENT.md)、[Admin 目录说明](ADMIN_STRUCTURE.md)。
> 本文描述职责收口后的当前实现；最后一节记录保留的协调边界及后续开发规则。

## 1. 定位与审查结论

Engine 是单实例执行编排器，也是供 Fetcher 和宿主使用的公开门面。它连接任务持久化、阶段 worker、暂停/熔断、账号和抓取资源，负责跨组件的执行顺序及关停排空。

scheduler、execution、fetchruntime 保持独立内部包；根包的 submissionStore、taskFlowService、traceService、archiveService、siteCatalog、accountAlertGate 和 breakerCoordinator 各自拥有明确数据与操作契约。Engine 保留公开门面和跨暂停、持久化、投递的协调，不把完整 Engine 或 Config 交给这些组件。

判断是否需要拆分应看三个问题：是否拥有独立状态、是否存在语义接口、能否在不暴露内部字段的情况下独立验证。文件多或名字相似不构成单独拆包理由。涉及暂停、熔断和投递的原子协议尤其不能按目录机械拆开。

## 2. 当前物理目录

```text
engine/
├── *.go                       # 公开门面、同包协调器、执行与领域适配
├── *_test.go                  # 功能单测、Engine 集成、SQL 和 MySQL 回归
├── internal/
│   ├── scheduler/
│   │   ├── pools.go           # 泛型活池登记、懒建、回收及停止
│   │   ├── delay.go           # 泛型延迟堆、唤醒和取消协议
│   │   ├── doc.go
│   │   └── *_test.go
│   ├── execution/
│   │   ├── state.go           # 未确认持久化状态与重试背压
│   │   ├── doc.go
│   │   └── state_test.go
│   └── fetchruntime/
│       ├── resources.go       # 账号/身份/浏览器/HTTP 资源与执行适配
│       ├── downloads.go       # 下载覆盖配置及实例快照
│       ├── download_config.go # 站点/阶段覆盖项的独立值副本
│       ├── doc.go
│       └── *_test.go
└── testdata/
    └── mysql_reserved_words.txt # SQL 列名守卫测试输入
```

当前根层有 69 个生产 Go 文件；engine/internal 下有 3 个包。测试与所属职责同目录，根包保留跨组件集成回归。以下按职责分组列出全部根层生产 Go 文件。分组标题是逻辑分类，并不是当前存在的子目录。

## 3. 根包文件职责索引

### 3.1 公开门面、注册与生命周期

| 文件 | 当前职责 | 应守边界 |
| --- | --- | --- |
| `engine.go` | Engine 组件字段与 NewEngine 构造 | 不放任务流/提交 SQL 或功能服务实现 |
| `engine_config.go` | StageConfig、stageInfo、AddStage 声明登记与校验 | 下载覆盖项复制所有标量指针；声明期更新必须显式登记 |
| `lifecycle.go` | Stop、Errors，取消/worker/ticker/存留任务统计/熔断报告排空 | 共用原 deadline；等 ticker 收尾后再统计溢出、延迟和未确认写入 |
| `runtime_dependencies.go` | 数据库/配置/日志/事件/代理/默认下载器公开适配及 JSON 编码辅助 | 保留宿主和 Fetcher 兼容入口，不持有第二份功能状态 |
| `site_registry.go` | siteCatalog、SetSite/Site、请求头快照 | 使用原 breakerMu 协调；登记和读取都复制 map/slice，防止越过锁修改声明 |
| `account_alerts.go` | accountAlertGate，按账号/池/事件种类限频和有界淘汰 | 只管理通知节流，不管理账号健康真相 |
| `alias.go` | core 的类型、常量及函数兼容出口 | 不增加新状态或第二份类型实现 |
| `deps.go` | 阶段所需运行依赖检查 | 只验证声明/依赖是否可用，不隐式启动资源 |
| `stage_registration.go` | 构建/懒建 worker 池、错误转发、空闲回收、ApplyRegisterStage 启动接线 | 池登记状态归 scheduler；created 事件在登记锁外由实际创建者发一次 |
| `pools.go` | scheduler.Registry 的 Task 特化别名和构造桥 | 属于必要的类型适配，不应重新实现登记表 |
| `dynticker.go` | 动态周期、唤醒合并、取消及 ticker 等待组协作 | 由 Engine 生命周期管理，不能启动脱离 Stop 的后台循环 |

### 3.2 任务模型、提交、持久化和人工操作

| 文件 | 当前职责 | 应守边界 |
| --- | --- | --- |
| `task.go` | 公开 Task、模型转换、身份计算入口、旧 Insert/UpdateStatus/IncRetry 兼容方法 | Task 同时带有私有执行标记；旧接口兼容不意味着新执行路径应继续使用无法返回错误的写入 |
| `submit.go` | 公开提交、阶段/池选择、提交结果投递、延迟/暂停/满池协调 | 部分失败仍承接成功记录；单条已入库但回读失败时先保留 spill 责任，再记录错误日志 |
| `submission_store.go` | submissionStore 的身份查找、引用保护入库、批量部分成功、投递前条件更新、认领存储；返回单条计划/批量逐行交接结果 | 只接收数据库、context、去重缓存、引用保护接口和日志；不拥有 worker 或 Engine |
| `dedup.go` | 有界 LRU 去重状态、淘汰、活跃任务预热 | 缓存只做加速，数据库身份约束仍是持久依据 |
| `persistence.go` | 执行轮次 context、认领、终态/释放相关持久化协调、未确认状态登记、重试计数 | 复用 taskstore 语义操作；不可把确认丢失当作业务 handler 应重跑 |
| `task_timing.go` | 时间辅助值、尝试计时适配、未到期任务推迟 | 调度时间与实际执行时间分开，不擅自改变执行轮次 |
| `taskadmin.go` | 后台/宿主的重试、加急、轮询设置、人工失败、删除及拒绝原因 | 这是任务领域操作，不是 HTTP handler；实际数据修改复用条件更新协议 |
| `compensation.go` | 账号失败补偿范围/版本、尝试归属持久化、预览、释放、批量补偿 | 保留原任务身份和版本；不得在 admin 再实现另一套补偿状态机 |
| `taskflow.go` | 公开任务流门面、开关/阶段校验、Task 转换、投递和唤醒 | 服务提交成功后才投递，不执行 SQL |
| `taskflow_service.go` | taskFlowService 的创建、策略 CAS、父子关系登记、事务认领与运行视图 | 仅持有数据库；flow 行锁范围与事务边界保持，不反调 Engine |
| `taskflow_policy.go` | FlowPolicy、参数验证、放行判据与路径计算 | 纯策略方向正确；有独立消费需求时才提取契约，避免依赖整个 Engine |

### 3.3 执行轮次与持久化确认

| 文件 | 当前职责 | 应守边界 |
| --- | --- | --- |
| `stage.go` | worker 执行编排、任务认领/释放、重试、成功/失败结算、单次尝试装配 | 保留跨组件顺序，避免把所有细节重新塞回 stageHandler |
| `attempt_execution.go` | attemptExecution：一次 handler 的 context、trace/归档终结、panic 传播、账号反馈与计时协调 | 不持有 Engine；handler 调用通过回调传入，持久化重试不能重新调用 handler |
| `attempt_persistence.go` | attemptPersistence：本次尝试时间写入及失败结算依赖 | 不持有 Engine，但仍直接依赖 GORM、execution.State 和 breakerCoordinator；属于同包组件，不是完全独立存储层 |

### 3.4 调度、溢出和治理队列

| 文件 | 当前职责 | 应守边界 |
| --- | --- | --- |
| `delayqueue.go` | 把 Task 接入延迟堆、到期投递和必要的重新排期 | 堆/唤醒算法在 scheduler；这里只决定任务到期后怎么交回执行链 |
| `scheduler_spill.go` | spillQueue：溢出任务成员、去重、Take/Restore/Put 和锁 | 无 Engine 回引用；取出的桶交由调用者持有，锁不跨 SQL/worker 投递 |
| `spill.go` | 溢出登记、回灌循环和任务重新投递 | 协调暂停、容量与持久状态，不自己复制溢出 map |
| `batch.go` | 有界批处理、批间暂停、并发辅助及重投失败处理 | 为治理入口提供执行工具；不演变为任意功能工具集合 |
| `errorqueue.go` | 失败任务查询、手动/周期重投及失败队列循环 | 一条全局治理队列；重投状态更新使用既有持久化协议 |
| `recoverqueue.go` | 启动恢复范围、串行化处理、恢复投递及跳过统计 | 遵守进程启动截止时间，不能重新认领本进程已接管的任务 |
| `repeatpoll.go` | 到期/强制轮询查询、轮次重置、上限、下次周期和唤醒 | 首次提交、下一轮轮询与同轮回灌不可混为同一次计数 |
| `queuestats.go` | 队列运行/积压快照、计数及采样循环 | 运行状态在自身锁内更新，队列轮次事件在锁外发布，允许观察者查询完成后的统计 |

### 3.5 暂停与熔断

| 文件 | 当前职责 | 应守边界 |
| --- | --- | --- |
| `pause.go` | 站点/阶段暂停信号、继承查询、人工操作、广播闸门、声明期暂停 | 暂停信号必须只有一份；状态目前直接位于 Engine |
| `breaker_coordinator.go` | breakerCoordinator 状态、准入、历史与报告写入串行化 | 不持有 Engine；对外提供锁内语义操作和报告接口 |
| `breaker_events.go` | 当前熔断事务的独立事件值批次与锁外同步派发 | 先完整发布状态及暂停，再调用外部 emitter；不新增后台队列或派发互斥锁 |
| `breaker_control.go` | Engine 公开暂停/恢复/状态门面 | 协调既有暂停信号与站点投影 |
| `breaker_control_state.go` | 熔断登记、人工接管/恢复决策和状态投影 | 仅改变 coordinator 内部状态 |
| `breaker_outcome_state.go` | 终态观测、证据累计与保护决策 | 返回动作结果，由 Engine 在原临界区内发布暂停 |
| `breaker_probe_state.go` | 探针选择、轮次身份和结果结算 | goroutine 由 Engine 登记并排空；取消状态在决策位置读取，不能提前保存 Err 快照 |
| `breaker_recovery_state.go` | 检查覆盖、重试轮次、周期状态推进和恢复完成 | 显式接收阶段覆盖与治理证据值，不读取整个 Engine |
| `breaker_restore_state.go` | 存储适配设置与启动快照恢复 | 数据库加载放在锁外，恢复状态与保护发布保持原顺序 |
| `breaker_sample_state.go` | 样本预留、重验和投递结果结算 | 以事件/截止时间等令牌防止跨轮次误结算 |
| `breaker_settlement_state.go` | 失败结算预留、确认和延后 | 任务 SQL 在调用侧锁外执行，状态确认回到原锁内 |
| `breaker_runtime.go` | 熔断类型与运行初始化、恢复持久状态、终态观测、周期推进、探测执行 | 内部状态由 coordinator 方法处理，门面协调治理、暂停和 probe 生命周期 |
| `breaker_recovery.go` | 检查重置、恢复完成、阶段探测、重试轮次和人工暂停协调 | 保留暂停/熔断共同发布的锁序和原子性 |
| `breaker_samples.go` | 样本需求、选择/超时判据及样本任务重试 | 纯判据与 Engine 投递适配可分别验证 |
| `breaker_settlement.go` | 失败意图保留、补结算以及任务字段更新投影 | 任务终态与熔断计数确认必须遵守已有日志/确认协议 |
| `breaker_snapshot.go` | 熔断事件、检查与状态的报告快照和裁剪 | 返回投影，不能泄漏内部可变证据状态 |

### 3.6 抓取、账号与运行治理

| 文件 | 当前职责 | 应守边界 |
| --- | --- | --- |
| `fetch.go` | 渲染/HTML 公开入口、选项和请求头合并、资源注入、trace/归档接入 | 浏览器资源申请与归还委派 fetchruntime；当前仍包含较多适配策略 |
| `http.go` | HTTP/增量抓取、响应捕获与执行上下文衔接 | 网络传输实现使用独立客户端，不在这里新增站点业务 |
| `capture.go` | 浏览器网络请求捕获与捕获记录 | 只接通任务观测上下文，捕获实现依赖通用浏览器能力 |
| `record.go` | 网络录制公开入口 | 录制规则在通用组件中，Engine 提供执行环境 |
| `downloads.go` | 下载器初始化、站点/阶段配置装配及公开查找入口 | 覆盖配置与解析快照归 fetchruntime.Downloads |
| `account.go` | 账号 Manager 注入、状态和账号 HTTP/HTML/浏览器公开适配 | 凭据与账号生命周期归 pkg/account |
| `account_pool.go` | 账号池引用、准备账号、按池抓取的公开入口 | 池选择和资源归还委派账号资源能力，不复制账号状态 |
| `account_events.go` | 账号健康事件订阅、告警与节流 | 取消订阅由 Engine 生命周期持有；告警节流由 accountAlertGate 拥有，Stop 仍按原顺序解绑 |
| `identity.go` | 身份 Manager 注入、查询和会话身份 context 适配 | 档案与绑定状态归浏览器身份组件 |
| `governance.go` | 运行策略解析/安装、声明与 schema 检查、任务 context/账号池准备 | controller 与独立的延迟 map 副本在同一 e.mu 临界区发布；后台策略事务不属于 Engine |
| `restricted.go` | 特定运行场景的能力限制检查 | 统一执行限制入口，不能在各抓取路径另造互相冲突的开关 |

### 3.7 追踪、归档、快照与通知

| 文件 | 当前职责 | 应守边界 |
| --- | --- | --- |
| `trace.go` | Trace 步骤缓冲/锁/终结、context 及公开查询/配置适配 | 单次记录器委派存储，不执行维护 SQL |
| `trace_service.go` | traceService 的窄配置值、查询、批量写入与分批过期清理 | 无 Engine/完整 Config，取消在批次间生效 |
| `archive.go` | archiveBuffer 的单次页缓冲、限额、结果选择及公开适配 | 单次尝试拥有页面，文件操作委派服务 |
| `archive_service.go` | archiveService 的配置值、命名、文件写入、保留期清理 | 只接收归档配置值与日志，不接收 Engine |
| `evidence_maintenance.go` | 共同巡检循环，启动前登记 tickerWG、取消后退出 | Engine 仍负责取消与等待；后台清理未完成时 Stop 不得报告 drained |
| `stats.go` | 阶段统计队列、池回收后的累计量及阶段快照 | 不能把池回收误当累计统计清零 |
| `sitestat.go` | SiteStore 契约、声明对齐、站点统计和熔断状态投影写入 | Store 注入，站点统计投影与运行暂停信号不是两个决策源 |
| `snapshots.go` | 周期运行事件快照、失败事件、时间与阶段键投影 | 消费统计，不执行调度决策 |
| `notifications.go` | Notifier 注册、失败和熔断告警、失败观测衔接 | 通知失败不能改写任务已确认的结局 |

## 4. 三个内部包是否真正解耦

| 包 | 独立拥有的状态/接口 | 判断与限制 |
| --- | --- | --- |
| `engine/internal/scheduler` | Registry 的池表/最后投递时间/停止位；Delay 的堆、锁、唤醒、timer 和待交付值快照 | 泛型接收任务值，不导入父包；Delay.Snapshot 复制容器，不在堆锁内运行回调。Task 的持久化和暂停仍由 Engine 协调 |
| `engine/internal/execution` | State 的 unresolved map、锁、快照和 Retry 背压 | 已实质拆分，但职责是持久化确认状态，不是“所有任务执行”。依赖 GORM/taskstore 错误语义，后续不应随名称向此处堆执行功能 |
| `engine/internal/fetchruntime` | Resources 资源登记与锁、资源申请/归还；Downloads 配置覆盖、解析和查找快照 | 不持有 Engine。download_config.go 复制站点/阶段覆盖值，不能保留调用方的可变指针。资源生命周期仍归各 Manager/Pool |

三个包均有自己的测试，且架构守卫禁止其导入父 engine、admin 或 internal/app。导入守卫只证明依赖方向，不能证明父包已停止直接操作同包组件的字段。

## 5. engine 目录之外的协作者

| 位置 | 职责与调用边界 |
| --- | --- |
| `core/` | 跨组件值契约；保留 engine 别名，不制造同名但身份不同的 DTO |
| `internal/taskstore/` | 任务认领、终态、释放、重试/轮询等语义持久化操作；不依赖 Engine/HTTP |
| `internal/workerpool/` | worker 执行、队列和关闭能力；阶段策略由 Engine 组装 |
| `internal/breaker/` | 检测与恢复策略基础能力；不等于 engine 的跨任务恢复协调器 |
| `internal/breakerstore/` | 熔断控制、生命周期/历史的存储适配 |
| `pkg/account/`、`pkg/browser/`、`pkg/htmlfetch/` | 账号、浏览器与 HTTP/HTML 能力和资源管理 |
| `pkg/governance/`、`pkg/middleware/` | 单实例请求预算、限速、代理和下载器等运行能力 |
| `admin/*` | 将公开操作/快照适配成 HTTP 和页面；不能把任务规则再写一遍 |

## 6. 状态、锁与生命周期

| 状态 | 当前拥有者 | 协调约束 |
| --- | --- | --- |
| 阶段登记、阶段累计量、错误转发及部分运行配置 | Engine，部分字段使用 `e.mu` | 声明期登记与运行期修改有不同协议，不能默认所有 map 都允许任意并发修改 |
| 活池、延迟任务、溢出任务 | Registry、Delay、spillQueue | 关闭后不得新建池；回灌不重复计轮次；未投递任务要计入停机统计 |
| 未确认任务写入 | execution.State | 确认后清除，取消时保留可见性；不能靠重复调用 handler 补写 |
| 暂停 map、广播 channel、恢复时间 | Engine 的 pause 状态 | `breakerMu → pauseMu → (overflow.mu 或 e.mu)`；pause 锁不跨数据库调用 |
| detector、trial、证据和报告串行门 | breakerCoordinator | 纯内部变更在 coordinator 方法内；涉及暂停/投递的事务由明确协调入口发布 |
| 站点声明 | siteCatalog，继续使用既有 breakerMu | 目录组件拥有声明内容并提供脱离内部存储的快照；为保持协调顺序不引入第二把锁 |
| 单次 trace 与归档 | Trace、archiveBuffer、attemptExecution | 尝试结束和 panic 路径都终结；账号敏感内容不进入原始页面归档 |
| 抓取资源登记及下载配置快照 | fetchruntime.Resources/Downloads | 短锁取依赖，网络工作和用户回调不在登记锁内执行 |

Stop 不只是取消 context：还要关闭池登记并排空 worker、等待 ticker、统计溢出/延迟/未确认写入、结算熔断报告。worker 与 ticker 共用入口 deadline；统计必须在 ticker 等待结束后执行，避免漏掉它最后保留的任务或重复报告已确认写入。超时后的统计仅代表最后观察值，不表示后台已静止。Delay.Snapshot 保留任务，由 Engine 在锁外按阶段汇总，不清空堆。任何后续抽取的维护循环或服务必须明确如何加入这个关闭协议。

配置的“拥有”也包含输入复制：SetSiteDownloads/AddStage 保存覆盖项的独立值，InstallGovernance 保存延迟 map 副本；调用方随后复用输入不会改变已登记状态。显式重新登记/安装仍是更新入口，不改变阶段优先于站点的解析顺序。

事件是完成状态的观察值。创建池、队列轮次和熔断事务都在释放对应状态锁后同步派发，不能让自定义观察者查询状态时重新等待当前持有的锁。熔断探针的结果里程碑先派发，再重验当前事件/控制轮次和取消状态，旧结果不能覆盖回调期间的新人工操作。批次内部保持事件顺序，不承诺并发或嵌套回调的全局顺序。

## 7. 测试归属与新增代码规则

- 内部包算法和并发状态直接在所属包测试；Engine 测试验证多个组件组合后的执行协议。
- `component_boundaries_test.go` 防止同包组件重新持有 Engine 或完整配置；组件测试覆盖提交部分成功、任务流事务、证据清理排空及站点快照隔离。
- 根包中的 `fakedb_test.go` 是共享假库，不等同真实 SQL 执行；`sqlshape_test.go`/`sqlreserved_test.go` 检查 SQL 形状与列名，`*_mysql_test.go` 验证实际事务/CAS/重启恢复。
- `attempt_*`、`stage_runversion_test.go`、`persistence_test.go` 验证尝试与持久化确认；暂停、溢出、延迟、轮询相关测试验证调度协作；`breaker_*_test.go` 验证恢复证据与结算链。
- 保留同包测试访问必要内部状态的能力，但不能为了测试创建无生产用途的转发 API。若组件抽包，迁移算法单测，同时保留关键 Engine 集成用例。
- 新能力先确定执行状态属于谁；Engine 只增加公开适配和跨组件编排。只处理 SQL 的实现优先归存储组件，纯策略优先保持无运行依赖。
- 不把整个 Engine 传给新子组件。现有 Fetcher 公开契约接收 Engine 是兼容入口，不能作为内部组件继续持有 Engine 的理由。
- 拆分涉及任务状态、锁序、取消、背压或关停时，除 test/vet/build 外需要 race；涉及 SQL 事务还需隔离 MySQL 回归。
- `config_ownership_test.go` 和 fetchruntime 的 `download_config_test.go` 验证输入隔离；`lifecycle_review_test.go` 验证 ticker 收尾统计与延迟存留；`breaker_review_cancellation_test.go` 验证结果事件期间的取消；提交/任务流测试覆盖错误日志、批量交接异常和事务中途取消。

## 8. 收口结果与保留边界

| 原审查问题 | 当前实现 | 后续约束 |
| --- | --- | --- |
| engine.go 职责混合 | 契约、依赖适配、站点目录、控制入口、生命周期分文件；构造只连接组件 | 分文件只是可读性改进，实际边界由组件状态与操作接口保证 |
| 提交 SQL 与投递混合 | submissionStore 负责持久化准备和认领；submissionPlan 显式交付成功/未读记录的投递责任 | 部分失败也必须处理成功项；重复项在投递后回填轮次信息 |
| 任务流事务挂在 Engine | taskFlowService 只依赖数据库，统一版本、父子登记、认领与查询 | flow 行锁和事务范围不能随重构拆散 |
| 熔断状态分散修改 | coordinator 收回内部状态操作；Engine 根据值结果执行暂停发布、任务操作和探测生命周期 | 原 breaker → pause → overflow/e.mu 顺序不变，不引入互相调用的新锁 |
| 追踪/归档混合记录与维护 | 单次记录器 + 窄配置服务 + 统一维护循环 | 动态配置通过门面按调用取值；维护继续纳入 tickerWG，不能先报告排空再关闭数据库 |
| 站点及告警状态分散 | siteCatalog 拥有声明快照，accountAlertGate 拥有限频状态 | 站点继续使用原协调锁；不能用修改返回 map 的方式改变引擎声明 |

Task/Trace、Fetcher、StageConfig 以及 Engine 公开 API 保留。stageHandler 仍协调账号准入、执行轮次、终态确认和熔断结算；pause.go 仍拥有暂停信号及跨状态发布。它们是需要显式顺序的执行协议，不是应该按文件名机械拆包的剩余工作。

本轮优先采用同包组件，没有为了缩短目录列表而新增重复子包。下一步新增功能必须使用组件语义接口；若出现新的独立消费者或生命周期，再评估抽包，而不是重新导出内部 map、锁或完整 Engine。

历史复核与修复记录位于源码仓库 `docs/issues/2026-11-13-engine-second-review.md`；它不随使用手册分发，当前职责以本册为准。
