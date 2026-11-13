# Papa 目录说明书

> 面向：维护 Papa、判断新增代码归属、检查状态所有权与依赖边界的开发者。
> 本目录是当前源码目录职责的统一说明。框架用法见 [手册导航](../README.md)，长期架构约束见 [ARCHITECTURE.md](../architecture/ARCHITECTURE.md)，构建与验证见 [DEVELOPMENT.md](../architecture/DEVELOPMENT.md)。
> 适用版本：当前工作区的独立 v4；历史重构/兼容过渡方案不覆盖本说明书，也不表示 v3 或更早接口仍受支持。

## 1. 阅读路径

先看 [架构图解](../architecture/ARCHITECTURE_DIAGRAMS.md) 的静态组件图与动态数据流图，再按下表定位具体包的职责、状态和接口。图中的运行关系不替代本说明书的 Go 依赖边界。

| 分册 | 覆盖范围 | 主要回答的问题 |
| --- | --- | --- |
| [根目录与基础包](ROOT_STRUCTURE.md) | 根包 papa、config、core、models、docs、仓库配套文件 | 公开门面、配置和值契约、持久模型分别归谁；文档如何组织与分发 |
| [Admin](ADMIN_STRUCTURE.md) | admin 全部功能包及 admin/internal/httpjson | HTTP、页面资源、前端状态、鉴权、表格和审计如何归属 |
| [Engine](ENGINE_STRUCTURE.md) | engine 根包及 engine/internal 的 execution、fetchruntime、scheduler | 执行、调度、持久化、熔断的同包组件边界，以及锁序和排空 |
| [Internal](INTERNAL_STRUCTURE.md) | internal 全部包，包含 [app 组合根](INTERNAL_STRUCTURE.md#app) | 装配与声明、运行服务、存储适配、通用执行设施及测试基础设施怎样分工 |
| [Pkg](PKG_STRUCTURE.md) | pkg 的账号、浏览器、HTTP、下载、治理、出口与辅助能力 | 可复用能力的公开接口、状态、锁和资源释放由谁负责 |
| [Obs](OBS_STRUCTURE.md) | obs 及 analyze、eventlog、inputbudget、logidx、pattern、report | 观测数据如何读取、分析、形成证据和报告；何处有文件写入 |
| [Cmd](CMD_STRUCTURE.md) | cmd/papa、cmd/monitor-demo、脚手架模板 | 命令调度、显式写操作、框架文档分发和合成演示如何组织 |

`internal/app` 是 Internal 中的一章，不再维护独立 APP_STRUCTURE 分册。Admin、Engine、Internal、Pkg、Obs、Cmd 的目录彼此独立；同名 `runtimecontrol`、`identity`、`report` 应按完整路径区分职责。

## 2. 框架根目录

```text
papa/
├── papa.go / task.go / errors.go / resources.go # 公开门面
├── admin/                         # 后台协议、页面、资源与适配器
├── cmd/                           # CLI 和本地合成后台演示
├── config/                        # 配置文件解码、默认值和校验
├── core/                          # 只依赖标准库的跨包值与协议
├── docs/
│   ├── package_structure/         # 本说明书：总入口 + 七份职责分册
│   ├── README.md                  # 当前文档总导航
│   ├── architecture/             # 架构图、规则与框架开发
│   ├── assets/diagrams/           # 图源、SVG/PNG 与哈希清单
│   ├── configuration/            # 配置、显式建表与资源预算
│   ├── guides/                   # 抓取接入与数据落地
│   ├── accounts/                 # 账号与浏览器身份
│   ├── runtime/                  # 调度、恢复与治理
│   ├── operations/               # CLI、后台与观测
│   ├── embed.go                   # 可分发文档清单
│   └── history/                   # reviews/designs/plans/changes，均不分发
├── engine/                        # 执行编排和同包领域组件
│   └── internal/                  # 引擎专属调度、执行状态和抓取资源
├── internal/                      # 框架内部装配、服务及设施
├── models/                        # 框架持久模型与局部 hook
├── obs/                           # 观测分析与报告
├── pkg/                           # 通用抓取及资源能力
├── *_test.go                      # 根 API 与架构边界验证
└── go.mod / go.sum / Makefile / LICENSE.txt
```

`engine/internal/` 和 `admin/internal/` 按 Go 的 internal 规则分别限制在其父目录范围，不归根 `internal/` 分册。`static/`、`views/`、`templates/`、`testdata/` 是资源/模板/测试目录，不因存在子目录就构成 Go 子包。

## 3. Go 包覆盖索引

以下按源码目录归册；表内子项相对于该行根目录。目录里的测试仍与被测包相邻，分册中的测试章节区分组件单测和组合行为验证。

| 根目录 | 纳入的包 | 分册 |
| --- | --- | --- |
| 仓库根及基础包 | 根包 papa、config、core、models、docs | ROOT_STRUCTURE |
| admin | access、accountpools、accounts、auth、breakers、configstore、diagnostics、frameworkdata、gormsource、internal/httpjson、logs、monitoring、oplog、page、queues、reports、runtimecontrol、server、settings、shell、sitesource、sysinfo、taskflow、tasksource、tasktrace、tokenadmin | ADMIN_STRUCTURE |
| engine | 根包、internal/execution、internal/fetchruntime、internal/scheduler | ENGINE_STRUCTURE |
| internal | tracestore、 app、breaker、breakerstore、browserdiagnostics、database、gormfake、msgqueue、reportrunner、runtimecontrol、sitedecl、taskstore、track、workerpool | INTERNAL_STRUCTURE |
| pkg | account、account/contract、account/gormstore、account/onboarding、breakerpolicy、browser、browser/identity、browser/identity/runtime、discovery、egress、events、extract、governance、htmlfetch、incident、loggers、middleware、middleware/filedown、middleware/m3u8、middleware/proxy、notify、redact、temporary、testdb | PKG_STRUCTURE |
| obs | 根包、analyze、eventlog、inputbudget、logidx、pattern、report | OBS_STRUCTURE |
| cmd | papa、monitor-demo | CMD_STRUCTURE |

## 4. 职责、状态与依赖的读法

Papa 专注单实例大单体。业务站点、真实模型、固定采集目标和验收接线放独立业务模块；框架保留通用能力和合成 fixture。目录分册描述当前实现，不代表新增分册就需要创建新 Go 包。

| 层次 | 拥有的责任 | 与其它层的交接 |
| --- | --- | --- |
| 公开门面 | 当前类型导出、入口和业务扩展 | 委派实际组件，不另存领域状态 |
| App 组合根 | 创建依赖、注入、注册、启停与排空结果汇总 | 调用组件语义接口，未排空时保留共享依赖 |
| HTTP/页面适配 | 请求解析、权限、响应、展示、前端局部生命周期 | 使用窄服务接口；不复制领域版本和事务规则 |
| 执行与服务 | 调度、状态、原子操作、锁、取消和完成信号 | 在已有事务/锁序内协作，不通过任意字段回调绕开所有权 |
| 存储与资源适配 | SQL、浏览器进程、网络、文件等实际 I/O | 返回真实结果；不将发出取消或关闭连接等同于排空 |
| 值与契约 | 跨包共享的数据形状和语义 | 明确可变成员复制范围，不持有整个 App/Engine |
| 观测与工具 | 读取运行证据、生成产物、显式命令操作 | 事件不是执行真相，报告不是调度器；写操作由明确入口负责 |

这张表是职责说明，不是严格的单向 import 分层图。例如 sitedecl 仍使用公开 Engine 注册协议类型，config 使用能力包的配置值，admin/configstore 为运行服务提供持久化适配；实际允许依赖以各分册和架构规则为准。

## 5. 文档维护规则

1. 新增包或重要文件先确认职责、状态拥有者、输入输出、依赖、资源关闭者，再修改对应分册和本索引。
2. 同包职责文件可以平铺。拆包需要独立契约或生命周期，不能只按文件数、行数或名字建 manager/service/utils 容器。
3. 原子性、锁序、背压、取消、重试、排空和公开 API 是代码行为契约；目录树和 import 守卫不能证明它们没有回归。
4. 参数、配置字段与操作步骤写入对应使用手册；本目录说明实现归属。历史发现与验证写入 docs/history/reviews，设计与计划分别归 designs/plans，并标注适用版本/阶段、已替代的契约与当前手册入口，不重复维护当前目录；未记录精确发布版时不得凭日期推断版本号。
5. `DEVELOPMENT.md`、`ARCHITECTURE.md` 与其它手册仅链接职责分册，避免维护不同步的逐文件清单。移动说明书须同步链接、源码注释和脚手架分发验证。
6. 本说明书随六组主题手册嵌入 CLI，并保留目录复制到业务项目的框架文档位置；history 不分发。新建/升级规则及测试归属见 Cmd 分册。

各分册的“测试归属”指代码中的验证位置，不表示这些测试在每次文档维护时都重新执行。具体执行结果与环境限制应单独报告。
