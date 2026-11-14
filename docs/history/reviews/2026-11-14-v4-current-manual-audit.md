# 当前 v4 使用手册全面核对

<!-- version-scope -->
> **适用版本/阶段：2026-11-14 独立 Papa v4 当前工作区的文档核对。**
> 本轮只调整文档，不变更运行代码、配置模板或测试逻辑。历史验证不自动成为本轮通过证据。当前入口见 [手册导航](../../README.md)，历史材料边界见 [历史索引](README.md)。
<!-- /version-scope -->

## 范围与结论

覆盖 `docs/` 全部 23 份顶层使用手册及 `package_structure/` 8 份说明书，共 31 份；结合当前实现、配置解析、公开入口、CLI 模板和两张架构图核对。为原有 80 份历史报告、设计和计划补充适用版本/阶段说明，保留历史正文与当次测试结论；另新增本报告和历史索引。

发现的主要问题是跨阶段残留：SQL Trace 迁移、旧工程布局兼容、过期 API 示例、在线/离线报告混写，以及把历史验证或设计当作现行行为。已修正当前手册中的确认问题。未记录精确发布版的历史材料按演进阶段标注，不凭日期补造版本号。

## 核对矩阵

| 当前文档 | 对照实现与职责 | 结果 |
| --- | --- | --- |
| README、ARCHITECTURE、ARCHITECTURE_DIAGRAMS | 根门面、`internal/database`、`cmd/papa/layout.go`、App/Engine 执行和观测装配 | 明确独立 v4、文件 Trace、现行熔断、单实例以及历史材料边界；导航移除旧布局兼容和熔断待实施说明 |
| CORE_CONFIG、DATA_LANDING_GUIDE、TASK_TIMING | `config`、`models`、`internal/database`、任务时序与身份实现 | 区分当前 schema 迁移与旧库转换；AutoMigrate 可能调整受支持的类型/约束，不承诺只新增；任务身份和账号关联不依赖 Trace |
| FETCHER_WRITING_GUIDE、TASK_FLOW | Engine Task/Fetcher、提交、执行与 flow 入口 | 补 Site/轮次/计时字段、修业务键与 Meta 更新语义、区分立即重试与后续补偿；明确 TaskFlow 账号池/轮询/延迟限制 |
| ERROR_QUEUE、RECOVER_QUEUE、REPEAT_QUEUE、BREAKER | 队列配置、Engine 重投/补充、熔断控制与持久恢复 | 修 batch_size=0 的默认值、手动重投开关、pending 持续补充与 processing 启动恢复区别、未注册阶段处理；旧控制格式拒绝而非转换 |
| RESOURCE_BUDGETS | scheduler、inputbudget/reportrunner、下载资源/续传、tracestore | 分开在线槽位/维度预算与离线扫描预算；关停观测共用 2 秒，Trace 同步尝试收尾另计；只描述当前支持的续传格式 |
| ACCOUNT_RUNTIME、ACCOUNT_ONBOARDING、BROWSER_IDENTITY、RUNTIME_CONTROL | account/onboarding、browser/identity、运行策略装配 | 修受管 client 的凭据 context、真实状态字段、认证预算、自动账号证据及当前建表语义；补 onboarding 参数和共享期限排空，明确宿主持有账号池及标准任务身份 |
| CRAWL_TOOLKIT、TEMPORARY_RESOURCES | 网络录制、发现/提取、临时资源契约 | 修不存在的账号录制调用和公共浏览器池边界；补临时名称约束、Recover 首错返回和文件删除取消边界 |
| MONITOR | admin 功能路由、tasksource/taskadmin、reports、App 默认接线 | 去掉 SQL Trace execution_id 迁移，区分最新保留证据与当前状态；修自动熔断跨重启、暂停准入、补偿版本及报告文件只读边界 |
| OBSERVABILITY、CLI、DEVELOPMENT | obs/load/analyze、在线 aggregate、事件 emitter、CLI/check/upgrade/report 与模板 | 无事件时所有 CLI 生成模式均报错；在线固定 snapshot/unknown；seq 连续不证明无丢失；生成时间不等于写盘完成；补现行命令、校验副作用及测试位置 |
| package_structure 全部 8 份 | 文件索引、依赖方向、状态所有权与测试归属 | 修漏列 resources 门面、App 测试清单、旧 CLI 文件名及散落内容归属；Engine 清单核对一致，保留现有准确内容 |

