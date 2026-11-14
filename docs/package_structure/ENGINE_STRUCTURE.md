# Engine 目录结构与职责细则

> 面向：维护执行引擎、任务提交、调度及恢复协议的开发者。
> 总入口：[Papa 目录说明书](README.md)。
> 配套：[总体架构规则](../architecture/ARCHITECTURE.md)、[开发手册](../architecture/DEVELOPMENT.md)、[Admin 目录说明](ADMIN_STRUCTURE.md)。
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
│   │   ├── settlement.go      # 成功业务遇取消时的有界持久化确认
│   │   ├── doc.go
│   │   └── *_test.go
│   └── fetchruntime/
│       ├── resources.go       # 账号/身份/浏览器/HTTP 资源与执行适配
│       ├── downloads.go       # 下载覆盖配置及实例快照
│       ├── download_config.go # 站点/阶段覆盖项的独立值副本
│       ├── doc.go
│       └── *_test.go
└── testdata/
    └── mysql_reserved_words.txt # SQL 列名守卫测试输入
```

当前根层有 74 个生产 Go 文件、98 个测试 Go 文件；engine/internal 下有 3 个包、10 个生产 Go 文件、7 个测试 Go 文件。测试与所属职责同目录，根包保留跨组件集成回归。以下按职责分组列出全部根层生产 Go 文件。分组标题是逻辑分类，并不是当前存在的子目录。

## 3. 根包文件职责索引

### 3.1 公开门面、注册与生命周期

| 文件 | 当前职责 | 应守边界 |
| --- | --- | --- |
| `engine.go` | Engine 组件字段与 NewEngine 构造；预热活跃任务去重并启动延迟 dispatcher | 构造已经有读库及后台循环副作用，直接使用 Engine 的宿主也须负责 Stop；不放任务流/提交 SQL 或功能服务实现 |
| `engine_config.go` | StageConfig、stageInfo、AddStage 声明登记与校验 | 下载覆盖项复制所有标量指针；声明期更新必须显式登记 |
| `lifecycle.go` | Stop、Errors，取消/worker/ticker/存留任务统计/熔断报告排空 | 共用原 deadline；等 ticker 收尾后再统计溢出、延迟和未确认写入 |
| `runtime_dependencies.go` | 数据库/配置/日志/事件/代理/默认下载器公开适配及 JSON 编码辅助 | 保留宿主和 Fetcher 当前公开入口，不持有第二份功能状态 |
| `site_registry.go` | siteCatalog、SetSite/Site、请求头快照 | 使用原 breakerMu 协调；登记和读取都复制 map/slice，防止越过锁修改声明 |
| `account_alerts.go` | accountAlertGate，按账号/池/事件种类限频和有界淘汰 | 只管理通知节流，不管理账号健康真相 |
| `deps.go` | 阶段所需运行依赖检查 | 只验证声明/依赖是否可用，不隐式启动资源 |
| `stage_registration.go` | 构建/懒建 worker 池、错误转发、空闲回收、ApplyRegisterStage 启动接线 | 池登记状态归 scheduler；created 事件在登记锁外由实际创建者发一次 |
| `pools.go` | scheduler.Registry 的 Task 特化别名和构造桥 | 属于必要的类型适配，不应重新实现登记表 |
| `dynticker.go` | 动态周期、唤醒合并、取消及 ticker 等待组协作 | 由 Engine 生命周期管理，不能启动脱离 Stop 的后台循环 |

### 3.2 任务模型、提交、持久化和人工操作

| 文件 | 当前职责 | 应守边界 |
| --- | --- | --- |
| `task.go` | 公开 Task、模型转换、v4 身份计算及私有插入实现 | Task 带有私有执行标记；状态和重试写入归带版本的执行协议 |
| `submit.go` | 公开提交、阶段/池选择、提交结果投递、延迟/暂停/满池协调 | 部分失败仍承接成功记录；单条已入库但回读失败时先保留 spill 责任，再记录错误日志 |
| `submission_store.go` | submissionStore 的身份查找、引用保护入库、批量部分成功、投递前条件更新、认领存储；返回单条计划/批量逐行交接结果 | 只接收数据库、context、去重缓存、引用保护接口和日志；不拥有 worker 或 Engine |
| `dedup.go` | 有界 LRU 去重状态、淘汰、活跃任务预热 | 缓存只做加速，数据库身份约束仍是持久依据 |
| `persistence.go` | 执行轮次 context、认领、终态/释放相关持久化协调、未确认状态登记、重试计数 | 复用 taskstore 语义操作；不可把确认丢失当作业务 handler 应重跑 |
| `task_timing.go` | 时间辅助值、尝试计时适配、未到期任务推迟 | 调度时间与实际执行时间分开，不擅自改变执行轮次 |
| `taskadmin.go` | 后台/宿主的重试、加急、轮询设置、人工失败、删除及拒绝原因 | 这是任务领域操作，不是 HTTP handler；实际数据修改复用条件更新协议。版本化重投的读取、CAS 与冲突回查传递调用方 context，提交成功后的投递收尾不因请求取消而丢弃 |
| `compensation.go` | 账号失败补偿范围/版本、尝试归属持久化、预览、释放、批量补偿 | 保留原任务身份和版本；批量补偿读取传递调用方 context，不得在 admin 再实现另一套补偿状态机 |
| `taskflow.go` | 公开任务流门面、开关/阶段校验、Task 转换、投递和唤醒 | 服务提交成功后才投递，不执行 SQL |
| `taskflow_service.go` | taskFlowService 的创建、策略 CAS、父子关系登记、事务认领与运行视图 | 仅持有数据库；flow 行锁范围与事务边界保持，不反调 Engine |
| `taskflow_policy.go` | FlowPolicy、参数验证、放行判据与路径计算 | 纯策略方向正确；有独立消费需求时才提取契约，避免依赖整个 Engine |

### 3.3 执行轮次与持久化确认

| 文件 | 当前职责 | 应守边界 |
| --- | --- | --- |
| `retry_backoff.go` | Handler 退避策略：首次重试指数 0、60s 封顶、Equal Jitter | 仅调用 internal/backoff 计算；等待、计数和结算仍在 stage.go，无新增状态/锁 |
| `stage.go` | worker 执行编排、任务认领/释放、重试、成功/失败结算、单次尝试装配 | 保留跨组件顺序，避免把所有细节重新塞回 stageHandler |
| `attempt_execution.go` | attemptExecution：一次 handler 的 context、trace/归档终结、panic 传播、账号反馈与计时协调 | 不持有 Engine；handler 调用通过回调传入，持久化重试不能重新调用 handler |
| `attempt_persistence.go` | attemptPersistence：本次尝试时间写入及失败结算依赖 | 不持有 Engine，但仍直接依赖 GORM、execution.State 和 breakerCoordinator；属于同包组件，不是完全独立存储层 |

### 3.4 调度、溢出和治理队列

| 文件 | 当前职责 | 应守边界 |
| --- | --- | --- |
| `delayqueue.go` | 把 Task 接入延迟堆、到期投递和必要的重新排期 | 堆/唤醒算法在 scheduler；这里只决定任务到期后怎么交回执行链 |
| `scheduler_budget.go` | SchedulerLimits、residentSet；非阻塞准入、转移、释放和驻留计数 | 数量预算不等于 RSS；不执行 SQL |
| `scheduler_refill.go` | 持续 pending 补充、阶段轮转/keyset 游标和暂停 delay 卸载 | 由 refillMu 串行化，查询谓词归 taskstore；独立于 RecoverQueue |
| `scheduler_stop.go` | 按注册阶段计数持久 pending | 共用停止 deadline，包含启动前已接受任务；查询失败不能报告排空 |
| `scheduler_spill.go` | spillQueue：轮次成员索引、去重、TakeBatch/Restore/Put 与公平选择 | 无 Engine 回引用；取出的有限批次交由调用者持有，仍计入 resident 责任，锁不跨 SQL/worker 投递 |
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
| `breaker_events.go` | 当前熔断事务的有界独立事件批次与安全出口 | 先完整发布状态及暂停，再交给 pkg/events 的有界非等待出口；外部 callback 不在执行栈运行 |
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
| `downloads.go` | SetDownloadResources、下载器初始化、站点/阶段配置装配及公开查找入口 | 覆盖配置与解析快照归 fetchruntime.Downloads |
| `account.go` | 账号 Manager 注入、状态和账号 HTTP/HTML/浏览器公开适配 | 凭据与账号生命周期归 pkg/account |
| `account_pool.go` | 账号池引用、准备账号、按池抓取的公开入口 | 池选择和资源归还委派账号资源能力，不复制账号状态 |
| `account_events.go` | 账号健康事件订阅、告警与节流 | 取消订阅由 Engine 生命周期持有；告警节流由 accountAlertGate 拥有，Stop 仍按原顺序解绑 |
| `identity.go` | 身份 Manager 注入、查询和会话身份 context 适配 | 档案与绑定状态归浏览器身份组件 |
| `governance.go` | 运行策略解析/安装、声明与 schema 检查、任务 context/账号池准备 | controller 与独立的延迟 map 副本在同一 e.mu 临界区发布；后台策略事务不属于 Engine |
| `restricted.go` | 特定运行场景的能力限制检查 | 统一执行限制入口，不能在各抓取路径另造互相冲突的开关 |

### 3.7 追踪、归档、快照与通知

| 文件 | 当前职责 | 应守边界 |
| --- | --- | --- |
| `trace.go` | Trace 步骤缓冲/锁/终结、context 及公开查询/配置适配 | 单次记录器拥有异常判定与有界缓冲，委派文件存储 |
| `trace_data.go` | 有界输入预检与JSON 编码 | 序列化异常转为证据标记，不改变 handler 结果 |
| `trace_service.go` | traceService 的文件持久化与保留期维护 | 独占 tracestore；无待选任务状态，文件锁归 tracestore |
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
| `engine/internal/scheduler` | Registry 的池表/最后投递时间/停止位；Delay 的堆、锁、唤醒、timer 和待交付值快照 | 泛型接收任务值，不导入父包；Delay.Snapshot 只浅复制容器，指针值仍由调用方拥有；Next 只允许一个 dispatcher 消费者。Task 的持久化和暂停仍由 Engine 协调 |
| `engine/internal/execution` | State 的 unresolved map、锁、快照和 Retry 背压；ConfirmSuccess 的取消后成功结算边界 | 已实质拆分，但职责是持久化确认状态，不是“所有任务执行”。依赖 GORM/taskstore 错误语义，后续不应随名称向此处堆执行功能 |
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
| 驻留成员、ID 索引及站点/阶段计数 | residentSet，使用自己的 mu | 短锁维护准入/转移/释放；不跨 SQL 或用户回调；取出回灌的批次仍占令牌 |
| 回灌过程、refillCursor/refillStage | Engine 的 refillMu | 串行化回灌与持续补充，允许跨该流程的 DB 调用；不等同于 e.mu、pauseMu 或容器锁，不可替换为全局状态锁 |
| residents 配置指针、schedulerStarted | Engine 装配阶段 | SetSchedulerLimits 必须与提交、启动及停止串行；先发布 started 再启动 drain goroutine，运行中不替换预算 |
| 活池、延迟任务、溢出任务 | Registry、Delay、spillQueue | 关闭后不得新建池；回灌不重复计轮次；未投递任务要计入停机统计 |
| 未确认任务写入 | execution.State | Retry 成功本身不删除条目，确认后的 Forget 由协调调用方负责；取消时 Remember 保留可见性，不能靠重复调用 handler 补写 |
| 暂停 map、广播 channel、恢复时间 | Engine 的 pause 状态 | `breakerMu → pauseMu → (overflow.mu 或 e.mu)`；pause 锁不跨数据库调用 |
| detector、trial、证据和报告串行门 | breakerCoordinator | 纯内部变更在 coordinator 方法内；涉及暂停/投递的事务由明确协调入口发布 |
| 站点声明 | siteCatalog，继续使用既有 breakerMu | 目录组件拥有声明内容并提供脱离内部存储的快照；为保持协调顺序不引入第二把锁 |
| 单次 trace 与归档 | Trace、archiveBuffer、attemptExecution | 尝试结束和 panic 路径都终结；账号敏感内容不进入原始页面归档 |
| 抓取资源登记及下载配置快照 | fetchruntime.Resources/Downloads | 短锁取依赖，网络工作和用户回调不在登记锁内执行 |

Task 的私有 deliveryVersion 用于将持久补充出的轮次带到认领条件，不作为公开任务字段。运行中调度上限不热变更；有界工作集不能以丢任务换内存，持久 pending 必须由连续补充发现。停止结果描述在途资源安全，不能把内存 resident 下降解释为数据库 backlog 清空。设置和默认值见 [资源预算手册](../configuration/RESOURCE_BUDGETS.md)。

暂停下的持久 pool/spill/delay 任务可交还数据库以让出预算：预算模式允许 worker 取出引用，但 stageHandler 在 breaker/业务准入前拦截；heap 提取先快照暂停状态再取堆锁，避免锁序反转。flow 前置等待同样释放持久引用，不占住解除等待所需的最后一个槽。Stop 的持久计数也覆盖 ApplyRegisterStage 前已接受的远期任务。

下载预算装配通过 SetDownloadResources 交给 fetchruntime.Downloads 的 ConfigureResources/Resources：首次构造基座时冻结限额，两种下载基座及 With 派生实例共享同一 DownloadBudget。Engine 不实现磁盘清点、输入解码或检查点算法。

`refillPendingLocked` 的前置条件是持有 refillMu。回灌批读与扫描使用 100ms 时间片派生的查询 context；这不是所有准备写入、锁等待或整个停止流程的硬超时保证。停止通过生命周期取消及 tickerWG 等待在途回灌。

Stop 不只是取消 context：还要关闭池登记并排空 worker、等待 ticker、统计溢出/延迟/未确认写入、结算熔断报告。worker 与 ticker 共用入口 deadline；统计必须在 ticker 等待结束后执行，避免漏掉它最后保留的任务或重复报告已确认写入。超时后的统计仅代表最后观察值，不表示后台已静止。Delay.Snapshot 保留任务，由 Engine 在锁外按阶段汇总，不清空堆。任何后续抽取的维护循环或服务必须明确如何加入这个关闭协议。

`NewEngine` 使用独立的 background context，并在构造时预热去重、登记并启动延迟 dispatcher；它不是无副作用的纯容器构造。`ApplyRegisterStage` 再接入回灌、治理队列、证据清理、采样与回收循环，不能与 Stop 并发重复启动。池登记表停止位和 Engine context 都不可恢复，同一个 Engine 不支持停止后重启。

池锁也有具体限制：`Registry.GetOrCreate` 在登记写锁内运行创建回调；`ReapIdle` 在同一锁内检查空闲、调用 pool.Stop 并摘除。创建回调不得重入登记表；Engine 的 created/reaped 事件均在登记锁外发布。`Registry.Set` 是测试/历史路径，不检查 stopped，不能把它当作生产停机后的重新登记入口。

`StopStats.Unfinished` 的池部分来自 submitted − completed − failed，已经包含尚在队列中的任务；`Queued` 又展示队列长度，并加入 spill/delay 存留，再与该阶段 durable pending 计数取较大值，避免简单相加重复计算卸载前后同一任务。两列不是可直接相加的互斥人数。未确认写入还会追加到 Unfinished；超时快照可能重叠，不能据此宣称获得了全局去重的任务总量。即使 stats 为空，ticker 未退出、持久 backlog 查询未确认或最终熔断快照写入失败仍使 drained=false。熔断 stop 记录与最终 flush 仅在 ticker 已退出后执行，并使用同一入口 deadline。

排空范围也须区分：错误转发 goroutine 和 track 活动消费者响应 Engine context，但没有加入 tickerWG，Stop 不等待它们的 done；它不保证所有观测消息已消费。workerpool.Stop 返回首次任务计数快照，不是可反复查询的新结果。直接调用 Engine 公共管理/提交接口的外部 goroutine 也不自动进入 Stop 的等待组，宿主必须先停止这些生产者；App 的 HTTP 准入和业务收尾就是外层协调的一部分。

execution.State.Retry 在当前调用者中重试持久化，退避从 100ms 增至最多 2s；它不另起后台 goroutine，也不重跑 handler。ErrRecordNotFound/ErrChanged 直接返回，取消才登记 unresolved；调用方还可主动 Remember 模糊写入。终态确认后显式 Forget，非幂等的 retry 加一则只写一次，确认不明时保留未结算记录。

`execution.ConfirmSuccess` 只包装已成功返回业务后的幂等、版本条件写入（结束时间及 success 终态）。正常运行沿用原 context 的持久化重试；取消已经发生或与写入竞态时，保留执行版本等 context 值，给该写入一次最多 2 秒的独立确认窗口。ErrChanged/ErrRecordNotFound 不因取消而再试，确认失败仍留未结算状态；不包装业务 handler、retry 加一、取消失败或 panic，不增加后台 goroutine，也不改变锁序。两个写入分别有自己的确认窗口，Engine.Stop 截止时间仍独立；若 Stop 先超时，App 仍须保留依赖，不能把它写成统一硬关停时限。

配置的“拥有”也包含输入复制：SetSiteDownloads/AddStage 保存覆盖项的独立值，InstallGovernance 保存延迟 map 副本；调用方随后复用输入不会改变已登记状态。显式重新登记/安装仍是更新入口，不改变阶段优先于站点的解析顺序。

事件是已发布状态的有限观察值。创建池、队列轮次和熔断事务释放状态锁后交给安全异步边界；外部观察者不在执行调用栈运行。观察者执行时状态可能已经推进，不能要求每条事件都对应查询时的即时状态。探针结算仍重验当前事件/控制轮次和取消状态，人工操作通过控制 API 协调，不依赖事件回调发生时机。队列可丢弃，可靠恢复与历史保存另走事务协议。

## 7. 测试归属与新增代码规则

- 内部包算法和并发状态直接在所属包测试；Engine 测试验证多个组件组合后的执行协议。
- `component_boundaries_test.go` 防止同包组件重新持有 Engine 或完整配置；组件测试覆盖提交部分成功、任务流事务、证据清理排空及站点快照隔离。
- 根包中的 `fakedb_test.go` 是共享假库，不等同真实 SQL 执行；`sqlshape_test.go`/`sqlreserved_test.go` 检查 SQL 形状与列名，`*_mysql_test.go` 验证实际事务/CAS/重启恢复。
- `attempt_*`、`stage_runversion_test.go`、`persistence_test.go` 验证尝试与持久化确认；暂停、溢出、延迟、轮询相关测试验证调度协作；`breaker_*_test.go` 验证恢复证据与结算链。
- 保留同包测试访问必要内部状态的能力，但不能为了测试创建无生产用途的转发 API。若组件抽包，迁移算法单测，同时保留关键 Engine 集成用例。
- 新能力先确定执行状态属于谁；Engine 只增加公开适配和跨组件编排。只处理 SQL 的实现优先归存储组件，纯策略优先保持无运行依赖。
- 不把整个 Engine 传给新子组件。现有 Fetcher 公开契约接收 Engine 是当前公开入口，不能作为内部组件继续持有 Engine 的理由。
- 拆分涉及任务状态、锁序、取消、背压或关停时，除 test/vet/build 外需要 race；涉及 SQL 事务还需隔离 MySQL 回归。
- `config_ownership_test.go` 和 fetchruntime 的 `download_config_test.go` 验证输入隔离；`lifecycle_review_test.go` 验证 ticker 收尾统计与延迟存留；`breaker_review_cancellation_test.go` 验证结果结算协议阶段之间的显式取消；提交/任务流测试覆盖错误日志、批量交接异常和事务中途取消。

## 8. 收口结果与保留边界

`resilience_mysql_test.go` 归属 Engine 跨组件集成验证：使用真实调度、任务认领/终态、启动恢复和 Stop，旧子进程退出后才启动恢复进程。验收校验器核对原始任务集合、逐任务执行次数、业务结果和时间约束，负向用例用真实 MySQL 事务篡改证据并回滚。持续负载关闭启动恢复队列以单独验证持续回灌、驻留预算和中途进展。父进程独占随机测试库并清理，DROP 后按精确库名确认不存在；另用故意断言失败的子进程覆盖失败清理。合成业务结果/执行次数/控制表只在测试库中，不进入框架迁移清单。不把数据库积压等同于内存驻留，也不把可重放恢复等同于业务 exactly-once。运行方式及边界见 [开发工具手册](../architecture/DEVELOPMENT.md#单实例持续负载与进程崩溃恢复)。

| 原审查问题 | 当前实现 | 后续约束 |
| --- | --- | --- |
| engine.go 职责混合 | 契约、依赖适配、站点目录、控制入口、生命周期分文件；构造负责装配并启动既有初始化任务 | 分文件只是可读性改进，实际边界由组件状态与操作接口保证 |
| 提交 SQL 与投递混合 | submissionStore 负责持久化准备和认领；submissionPlan 显式交付成功/未读记录的投递责任 | 部分失败也必须处理成功项；重复项在投递后回填轮次信息 |
| 任务流事务挂在 Engine | taskFlowService 只依赖数据库，统一版本、父子登记、认领与查询 | flow 行锁和事务范围不能随重构拆散 |
| 熔断状态分散修改 | coordinator 收回内部状态操作；Engine 根据值结果执行暂停发布、任务操作和探测生命周期 | 原 breaker → pause → overflow/e.mu 顺序不变，不引入互相调用的新锁 |
| 追踪/归档混合记录与维护 | 单次记录器 + 窄配置服务 + 统一维护循环 | 动态配置通过门面按调用取值；维护继续纳入 tickerWG，不能先报告排空再关闭数据库 |
| 站点及告警状态分散 | siteCatalog 拥有声明快照，accountAlertGate 拥有限频状态 | 站点继续使用原协调锁；不能用修改返回 map 的方式改变引擎声明 |

Task/Trace、Fetcher、StageConfig 以及 Engine 公开 API 保留。stageHandler 仍协调账号准入、执行轮次、终态确认和熔断结算；pause.go 仍拥有暂停信号及跨状态发布。它们是需要显式顺序的执行协议，不是应该按文件名机械拆包的剩余工作。

本轮优先采用同包组件，没有为了缩短目录列表而新增重复子包。下一步新增功能必须使用组件语义接口；若出现新的独立消费者或生命周期，再评估抽包，而不是重新导出内部 map、锁或完整 Engine。

历史复核与修复记录位于源码仓库 `docs/history/reviews/2026-11-13-engine-second-review.md`；它不随使用手册分发，当前职责以本册为准。

### 执行证据身份

`evidence_identity.go` 为每次成功认领建立 context 内的 UUID，同轮重试共用，恢复/重投/轮询重新认领则换号；只标识观测证据，不参与 taskstore 终态栅栏。`attemptExecution` 装配时把同一身份交给 Trace 和 archiveBuffer，缓冲与锁仍由各自拥有。traceService 查询最新有记录的执行，保留末 500 步并明确截断；旧 SQL 历史不再读取，不虚构轮次。

## 九项诊断修复后的治理取消契约

`batch.go` 负责批处理生命周期：每次查询绑定 Engine context，每批和每条回调准入前检查取消，零批间停顿也不能跳过取消。`errorqueue.go`、`repeatpoll.go`、`recoverqueue.go` 的条件重置同样绑定生命周期；已经成功重置的任务继续既有投递/溢出收尾，不在提交中间插入新的提前返回。`queuestats.go` 的后续采样也使用该 context。取消不等于回调完成，批处理仍等待已经准入的 worker 返回，tickerWG/Stop 的排空判断不变。

`batch_cancel_test.go` 覆盖零暂停、同批未准入任务、已取消无查询/重置、SQL context 传播和在途回调排空；公开方法签名、条件更新谓词和队列锁顺序不变。



## Trace 文件职责与状态所有权

`trace.go` 拥有单次尝试的缓冲、步骤序号、异常标记与终结幂等锁；`trace_service.go` 拥有一个文件 Store，没有手动选择表或选择锁。Engine 用 sync.Once 初始化组件；声明期配置不允许运行中改写。每次尝试从配置取得 off/errors/all 策略；Trace 自己拥有 keepAll 标记与终结缓冲。

默认仅保存错误、panic、显式 Fail/Warn；干净成功不落盘。正常关停取消不单独构成异常；all 模式仍保留所有尝试。`ListTraceExecution` 为唯一查询入口，查询最新已保留执行的尾 500 步，不能据此推断任务当前状态。文件格式、容量、发布、路径安全、清理均归 `internal/tracestore`；Engine 无 Trace SQL。

维护沿既有 evidence maintenance ticker 生命周期执行，取消可中断清理；尝试结束在 worker 收尾内同步发布，不引入脱离 worker 的后台写队列。Trace 存储失败仅告警，不改变任务结果。默认保留七天，磁盘容量预算独立生效。旧 MySQL 验证记录仅说明迁移前实现，当前行为以文件回归为准。

恢复只接受 `Snapshot.ValidateCurrent` 验证过的控制版本及精确窗口；旧控制升级与近似计数回退已删除。后台重投只提供 RetryTaskVersioned，始终检查补偿版本。

Engine 的 SetEventsEmitter 将未知消费者隔离到固定异步边界；Stop 只请求关闭自己创建的包装器，不等待任意 observer。`breaker_probe_evidence_test.go` 验证探针证据与持久投影的边界/脱敏一致；`breaker_events_test.go` 验证阻塞或 panic observer 不阻止结算，重入操作通过已提交状态进行。可靠快照、窗口和追加历史仍为同步存储协议，依赖共享 `pkg/incident`，不依赖 obs 服务。


### 熔断恢复初始化的排空归属

InitBreakerRuntime 在访问存储前，通过 Engine.mu 的短准入区检查取消并登记 tickerWG 初始化在途位；Stop 在同一锁内取消，再等待。这把恢复 Load/flush 和后续维护循环的交接连成同一生命周期，锁不跨数据库调用。恢复 context 同时响应调用方和 Engine 取消。存储未返回时 Stop 如实返回未排空，不能据此关闭共享 DB；已经停止的 Engine 不再访问恢复存储。回归见 breaker_startup_lifecycle_test.go，覆盖阻塞读取、停止后拒绝和异步恢复观察者触发 Stop 时的在途写入。


并发恢复初始化由 breakerCoordinator 的 breakerRestoreGate 可取消串行化：gate 包含加载、状态发布、可靠 flush 与维护循环交接，等待者也纳入 Engine 的初始化在途位。该 gate 只约束初始化，不复用控制锁/存储写 gate；等待或访问存储时均不持 Engine.mu、breakerMu。初始化成功标志在可靠 flush 后发布，避免并发调用越过尚未完成的持久化确认。

恢复 gate 的首次创建由 breakerRestoreOnce（sync.Once）拥有，不为通道初始化直接解锁 breakerMu；事务事件派发守卫保持原约束。

### Handler 退避回归归属

`retry_backoff_test.go` 验证策略极值、实际 Handler 重试计数/单次认领、取消时认领释放及真实 worker Stop 退出。`stage_test.go` 保留首次不等、只在尝试之间等待和末次失败不空等的契约；Equal Jitter 的最低等待为封顶指数值的一半。`persistence_test.go` 继续验证 retry 加法回执不明不能重放。该退避不用于数据库确认重试、账号冷却、熔断探测或治理层 Retry-After。
