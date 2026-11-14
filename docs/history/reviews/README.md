# 历史审查、验证与设计的阅读边界

<!-- version-scope -->
> **适用版本/阶段：独立 Papa v4 的历史记录导航（2026-11-14 整理）。**
> 当前操作契约见 [使用手册](../../README.md) 和 [架构图](../../architecture/ARCHITECTURE_DIAGRAMS.md)。本目录及相邻 designs/plans 不随 CLI 分发；记录中的测试通过、缺陷和计划分别只对应其注明的工作区、环境和阶段。
<!-- /version-scope -->

| 材料 | 适用范围与当前入口 |
| --- | --- |
| [Repeat 全达上限成本](2026-11-14-repeat-cap-cost.md) | ICP 与残余条件成本实测；相同阶段/上限的站点条件合并，保留 NULL 及原子写入语义 |
| [核心引擎三图复审](2026-11-14-engine-guide-review.md) | 调度重验、状态重试与取消语义、图源行尾及 new/upgrade 完整替换 |
| [Repeat 条件合并复审](2026-11-14-repeat-cap-rereview.md) | 三值逻辑、同上限跨阶段、分组 SQL 取消/排空与性能夹具断言；错误注入及最终测试/race |
| [Repeat 查询复审](2026-11-14-repeat-query-rereview.md) | 非法日期及调用方排序/分页回归修复；单条过滤聚合、独立 MySQL 与最终测试/race |
| [Repeat 首次实施](2026-11-14-repeat-query.md) | 历史有序候选实现已撤销，旧性能数字不代表当前代码 |
| [剩余算法合成评估](2026-11-14-algorithm-evaluation.md) | CPU EWMA、自适应 α 暂不落地；repeat 聚合实库瓶颈与无缓存查询候选，含验证边界 |
| [退避优化复审](2026-11-14-retry-backoff-rereview.md) | 确定性取消/Stop、生产抖动分散、反证和最终全仓/race |
| [退避优化实施](2026-11-14-retry-backoff.md) | 安全封顶、Equal Jitter、计数/取消/Stop 回归；其余算法待验证 |
| [目录与图册分发复审](2026-11-14-docs-layout-rereview.md)、[最终全仓跳过清单](2026-11-14-docs-full-validation-skips.md) | 六图与分发保护复核；最终全仓/race 及额外发现的页面取消清理修复，124 项跳过单独列明 |
| [最终框架画像](2026-11-13-framework-diagnosis/v4-final-portrait.md)、[诊断索引](2026-11-13-framework-diagnosis/README.md) | 独立 v4 的阶段汇总；保留历史验证边界，不能把引用旧报告视为恢复旧能力 |
| [v4 清理记录](2026-11-13-v4-independent-cleanup.md) | 旧兼容移除及后续独立 MySQL、生命周期、负载验证；当前 v4 不转换 v3 及更早 API/配置/任务数据 |
| [SQL Trace 验收](2026-11-13-framework-diagnosis/trace-mysql.md) | **已被文件 Trace 替代**。旧表迁移、历史行读取和旧测试名只保留为当时证据；当前迁移不包含 Trace 表 |
| [文件 Trace 实施](2026-11-13-framework-diagnosis/trace-files.md) | 文件存储切换快照；当时保留的旧配置、旧接口等兼容约定后来已删除，当前见 [配置手册](../../configuration/CORE_CONFIG.md) |
| [手动 Trace 方案](2026-11-13-framework-diagnosis/trace-admin.md) | **已取消**；没有主动追踪按钮或待选状态，仅 off/errors/all 三模式，默认 errors |
| [九项修复](2026-11-13-framework-diagnosis/remediation.md)、[复审](2026-11-13-framework-diagnosis/rereview.md)、[初次闭环](2026-11-13-framework-diagnosis/closure.md) | 独立清理前的修复批次；D9 的 SQL Trace 和旧查询兼容不属于当前契约 |
| [资源预算实施](2026-11-13-framework-diagnosis/resource-implementation.md) | 调度、报告和下载预算的实施快照；在线报告后来改为仅信号聚合，当前见 [资源预算](../../configuration/RESOURCE_BUDGETS.md) 与 [观测手册](../../operations/OBSERVABILITY.md) |
| [统一监控中心路线图](../plans/2026-11-13-major-version-plan.md) | 在线 DB/回调/反馈耦合方案已被辅助报告定位替代；勾选项不是现有能力或后续施工授权 |
| [obs 修订设计](../designs/2026-11-13-obs-monitor-center-design.md)、[实施](2026-11-14-obs-sidecar-implementation.md)、[复审](2026-11-14-obs-sidecar-review.md) | 独立 v4 的信号旁路设计及当次验收；任务结算与熔断恢复不依赖 obs 报告 |
| [算法优化提案](../designs/2026-11-12-algorithm-optimization-design.md) | 退避已实施；其余三项已做合成评估，repeat 单语句查询已落实，暂不引入 EWMA/自适应 α/缓存 |
| [最终代码复审](2026-11-14-final-code-rereview.md)、[跳过项补验](2026-11-14-skipped-tests-verification.md) | 只证明所记录代码、合成场景与环境；文档核对不重算或扩大这些测试结论 |
| [本轮手册核对](2026-11-14-v4-current-manual-audit.md) | 当前手册过时说明、遗漏与矛盾的修订，以及文档分发验证 |
| [变更记录](../changes.md) 及其它历史审查/计划 | 各条/各文按自身基线阅读；未记录精确发布版时不凭日期推断版本号，不将整份记录当作当前能力清单 |