## 关键契约统一

1. **Trace**：文件存储；`crawler.trace.mode` 仅 off/errors/all，默认 errors，默认七天保留。没有 SQL Trace 迁移/查询回退或手动选择线路。最新保留执行的尾部证据不能证明当前任务终态；尝试收尾同步发布，不属于 obs 异步旁路。
2. **数据库**：任务、账号、熔断等仍需显式创建当前表；旧任务结构和缺失当前身份的数据在迁移前拒绝。独立 v4 不是旧库原地升级器，也不会删除旧库。
3. **观测**：在线只收有限信号、分析和写报告；不持业务 DB 或分析回调。转发、在线报告和事件归档共用最多 2 秒收尾预算，可靠任务结算与熔断恢复另有所有者。CLI/业务显式离线分析才读取文件和可选数据库。
4. **报告可信度**：在线与离线可使用相同产物格式，但检查内容不同。事件序号/时间断层只提供部分缺失证据；生成时间不是四份文件的事务提交时间。并行在线/离线分析应使用独立输出目录，避免互相覆盖。
5. **任务循环**：持久 pending 补充独立于启动 recover；processing 孤儿恢复仍有启动筛选。立即重试、失败补偿和轮询不同；只重试幂等状态确认，不盲目重放 retry 加法或业务 handler。图中的 Item/Pipeline 是业务职责，不是框架自动产出协议。

## 历史材料处理

- SQL Trace MySQL 验收明确标为文件存储切换前阶段；旧测试名、旧接口及迁移步骤只保留证据价值。
- 主动追踪方案标为已取消；文件 Trace 实施保留其切换过程，同时明确旧兼容约定后来被独立 v4 清理替代。
- 统一监控中心路线图标为已被辅助报告定位替代；算法提案保留“待实施”，不伪装为现有能力。
- 已持续维护的诊断索引和最终画像标为 v4 阶段汇总；各批测试仍按原环境和代码解释，未将历史报告整体判为失效。
- `issues/`、`superpowers/` 不在 `docs/embed.go` 的分发范围。当前手册不依赖它们提供主契约，避免 `papa new/upgrade` 后产生断链。

## 验证与边界

- `GOWORK=off go test ./cmd/papa . -count=1` 通过，覆盖 CLI/脚手架、真实生成项目的 tidy/build、文档分发及根包架构守卫。
- 31 份当前文档的相对文件链接与嵌入分发集合检查通过；历史材料均具备版本/阶段标记。
- `docs/architecture/ARCHITECTURE_DIAGRAMS.md` 两个 Mermaid 代码块均由本地 Mermaid 解析，并通过本地 Chromium 渲染为 SVG/PNG；运行关系沿对应实现复核，语法通过不替代代码行为测试。
- `git diff --check` 用于检查工作区文档空白错误；未执行 Git 索引或历史写入。

本轮没有访问业务数据、创建 MySQL 测试库、运行真实站点采集或重跑全仓 race。文档与示例按源码核对，不声称每个业务片段均单独编译或所有硬件/插件组合已验证；既有全仓、MySQL、长稳和崩溃恢复证据继续保留各自记录的限制。所有原有工作区改动保留，未提交。

另发现源码注释/页面提示仍有历史用词：`engine/task.go`、`engine/taskadmin.go`、`admin/tasksource/table.go` 说“本站轮询周期”，实际 `SetTaskRepeatInterval` 使用 `e.cfg.RepeatQueue.Interval`，即全局周期；`internal/app/bootstrap.go` 的 AutoMigrate 注释和 `engine/attempt_execution.go` 的手动追踪注释也未同步。本轮核对当前手册时以实际执行代码为准，未把这些注释复制为现行契约；源码及页面提示不在本轮文档修改范围。
