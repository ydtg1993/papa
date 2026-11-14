# Papa 整体收尾审查（obs 解耦后）

<!-- version-scope -->
> **适用版本/阶段：独立 Papa v4 的 2026-11-14 工作区验证快照。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 当前使用方式以 [v4 手册导航](../../README.md) 为准。
<!-- /version-scope -->


日期：2026-11-14；对象为当前工作区，不以 Git HEAD 代替既有未提交修改。保留用户已暂存的图片删除，不执行索引或历史写操作，不使用业务数据。

## 模块核对

| 模块 | 核对结论与边界 |
| --- | --- |
| 根包 / core / config / models | 根包公开门面不另存执行状态；core 仍只依赖标准库，config 不导入 internal/breaker；模型与配置不接管结算事务。直接依赖清单留在 final-closure-deps.txt |
| Engine | 提交、调度、执行、持久化确认与熔断控制分别持有状态；事件出口只做有界投递。任务版本 CAS、未确认历史与恢复窗口仍为可靠控制事实，不依赖报告成功或观测完整 |
| internal/app 与运行服务 | App 组合和收集实际排空结果；HTTP、业务 hook、Engine、运行组件、消息监听分别确认，未排空保留共享 DB/浏览器。启动异常复用关停流程，不以取消冒充完成 |
| pkg 能力组件 | 浏览器池只清理自身 profile；下载组件保有预算/检查点职责；事件拥有复制和异步消费者；incident 负责共享证据与分类，不拥有后台报告生命周期 |
| Admin | HTTP/页面归功能包，server 负责组合；后台读取已有报告文件，不调用在线报告 Runner 生成报告。鉴权/资源路径约束继续由原功能组件负责 |
| Obs | 在线 Runner 仅接收值信号、静态配置；历史读取、DB 对账和业务 Section/Pattern 仅在显式离线调用。归档与报告共用有界关闭等待，故障不进入业务资源关闭门控 |
| Cmd / 模板 | CLI 显式选择报告来源与离线分析；脚手架和后台提示须与当前 events 开关、报告 unknown 语义一致，不再指向已移除的 App 业务报告注册 |

依赖守卫与实际 imports 共同核对：core 标准库、配置不反引 breaker 实现、admin 不反引 App、服务不反引 HTTP/Engine、Engine/可靠熔断存储不引 obs。守卫是防回退措施，不代替运行并发和 SQL 验收。

## 本轮修正

1. **事件限长与脱敏顺序**：有界 Detach 原先保留超长字符串前缀，可能把 URL userinfo/DSN 的凭据分隔符截掉，使消费者后续无法识别秘密。pkg/events 改为超限字符串整体省略并说明信息缺失；TruncateError 同样不再保留危险前缀。不在生产者端扫描或脱敏任意长度文本，不改任务 URL/身份，只处理观测副本。
2. **后台与 CLI 提示**：后台无报告提示明确 `events.enabled` 前提、正确的 `papa report generate` 命令；CLI 帮助区分 unknown 和离线业务扩展。修正手册对“无事件仍可直接生成默认日报”的过度描述；公开 ReportInputLimits 注释明确在线准入/维度预算，不冒充文件扫描限制。
3. **熔断启动恢复未纳入排空**：定向复现 LoadRuntime 阻塞时 Stop 错报 drained，以及 Stop 后初始化继续读 Store。恢复初始化须在任何存储调用前登记在途工作，并与 Stop 的取消/等待准入协调；数据库访问不放在该准入锁内。具体修复与验证结果见下方记录。

修前证据：事件 URL/error/attrs/stage/queue 五个子用例均报 `truncation removed credential delimiter before redaction`；生命周期红灯日志 `.review-tmp/closure-lifecycle-red.log` 报 `Stop reported drained while breaker startup storage was still active` 与 `initialization accessed storage after Stop allowed its owner to close it`。这些问题由实际回归发现，不把所有原有代码或文档问题归咎于目录迁移。

## 关停契约统一理解

“停止接收”“取消”“等待结束”“资源关闭”是不同阶段。Engine.Stop 的截止时间约束自身等待；App 根据各业务组件真实排空结果关闭共享资源；浏览器借出实例经归还路径回收；在线 obs 共用最多 2 秒等待，晚到单元只持纯值与自有文件。不同组件的 Close/Stop 不保证相同语义，不能把它们包装成未经证明的整个 App 硬退出期限。

本轮画像修正后台箭头为“读取已有报告文件”，避免图示暗示 Admin 调用在线 Runner。旧诊断和验证文件保留历史语境，当前使用手册及目录说明不得再提供旧入口示例。

## 验证与限制

本轮运行全仓普通/race、vet/build，并以定向回归验证实际修正。普通全仓跳过的 MySQL、真实浏览器或外部工具门控不算作通过。前轮独占 MySQL 的结算、熔断恢复、App 故障隔离/活跃排空/强杀恢复证据仍有独立适用范围，不能冒称本轮重新执行或生产验收。

| 本轮证据 | 结果及范围 |
| --- | --- |
| `.review-tmp/final-closure-all.jsonl` | 普通全仓 77 个有测试包、2,917 项测试/子测试通过，124 项跳过、零失败 |
| `.review-tmp/final-closure-race.jsonl` | 审查开始时启动的全仓 race：77 个有测试包、2,906 项测试/子测试通过，124 项跳过、零失败；Engine 312.309s。后续修正另由最终 events race、生命周期定向 race、全仓普通测试覆盖，不把两批源码/计数混为同一批最终全仓 race |
| `.review-tmp/closure-lifecycle-race.log` | 最终初始化恢复新回归、恢复观察者/探针 Stop 和 Stop 共享预算定向 race 通过，9.014s；夹具最后补强有界清理后再跑 |
| `.review-tmp/final-closure-mysql.log` | 本轮另行复验独占 MySQL 的任务计时、调度、熔断串行及结算恢复 race，40.411s 通过；没有复跑前轮完整 App 混合强杀套件 |
| `.review-tmp/closure-pkg-lifecycle.jsonl` | 浏览器/账号/下载的 Shutdown、Close、Cancel、Bound、Budget、Drain 专项 115 项通过、3 项环境门控跳过；一个实际浏览器池关闭用例通过，不扩大成所有 live browser 场景 |
| Admin / CLI | 全功能包测试与 vet 通过；提示修正后重跑 reports/server/cmd，含脚手架生成构建验证 |
| 事件边界 | 五类字段合成凭据回归先失败后通过，最终 events/incident race、events vet/build 通过；同时验证 TruncateError 到 JSONL 的缺失标记 |
| `.review-tmp/final-closure-vet.log`、`final-closure-build.log` | 全仓 vet/build 退出 0 |

专项结果不与全仓计数相加；短合成测试不等于生产 SLA。文档链接与 diff 空白检查另行完成。审查期间 Git 基线被外部更新，助手仅继续编辑当前文件，没有执行提交、回退或索引写操作。

框架仍为单实例模块化大单体；共享 CPU/RSS/磁盘不是硬配额沙箱，报告允许缺失、四文件非整组事务；业务非幂等副作用不保证 exactly-once。生产站点、24/72 小时长稳、断电、数据库服务崩溃及所有可选插件组合未验证。不把这些边界描述为多实例建设计划。


## 后续最终代码复审

并发恢复 gate、事件身份总预算及报告二次限长另有补正，最终无缓存全仓验证见 [复审记录](2026-11-14-final-code-rereview.md)。此前验证批次保持历史范围，不当作这些补正后的完整验收。
