# 九项修复二次复审

<!-- version-scope -->
> **适用版本/阶段：Papa v4 演进过程中的历史工作区快照（具体基线以正文为准；未记录精确发布版的条目不推定版本号）。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 当前使用方式以 [v4 手册导航](../../../README.md) 为准。
<!-- /version-scope -->


返回 [诊断与框架画像](README.md) / [首轮修复记录](remediation.md)。日期：2026-11-13。

## 结论与本次修正

逐项检查 D1–D9 的实现、公开入口、状态所有权和回归。发现并修复下载文件权限、Trace 公开值布局两处回归，同时补齐 Trace 旧表就绪提示和有序读取索引。其他项目未发现新的生产代码缺陷，补充测试覆盖此前不足的取消窗口与历史报告筛选。此结论受下述环境验证范围限制。

| 项目 | 复核结果与证据 |
| --- | --- |
| D1 | 固定 worker 准入、取消后等待已进入的进度回调；新增 TestChunkCancellationDrainsProgressCallback / TestSegmentCancellationDrainsProgressCallback，未改变路径锁与续传状态所有权 |
| D2 | transport 共享但 client 超时按实例独立；原超时隔离回归保留 |
| D3 | 临时文件替换曾将已有私有文件 0600 改为 0644；现在保留已有普通文件权限位，并补充发布失败后恢复测试 TestMergedPublicationFailureCanResume。Linux 修前实际报错：replacement changed private output permissions: got 0644 want 0600；修后通过 |
| D4 | 复核 os.Root、同句柄检查/读取、白名单与限长；保留链接逃逸回归。HTTP 取消不等于中断本地文件读取，列表返回上限不等于枚举上限 |
| D5 | TestReportGenerateExplicitHistoryRetained 分别验证 --since、--until、日期形旧 --run；避免新目录 mtime 掩盖历史清理问题 |
| D6 | 特殊 run_id 归一与写入前单段目录校验保留；配置输出根被视为可信，CLI 写入不承诺抵抗管理员修改根内链接 |
| D7 | 新增 TestQueueCancellationAfterResetRetainsDelivery，覆盖 error/repeat/recover 重置已成功后取消并关闭池，确认任务仍为 pending 且进入溢出队列；不回滚已成功重置，不丢已准入任务 |
| D8 | TestEvidenceIdentityAllocatedByClaimAcrossEngineRecreation 从实际认领和重试路径验证 UUID；跨 Engine 重建独立、同轮重试共享，身份不参与任务 CAS 或结算 |
| D9 | 恢复原 TraceStep 九字段布局与 ListTrace 签名；新增 TraceExecutionStep/ListTraceExecution，后台在提供请求服务前通过 setter 装配。新旧回调、扁平 JSON、旧行读取、最新执行尾 500 步均有回归 |

## 兼容与并发契约

core/engine/根包原 TraceStep 保持位置式初始化兼容，TestTraceStepPreservesOriginalValueLayout 使用真实位置式字面量验证。Options/MonitorConfig 保持原字段布局；新增详细接口通过 SetExecutionLookup / SetTaskTraceExecution 接线。setter 仅用于启动装配，不是可并发热更新的配置入口。

治理队列依旧由既有生命周期 context、条件更新和 tickerWG 管理。取消停止新的查询/准入，已完成重置继续走原投递/溢出路径；锁序和原子更新谓词未改。下载取消等待已准入 worker/回调，不承诺强制终止不合作的回调。文件合并先完整写入并关闭同目录临时文件，再发布；失败保留旧目标和可恢复片段。保留权限位不等于保留 inode、属主或 ACL，也不新增断电持久性保证。

Trace 服务拥有选轮次和读取，后台只适配 JSON，App 只接线。读取按第一次查询选中的执行隔离，并非跨两次查询的事务快照；保留期清理仍可能并发删行。旧空 execution_id 行按 ID 展示，不推测历史轮次。旧归档路径保持可读。

## Trace 显式迁移

启用 Trace 的既有库必须通过 papa migrate 或业务显式迁移入口增加 execution_id varchar(36) NOT NULL DEFAULT ''，以及 idx_trace_execution(task_id, execution_id)、idx_trace_latest(task_id, id)、idx_trace_tail(task_id, execution_id, attempt, seq, id)，保留 idx_task_attempt_seq。

internal/database.CheckTraceSchema 只读检查列名和索引名；App 在 Trace 开启且表存在时调用，缺项输出迁移提示。普通启动不自动迁移，也不新增启动硬失败。该探测不是完整类型/索引定义验证，旧表未迁移时不承诺新 Trace 读写可用。

新增 TestTraceMySQLLegacyMigrationAndLatestTail：隔离测试库模拟缺列旧表、插入旧行、执行真实迁移两次，再检查旧行与 551 条新执行的尾部查询。前次全仓批次未配置隔离连接，该用例当时跳过。后续已在 MySQL 8.1.0 独立随机库完成增强用例普通一次及 race 三次验证，旧表升级、重复迁移、历史字段保留和新旧执行读取均通过，详见 [真实 MySQL 验收](trace-mysql.md)。

## 验证

Windows、GOWORK=off，race 启用 CGO_ENABLED=1。本批 go test ./... -count=1 和 go test -race ./... -count=1 均退出 0；各有 2802 个测试/子测试通过、84 个跳过，76 个包通过、5 个包无测试，无失败或 DATA RACE 报告。go vet ./...、go build ./... 均退出 0。409 个生产文件职责索引、说明书/诊断报告相对文件链接、脚手架 TestScaffoldAndUpgradePreserveStructureManuals 与 git diff --check 均通过。原始输出保留在 .review-tmp/remediation-review/；定向 Trace 日志另见 .review-tmp/trace-rereview-tests.txt 和 trace-rereview-race.txt。Linux 权限回归使用交叉编译测试二进制在 WSL Ubuntu 执行 TestMerged*；Windows 定向下载、治理、Trace、后台测试及 race 已通过。

未执行真实业务数据库迁移、ffmpeg 门控验收、磁盘满/断电或大规模长稳测试。保留现有工作区改动，不执行 Git 索引或历史写操作。
