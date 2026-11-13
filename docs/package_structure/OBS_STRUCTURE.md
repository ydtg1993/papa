# Obs 目录结构与职责细则

> 面向：维护观测输入、报告分析、熔断证据和报告产物的开发者。
> 总索引：[目录职责说明书](README.md)。配套：[观测使用手册](../OBSERVABILITY.md)、[总体架构规则](../ARCHITECTURE.md)、[开发手册](../DEVELOPMENT.md)。
> 本文说明当前源码归属；配置、报告模式与业务扩展的使用方法以观测手册为准。

## 1. 定位与目录

`obs` 把事件、日志证据及框架状态组织成可解释的报告。它不拥有运行中的 Engine，也不决定暂停、恢复、任务重投或应用启停。事件生产与落盘归 `pkg/events`，常驻报告的定时器、生成互斥及退出等待归 `internal/reportrunner`，App 只接线；对应说明见 [Pkg 分册](PKG_STRUCTURE.md)、[Internal 分册](INTERNAL_STRUCTURE.md)（App 见其中的 [app 章节](INTERNAL_STRUCTURE.md#app)）。

当前共有 **8 个包、30 个生产 Go 文件、42 个测试 Go 文件**；没有独立 HTML、JS、CSS、模板或磁盘 fixture 目录。测试中的 JSONL、gzip、日志和报告产物在测试代码中构造。

```text
obs/
├── obs.go                 # 报告契约、生成流程及总体判定
├── engine.go              # 引擎状态的观测值
├── inputs.go              # 自证信息、事件覆盖与停机区间
├── mode.go                # 模式与触发方式解析
├── operational.go         # 历史窗口、可观测运行时长
├── load.go                # 单窗口事件与日志加载
├── daily.go               # 本地日历日输入拆分
├── analyze/               # 框架状态分析节、只读查询适配
├── inputbudget/           # 输入预算、取消读取与限额错误
├── eventlog/              # JSONL 与轮转文件读取
├── incident/              # 熔断诊断值、错误分类与证据整理
├── logidx/                # logfmt 证据索引
├── pattern/               # 通用异常模式
└── report/                # Markdown / JSON / AI brief 与产物保留期
```

## 2. 根包逐文件职责

| 文件 | 当前职责 | 维护边界 |
| --- | --- | --- |
| `obs.go` | `Input`、`Options`、`Report`、`Section`、`Pattern`、检查/问题/证据契约；`Generate` 顺序调用分析节与模式，汇总降级、排序、合成问题及总体判定 | 只协调本次报告；单节返回错误成为降级信息，其余节继续。数据不足不宣称通过；info 与 skipped 不承担异常通过/失败语义 |
| `engine.go` | `SiteState`、`EngineState` 与 `Inactive`，保存来源、采样时刻、暂停、池数量和恢复上下文 | 这些是观测值，不是 Engine 的可写控制接口；来源缺失时不凭空判断空闲 |
| `inputs.go` | `Inputs` 自证字段；`ComputeEventCoverage`、`ComputeDowntimes`、`TotalDowntime` 计算可见事件覆盖、断层和停机区间 | 复制待排序的事件切片；只根据看得到的边界配对，不假定窗口外发生过停机 |
| `mode.go` | `Mode`、`Trigger` 与 `ResolveMode`，区分 snapshot / acceptance，解析 auto 及非法值 | 这里只解释模式；配置默认值和 CLI flag 优先级分别归 config 与命令适配层 |
| `operational.go` | 历史窗口识别、历史状态检查的跳过规则、`ObservedRuntime` 的按 run 区间合并 | 当前 DB/引擎快照不能冒充历史期末；按检查 ID 区分当前状态的处理不应扩展为真实站点分析逻辑 |
| `load.go` | `SourceConfig` / `Sources`；发现及读取事件、按 run 筛选、重算窗口内缺口、转接日志索引 | 事件是指标来源，日志只是证据；日志窗口有前后 5 分钟富余。run 筛选重算 Count/Gaps；Files/Unparsed 反映文件读取范围，Duplicates 是时间窗口内、筛选 run 之前的重复数 |
| `daily.go` | `ConsumeDailySources` 有预算地扫描、分桶和逐日消费；`LoadDailySources` 提供收集式入口 | 尊重显式窗口；次日零点只进入下一天。先预检源输入再进入消费回调；保留每日报告重复/缺口口径，不把全窗口元信息复制给每一天 |

`Input` 的“只读”是扩展契约，不是类型系统强制隔离：其中仍含 `*gorm.DB`、map 和 slice。`Generate` 没有为每个分析节深拷贝全部输入，也不为调用方加并发锁；业务 `Section` / `Pattern` 应只读输入，不通过借来的 DB 写业务状态。框架分析节可追加本次调用的 `Input.Degraded`，由生成流程合并进结果。

## 3. 子包逐文件职责

### 3.1 `eventlog`：事件文件读取

| 文件 | 职责 |
| --- | --- |
| `eventlog.go` | 只读包契约及 `Result`：事件、文件数、坏行、重复数和同 run 序列缺口 |
| `discover.go` | `Discover` 根据活动文件名识别 lumberjack 时间戳轮转备份及 `.gz`，稳定排序；不把相似名字当备份 |
| `read.go` | JSONL/gzip 文件读取与关闭、当前事件 schema 版本校验、闭区间窗口、`(run_id, seq)` 去重、稳定排序、`CountGaps` |

依赖标准库、`pkg/events` 与 `obs/inputbudget`。坏 JSON 行计数后继续；文件打开、解压或读取失败返回错误。序列缺口以实际保留事件为范围，不能证明窗口之前、之后或整段缺失的情况。

窗口过滤先于去重，同一 `(run_id, seq)` 保留输入顺序中窗口内的第一条。事件、去重键、物理行及解压输入均受 inputbudget 限制；ReadContext 支持合作取消。返回的事件仍是完整切片，编码字节预算不是精确 Go 堆字节保证。输入文件仍可能被 writer 追加或轮转，本包没有与 writer 协商一致快照或排空的接口。

### 3.2 `logidx`：有界日志证据

| 文件 | 职责 |
| --- | --- |
| `windows.go` | BuildWindowsContext 单次扫描构建多个日报窗口；跨窗口保留总量受限，保持各窗口 MaxEntries/Truncated 口径 |
| `logidx.go` | `Entry`、`Signature`、`Config`、`Index`；读取普通/gzip 日志，限条数聚合，维护有限样本与确定性排序 |
| `logfmt.go` | logfmt 键值、引号及时间解析；日志来源与行号、额外字段、UTF-8 安全截断和凭据脱敏 |
| `signature.go` | 将消息中的 URL、动态 ID、数字归一为签名，限制签名长度，供聚合复用 |

聚合 map 和样本仅属于一次 `Build`；没有后台缓存或消费者。文件和 gzip reader 在读取路径内关闭。默认最多接收 20000 条、每个签名 3 个样本；截断通过结果标记暴露，不据样本估算吞吐或失败率。直接 `Build` 的默认时区为 UTC，`obs.LoadLogSummary` 会显式传入来源时区，缺省为本地时区。

### 3.3 `analyze`：框架分析节

| 文件 | 职责与边界 |
| --- | --- |
| `generic.go` | `Generic` / `Thresholds`；运行边界、队列、失败分组、耗时及框架任务状态，生成表格、检查与问题。失败证据按任务和分组匹配，不将一条日志泛用到所有失败组 |
| `engine.go` | `EngineSection`；暂停来源、阶段池、治理队列恢复与空闲解释，区分状态提示和异常检查 |
| `engine_load.go` | 离线 `LoadEngineState`；优先读取框架配置中的站点快照，再用事件补采样/池/恢复上下文，说明当前 DB 投影与历史窗口的差异 |
| `backlog.go` | `BacklogSection`；pending / processing 分布、老化、吞吐及 ETA，输出可供 AI brief 复用的 `BacklogSummary` |
| `querier.go` | `Querier` 的只读接口和 GORM 实现；站点状态、任务聚合桶、长时间 processing 查询。pending 年龄按 created_at，processing 按 updated_at |
| `integrity.go` | 事件覆盖完整性检查；与明确停机边界吻合的断层不当成丢事件，其余断层保留风险证据 |
| `breaker_reports.go` | 通过 `internal/breakerstore` 读取持久历史，关联事件 ID；报告引用同一份熔断事实，不从可能缺失的事件重造故障窗口 |

分析器依赖 `obs` 的契约、`pkg/events`、`models`、GORM、脱敏和 `incident` 的分类；持久熔断历史是有意的 `internal/breakerstore` 只读适配依赖，不导入 Engine 或 App。`Querier`、`Input.DB` 由调用方提供，本包不建连接、不迁移、不关闭调用方连接。历史被截断时只报告不完整，不能从最后一条可见记录推断日报截止仍在保护。

### 3.4 `incident`：熔断证据值与诊断

| 文件 | 职责与边界 |
| --- | --- |
| `incident.go` | `Sample`、`Capture`、`Group`、`Report`；样本清洗、保留错误码的文本模板、窗口分组、分析和纯文本输出 |
| `classify.go` | 根据 kind/message 给出分类、待验证解释和验证方向；分类不能当成已经证实的根因 |
| `failure.go` | `Failure` 与 `ClassifyError`；优先识别 core、account、browser、identity、htmlfetch 的结构化错误，再做文本回退，生成稳定指纹及可读代码/阶段 |
| `lifecycle.go` | `Lifecycle`、`Transition`、`StageCheck`、`FailureGroup`；有界脱敏副本、完整任务与只读检查的证据区分、诊断分析及文本表示 |

这里的 lifecycle 是可持久化的诊断数据，不是运行状态机。实时状态、锁、轮次和暂停归 Engine/熔断组件，事务写入归 `internal/breakerstore`；参见 [Engine 分册](ENGINE_STRUCTURE.md)与 [Internal 分册](INTERNAL_STRUCTURE.md)。`Clean` 创建脱离源状态的嵌套副本、限界证据及历史，并保留截断标记。对 browser/account 等包的依赖用于识别错误，不创建浏览器、不取账号、不执行探针。

### 3.5 `pattern`：通用异常模式

`pattern.go` 保存 `Builtin` 的确定性顺序及五类模式：事件缺口、缺少正常停止证据的重启、队列不消退、熔断噪声和阶段未注册。只读 `obs.Input` 的事件、日志、引擎上下文，输出 `Problem`；暂停或空闲不应误判成失活，也不能抹掉独立错误证据。业务模式由业务模块实现 `obs.Pattern` 并登记，不把固定站点目标写入内置库。

### 3.6 `report`：产物与文件边界

| 文件 | 职责与边界 |
| --- | --- |
| `markdown.go` | 报告 Markdown/JSON、模式/判定/表格渲染（缺失模式显示“未指定”，不推断旧版本）、诊断副本脱敏、Markdown 转义、稳定目录名、四份产物的 `Write` 和单文件原子替换 |
| `aibrief.go` | `AIBrief` 结构、从同一报告派生的问题/假设/下一步/验收边界，以及 Markdown/JSON 两种表示；不执行建议动作 |
| `retain.go` | `DayName` 与 `Prune`；仅处理报告根的直接子目录且要求存在 report.md，按日报日期或旧目录修改时间判断过期 |

`DirName` 保留正常 run_id，特殊点段、尾点和 Windows 设备名归一为普通单段名称；CLI 写前另做目录边界校验。

渲染使用诊断副本，不把脱敏后的值写回分析输入。`Write` 创建临时文件、写入、同步、关闭后 rename，并清理本次临时文件；**原子性按单文件保证，四份产物不是一个事务**，中途失败不意味着已更新文件会回滚。并发生成与退出等待由上层 Runner 协调，`report` 自身不保存目录级写锁。

`Prune` 是显式文件删除入口：负保留期不删除，零值回退默认 7 天。分析、读取和渲染本身不调用清理；CLI 或 Runner 决定何时调用 `Write` / `Prune`。CLI 指定 `--since`、`--until` 或 `--run` 时只生成、不执行保留清理，保证显式历史产物可以取走；无这些筛选的 CLI 生成和自动报告仍按原保留期清理。历史产物之后仍可能被自动清理，需要长期保存时请使用独立 `--out` 目录。后台的文件枚举、读取、下载与页面选择归 `admin/reports`，见 [Admin 分册](ADMIN_STRUCTURE.md)。

### 3.7 `inputbudget`：输入计量与取消

| 文件 | 职责 |
| --- | --- |
| `input.go` | Limits 默认值、LimitError/ErrLimit、Meter、固定缓冲逐行读取；只依赖标准库，不拥有文件生命周期 |

预算按调用与源类别计量，Meter 不允许并发共享；超限向调用方返回，不能静默生成不完整的验收结论。便捷读取入口使用有限默认值，Context 入口支持合作取消；配置见 [资源手册](../RESOURCE_BUDGETS.md)。

## 4. 依赖、状态与生命周期

调用方向是宿主/CLI → 输入加载及分析器 → `obs.Generate` → `obs/report`。Go import 方向按文件职责划分：`obs` 根包依赖低层 `eventlog` / `logidx`，`analyze` / `pattern` / `report` 依赖根包；因此不能让根包反向登记这些内置实现。CLI 和 Runner 负责选择具体分析节与模式。

| 对象或资源 | 拥有者 | 生命周期约束 |
| --- | --- | --- |
| 事件 writer、序列、缓冲和落盘 | `pkg/events` 与其宿主 | `obs/eventlog` 仅打开读取快照，不管理 writer |
| 本次输入、索引、报告、聚合 map | 当前调用 | 无跨调用全局可变分析状态；扩展必须遵守只读输入契约 |
| 运行中 Engine 状态及 DB 连接 | 宿主 | 传快照和借用连接；观测不接管其锁或关停 |
| 报告 ticker、串行生成、停止等待 | `internal/reportrunner` | App 装配和等待；不能在 `obs` 再开一套常驻循环 |
| 事件输入临时分桶、spool 目录与句柄 | obs/daily.go 的单次 ConsumeDailySources 调用 | 有界预检后逐日消费；正常/错误/取消时关闭并清理，强杀可能遗留；不是事件 writer 或最终报告产物 |
| 正在读取的文件、gzip reader、报告暂存文件 | 对应输入/产物函数 | 调用内关闭/清理；最终报告写入和保留期删除归 report |

取消是协作式且覆盖范围有限：`Generate(ctx, ...)` 把 context 传给各 `Section.Build`，内置 GORM Querier 使用 `WithContext`；Generate 自身不检查 ctx 来提前停止后续节，节返回取消错误仍按降级处理。`Pattern.Match` 没有 context 参数；`LoadSourcesContext` / `ConsumeDailySources`、eventlog/logidx 的 Context 入口已把取消传入文件发现、逐行和解压读取，不接收 context 的便捷入口使用 Background。`report.Write` / `Prune` 仍没有 context。上层停止等待超时不能证明在途分析或文件操作已经退出；不得据此关闭其仍借用的依赖，或宣称整个报告流程能立即取消。

## 5. 测试分工与新增功能落点

| 位置 | 现有测试关注点 |
| --- | --- |
| `inputbudget/input_test.go`、`eventlog/budget_test.go`、`logidx/budget_test.go`、`daily_budget_test.go` | 精确限额/超一单位、取消、逐日等价口径、预检失败不发布及临时文件清理 |
| 根包 `obs_test.go`、`policy_test.go`、`mode_test.go`、`operational_test.go` | 节/模式汇总、降级、unknown、info/skipped、模式解析、历史状态隔离、可观测时长 |
| 根包 `inputs_test.go`、`generated_at_test.go` | 事件覆盖、停机配对和生成时刻 |
| 根包 `load_test.go`、`load_slack_test.go`、`load_run_scope_test.go`、`daily_test.go` | 筛选、日志边界、交错 run/乱序/重复及 gzip、逐日窗口；`TestLoadSourcesGapCountFollowsRunAndWindow` 钉住缺口作用域 |
| `eventlog/*_test.go` | 备份发现、窗口、排序、去重、缺口、坏行（包括无版本、零版本及未来版本）、gzip/CRLF 与文件错误 |
| `logidx/*_test.go` | 解析、签名、有限样本、确定性、凭据与历史原文脱敏 |
| `analyze/*_test.go` | fake Querier、GORM fake 与 DryRun 的分析及 SQL 口径；`breaker_history_limit_test.go` 专门防止截断历史推断截止状态，`zz_test_helpers_test.go` 仅为测试适配 |
| `incident/*_test.go`、`pattern/pattern_test.go` | 分类不过度归因、错误码保留、证据限界/深拷贝/脱敏、验证强度、暂停/重启/队列模式 |
| `report/*_test.go` | Markdown 转义、JSON、AI brief 一致性、自证/时间/可信度、单文件替换和保留期 |

新增事件读取规则放 `eventlog`，日志格式和签名放 `logidx`；新增通用指标或 DB 聚合放 `analyze`，跨证据异常解释放 `pattern`，熔断证据分类与诊断值放 `incident`，产物格式与保留规则放 `report`。新增调度触发或关停等待改 Runner；新增后台交互改所属 `admin` 功能包；真实业务分析留在业务模块。

本分册是源码盘点，不声明测试已执行。修改时按受影响包选择 `go test ./obs/...`，联动检查 `cmd/papa`、`internal/reportrunner` 和相关后台功能；假库与 DryRun 只证明查询/分析分支及 SQL 形状，不能替代真实数据库语义或浏览器交互验收。
