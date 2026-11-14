# Papa 整体收尾审查与闭环清单

<!-- version-scope -->
> **适用版本/阶段：Papa v4 演进过程中的历史工作区快照（具体基线以正文为准；未记录精确发布版的条目不推定版本号）。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 当前使用方式以 [v4 手册导航](../../../README.md) 为准。
<!-- /version-scope -->


> 当前 v4 契约与最新验收见 [最终框架画像](v4-final-portrait.md)；本文保留对应历史审查阶段的证据，不表示旧 API/SQL Trace/旧库迁移仍受支持。

后续 Trace 存储方案已按用户决定改为文件；下文 D9/MySQL 结果保留历史口径，当前实现与边界见 [Trace 文件实施](trace-files.md)。

日期：2026-11-13。返回 [诊断与最终框架画像](README.md)。本结论针对当前未提交工作区；保留所有现有改动，不执行 Git 写操作。

## 收尾结论

D1–D9 均完成所属包内的实现、回归和说明文档闭环。复查了下载调度/发布、报告读写、治理取消、执行身份传递、Trace 存储与 API 装配，以及 App 排空后的共享资源关闭条件，未发现新的可确认代码缺陷。本次修正诊断首页中仍把已修项目列为短板、笼统声明 MySQL 未验证和文件数量过时的表述；原分报告与初次失败日志保留为历史证据。

“闭环”表示原始触发条件已被实现与回归覆盖，不表示每项功能获得所有平台、故障方式及业务环境下的无限保证，也不表示业务库已升级。

## 九项闭环矩阵

| 项目 | 责任归属与当前结果 | 关键验证 |
| --- | --- | --- |
| D1 | filedown/m3u8 固定 worker；filedown 增量生成偏移，取消停止新准入并等已准入回调 | TestChunkSchedulingBounded / TestSegmentSchedulingBounded、两包 CancellationDrainsProgressCallback |
| D2 | 下载器共享 transport，每实例保留独立超时；共享路径锁与代理策略不变 | 两包 TestProxyOverlayTimeoutIsolation |
| D3 | filedown 同目录临时文件完整合并并关闭后发布，失败保旧；保留已有普通文件权限位 | TestMergedArtifactPublication / TestMergedPublicationFailureCanResume；权限回归在 Linux 通过 |
| D4 | admin/reports 通过 os.Root 限制实际解析路径，同句柄检查/读取并限长 | TestReportFilesRejectOutsideLinks，覆盖目录/文件 symlink 和 Windows junction |
| D5 | cmd/papa 对显式历史窗口或 run 生成不执行本次保留清理 | TestReportGenerateExplicitHistoryRetained 分别覆盖 since/until/日期形旧 run |
| D6 | obs/report 归一特殊目录名；CLI 再次校验单段目录 | TestDirNameSpecialRunIDs / TestReportOutputDirRejectsTraversal、实际特殊 run 生成 |
| D7 | Engine 生命周期 context 贯穿批查询/条件重置；取消不放弃已确认重置后的投递，tickerWG 继续等在途工作 | BatchCancellation、QueueQueriesAndResetsCarryLifecycleContext、QueueCancellationAfterResetRetainsDelivery |
| D8 | Engine 成功认领分配 execution UUID，同轮重试共享；归档按执行及尝试分路径 | ArchiveExecutionIdentitySurvivesRetryReset / EvidenceIdentityAllocatedByClaimAcrossEngineRecreation |
| D9 | traceService 选择最新有记录执行的尾 500 步；旧行按 ID，不猜历史轮次；详细接口为增量 API | TraceStep 原九字段布局、旧 ListTrace/新详细回调、前端提示回归；MySQL 8.1.0 旧表两次迁移、551 条历史字段保留、实际索引及截断读取通过 |

公开 TraceStep/Options/MonitorConfig 形状保持兼容；持久化 TaskTrace 新列通过显式迁移生效。执行 UUID 只关联证据，不改变任务认领和终态 CAS。未改变组件锁所有权和既有锁序；取消与完成仍分开判断，未排空时不关闭共享 DB/浏览器。详细回调 setter 限装配期使用，不支持运行中并发重配。

## 最终验证证据

- 修后完整普通测试及 race：各 2802 个测试/子测试通过、84 个跳过，76 包通过、5 包无测试；完整 vet/build 通过。见 [复审](rereview.md)，不是本次重新运行的批次。
- Trace 增强实库用例：MySQL 8.1.0 普通一次、race 连续三次通过，四个随机测试库通过只读元数据查询确认清理。见 [实库验收](trace-mysql.md)。这不代表其它实库门控用例全部通过。
- 本次收尾定向 race：10 包通过，216 个测试/子测试通过，3 个跳过，无失败。覆盖下载、报告、治理取消、归档、Trace、API 装配与文档分发；日志 `.review-tmp/closure/targeted.jsonl`。
- 三个跳过：Windows 上的文件权限用例（此前 Linux 补验通过）；Trace 实库用例（已有上述独立批次）；BreakerReadOnlyMySQLRestoresReportButInvalidatesControlProof（本次未补验）。跳过不计入通过。
- 文档相对链接、409 个生产文件职责索引及 git diff --check 通过。本次未修改生产代码，不因文档更新再次重复全仓长测。

## 剩余风险与未验证项

| 范围 | 当前限制 | 后续验证/使用边界 |
| --- | --- | --- |
| 单实例容量 | R1–R7 后具备有限驻留、输入限额与共享下载准入；Task 内容、插件分配及外部写入仍不等于受控 RSS/卷配额 | 在目标硬件下测积压、背压、吞吐、P95、峰值 RSS 和恢复耗时 |
| 关停 | 多阶段预算不是统一硬时限；不合作的回调、报告生成、审计/事件 sink 仍可能延迟退出 | 慢 I/O 和不合作回调压测；run_stopped 仅描述 Engine，不能代替 App 所有排空结果 |
| 文件可靠性 | 合并发布未承诺断电持久性、ACL/属主保留；归档正文/元信息及报告四文件不是整组事务 | 尚未做断电、磁盘满、故障文件系统验收；报告输出根应可信，CLI 不承诺抵抗根内链接被管理员改写 |
| 观测 | Trace/归档写入尽力而为，事件可丢；最新指首次查询选中的最新持久记录，不是当前执行实时状态 | 旧空身份不能还原轮次；并发保留清理可能影响查询；诊断证据不能替代持久任务状态 |
| 数据库 | Trace 本次旧结构和重复迁移已实测；启动提示仅查列/索引名称，未自动迁移 | 其它历史 schema 漂移、其它 MySQL 版本、生产大表 DDL 时长/锁等待、迁移中断恢复未验证；业务升级仍需显式迁移 |
| 其它功能验收 | 本轮未补跑所有 MySQL 事务/丢应答/重启用例，未执行 ffmpeg 门控、真实 Provider/站点和生产部署 | 不把 Trace 实库通过外推为全部业务或基础设施验收；业务副作用仍需幂等 |
| 运维可用性 | HTTP ListenAndServe 失败仅记录日志，采集可继续 | 后台是否必须可用应由宿主部署约定；本次未改变启动策略 |

框架定位保持单实例模块化大单体；后续工作围绕进程内资源预算、持久化、崩溃恢复和纵向扩容。多实例协同、分布式租约和横向扩容不列入缺陷、路线图或验收门槛。

后续资源预算专项见 [R1–R7 审查与改进方案](resource-budget.md)，已把本页容量风险细化为所属包、触发路径和可验证的实施步骤；当前落地及验证见 [实施记录](resource-implementation.md)。