保留历史正文是为了追溯当时的决策、失败和验证，不应把里面的升级 SQL、已删除接口或旧施工命令复制到当前 v4。需要接入或操作时，从顶层使用手册进入；需要核对实现归属时，从 [目录说明书](../../package_structure/README.md) 进入。

## 全部审查与验证记录

返回 [历史导航](../README.md)。下表是文件索引，具体适用版本和验证范围以各文首部为准。

| 记录 | 主题 |
| --- | --- |
| [2026-11-14-v4-current-manual-audit.md](2026-11-14-v4-current-manual-audit.md) | 当前 v4 使用手册全面核对 |
| [2026-11-14-skipped-tests-verification.md](2026-11-14-skipped-tests-verification.md) | 全仓 124 项跳过测试梳理与本地补验 |
| [2026-11-14-skipped-tests-inventory.md](2026-11-14-skipped-tests-inventory.md) | 124 项跳过测试逐项清单 |
| [2026-11-14-obs-sidecar-review.md](2026-11-14-obs-sidecar-review.md) | obs 隔离调整复审 |
| [2026-11-14-obs-sidecar-implementation.md](2026-11-14-obs-sidecar-implementation.md) | obs 辅助报告隔离实施与验证 |
| [2026-11-14-framework-final-review.md](2026-11-14-framework-final-review.md) | Papa 整体收尾审查（obs 解耦后） |
| [2026-11-14-final-code-rereview.md](2026-11-14-final-code-rereview.md) | 收尾修复复审与最终代码验收 |
| [2026-11-13-v4-independent-cleanup.md](2026-11-13-v4-independent-cleanup.md) | Papa v4 独立版本清理记录 |
| [2026-11-13-settlement-chain-audit.md](2026-11-13-settlement-chain-audit.md) | 失败结算链路修复与完整复查 |
| [2026-11-13-package-boundaries-second-review.md](2026-11-13-package-boundaries-second-review.md) | 包职责重构二次审查 |
| [2026-11-13-framework-diagnosis/validation.md](2026-11-13-framework-diagnosis/validation.md) | 验证明细（2026-11-13） |
| [2026-11-13-framework-diagnosis/v4-final-portrait.md](2026-11-13-framework-diagnosis/v4-final-portrait.md) | Papa v4 最终框架画像与收尾结论 |
| [2026-11-13-framework-diagnosis/trace-mysql.md](2026-11-13-framework-diagnosis/trace-mysql.md) | Trace 真实 MySQL 迁移与旧数据验收 |
| [2026-11-13-framework-diagnosis/trace-files.md](2026-11-13-framework-diagnosis/trace-files.md) | Trace 文件存储实施与验证 |
| [2026-11-13-framework-diagnosis/trace-admin.md](2026-11-13-framework-diagnosis/trace-admin.md) | Trace 后台主动追踪 |
| [2026-11-13-framework-diagnosis/resource-implementation.md](2026-11-13-framework-diagnosis/resource-implementation.md) | R1–R7 分阶段落地与复核 |
| [2026-11-13-framework-diagnosis/resource-budget.md](2026-11-13-framework-diagnosis/resource-budget.md) | 单实例资源预算与背压专项审查 |
| [2026-11-13-framework-diagnosis/rereview.md](2026-11-13-framework-diagnosis/rereview.md) | 九项修复二次复审 |
| [2026-11-13-framework-diagnosis/remediation.md](2026-11-13-framework-diagnosis/remediation.md) | 九项诊断问题修复记录 |
| [2026-11-13-framework-diagnosis/README.md](2026-11-13-framework-diagnosis/README.md) | Papa 全面诊断、功能检查与框架画像 |
| [2026-11-13-framework-diagnosis/engine-internal.md](2026-11-13-framework-diagnosis/engine-internal.md) | Engine 与 internal 诊断证据（2026-11-13） |
| [2026-11-13-framework-diagnosis/closure.md](2026-11-13-framework-diagnosis/closure.md) | Papa 整体收尾审查与闭环清单 |
| [2026-11-13-framework-diagnosis/capabilities.md](2026-11-13-framework-diagnosis/capabilities.md) | Papa 能力层、配置与基础契约诊断（2026-11-13） |
| [2026-11-13-framework-diagnosis/admin-obs-cli.md](2026-11-13-framework-diagnosis/admin-obs-cli.md) | Admin / Obs / Cmd 诊断（2026-11-13） |
| [2026-11-13-engine-second-review.md](2026-11-13-engine-second-review.md) | Engine 重构后第二轮审查与修复 |
| [2026-11-13-engine-boundaries.md](2026-11-13-engine-boundaries.md) | Engine 同包组件职责收口 |
| [2026-11-13-app-structure-review.md](2026-11-13-app-structure-review.md) | internal/app 目录与职责复查（2026-11-13） |
| [2026-11-13-app-ownership-implementation.md](2026-11-13-app-ownership-implementation.md) | App 状态所有权与关停收口记录（2026-11-13） |
| [2026-11-13-admin-server-cleanup.md](2026-11-13-admin-server-cleanup.md) | admin/server 收尾整理 |
| [2026-11-13-admin-feature-boundaries.md](2026-11-13-admin-feature-boundaries.md) | 后台前后端功能职责拆分 |
| [2026-11-12-task-timing-identity-verification.md](2026-11-12-task-timing-identity-verification.md) | 任务时间与标准身份施工验证 |
| [2026-11-12-breaker-site-recovery-review-fixes.md](2026-11-12-breaker-site-recovery-review-fixes.md) | 整站恢复复查修复与验收 |
| [2026-11-12-breaker-settlement-review.md](2026-11-12-breaker-settlement-review.md) | 未结算失败修复与复查 |
| [2026-11-12-breaker-serial-resolved-issues.md](2026-11-12-breaker-serial-resolved-issues.md) | 串行验证、10分钟超时及复查修复 |
| [2026-11-12-breaker-implementation.md](2026-11-12-breaker-implementation.md) | 分类熔断施工与验收 |
| [2026-11-12-breaker-hourly-daily-history.md](2026-11-12-breaker-hourly-daily-history.md) | 三次失败样本验证、小时复查与每日历史验收 |
| [2026-11-12-breaker-analysis-design.md](2026-11-12-breaker-analysis-design.md) | Papa v4 分类熔断与故障分析方案 |
| [2026-11-10-docs-current-code-audit.md](2026-11-10-docs-current-code-audit.md) | 当前代码文档核对记录 |
| [2026-11-10-browser-identity-verification.md](2026-11-10-browser-identity-verification.md) | 浏览器身份首期实施与验证记录 |
| [2026-11-10-adaptive-runtime-verification.md](2026-11-10-adaptive-runtime-verification.md) | 站点账号池与运行治理验收记录 |
| [2026-11-10-account-lifecycle-improvement-report.md](2026-11-10-account-lifecycle-improvement-report.md) | Papa 账号接入、生命周期与任务补偿待办报告 |
| [2026-11-10-account-lifecycle-implementation.md](2026-11-10-account-lifecycle-implementation.md) | 账号生命周期实施与验证记录 |
