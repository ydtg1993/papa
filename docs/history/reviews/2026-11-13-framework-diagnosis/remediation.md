# 九项诊断问题修复记录

<!-- version-scope -->
> **适用版本/阶段：Papa v4 演进过程中的历史工作区快照（具体基线以正文为准；未记录精确发布版的条目不推定版本号）。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 当前使用方式以 [v4 手册导航](../../../README.md) 为准。
<!-- /version-scope -->


返回 [诊断与框架画像](README.md)。本文件描述诊断后的实现，原分报告保留修复前证据。二次复核与最新验证见 [复审记录](rereview.md)。

## 修复结果

| 项目 | 实现归属与结果 | 回归证据 |
| --- | --- | --- |
| D1 | filedown 增量生成偏移，filedown/m3u8 固定 worker + 无缓冲任务通道；取消停止准入并等待 worker，保留共享路径锁 | `TestChunkSchedulingBounded`、`TestSegmentSchedulingBounded`；修前并发设 2 却增加 2013 个 goroutine |
| D2 | 两下载器共享代理 transport，每次按当前实例配置创建 client；连接池共享、请求超时隔离 | 两包 `TestProxyOverlayTimeoutIsolation`；修前配置 1s、实际 7s |
| D3 | filedown 合并到同目录唯一临时文件，验证所有片段，显式 Close 后 Rename 发布；发布成功后才清理状态/分片 | `TestMergeFailureKeepsSegments` 及 bounded_download_test.go 中短片保旧、成功替换、临时文件清理；Windows 现有目标替换验证通过 |
| D4 | admin/reports 使用 os.Root 打开，检查及读取同一文件句柄，并对实际读取限长；列表摘要沿同一根边界 | `TestReportFilesRejectOutsideLinks`：目录 symlink、文件 symlink、Windows junction 均验证，不依赖字符串前缀判断 |
| D5 | cmd/papa 仅在无显式时间/run 筛选时继续默认保留清理；显式历史生成不在本次删除产物 | `TestReportGenerateExplicitHistoryRetained`；普通保留策略仍保留，后续自动清理仍可能删除旧日期目录，长期留存用独立 --out |
| D6 | obs/report 安全归一特殊 run_id，cmd/papa 写入前再次验证单段目标目录 | `TestDirNameSpecialRunIDs`、`TestReportOutputDirRejectsTraversal`、实际 `--run ..` 生成回归 |
| D7 | Engine 批查询/每条准入检查取消，查询和条件重置使用生命周期 context；零暂停也检查取消；已准入回调继续收尾并等待完成 | `TestBatchCancellationStopsZeroPauseAndPendingJobs`、`TestCancelledBatchDoesNotQueryOrReset`、`TestQueueQueriesAndResetsCarryLifecycleContext`、`TestBatchCancellationWaitsForAdmittedCallback` |
| D8 | 每次成功认领建立独立 execution UUID，同轮重试共享；归档文件名和元信息记录执行身份及 attempt | `TestArchiveExecutionIdentitySurvivesRetryReset`：3 个执行各 2 次尝试，6 份证据独立保留 |
| D9 | TaskTrace 增 execution_id；ListTrace 先选最新有记录执行，再取该执行尾 500 步并按尝试/步骤展示；详细接口 ListTraceExecution 首步返回 truncated；旧行按 ID 稳定展示 | `TestListTraceLatestExecutionTailKeepsFailure`、`TestListTraceLegacyRoundsRemainInInsertionOrder`、`TestListTraceClaimSelectedBeforeNewRoundFlush`、`TestTraceExecutionAndTruncationNotice` |

## 保留的契约

- 原 TraceStep 的九字段布局、ListTrace 签名保持不变；新增 TraceExecutionStep/ListTraceExecution 提供 execution_id/truncated。后台通过装配期 setter 接入详细接口，Options/MonitorConfig 布局不变，旧回调继续可用。后台显示身份、历史未知轮次与截断提示，身份先转义。
- 执行 UUID 只用于证据关联，不参与任务认领、轮次 CAS、终态条件或熔断结算；业务 handler 的重试/副作用契约不变。
- 治理队列锁顺序、条件重置谓词保持不变。取消只阻止后续工作，已经完成条件重置的任务仍走原投递/溢出路径；tickerWG 继续承担在途排空责任。
- 下载器输出路径锁/续传共享状态保持同一拥有者；文件发布失败保留旧产物和可恢复片段。未新增断电持久性保证，已完成续传索引仍随实际片数增长。
- 报告 HTTP 权限接线、四种文件白名单、状态与大小限制继续保留；正常 run_id 命名不变。
- 旧归档路径仍可下载。旧 Trace 无法可靠判断原轮次，不猜测、不回填；显示为历史未知轮次。
- ListTrace 的“最新”以第一次查询所选的最新持久记录为准；两次查询之间新执行完成，不把两个执行混排。观测查询仍非事务快照，保留期清理可并发删行。

## 显式迁移

开启 `crawler.trace.enabled` 的既有业务库升级前须运行 `papa migrate` 或自己的显式迁移入口：增加 `crawler_task_trace.execution_id varchar(36) NOT NULL DEFAULT ''` 和非唯一复合索引 `idx_trace_execution(task_id, execution_id)`、`idx_trace_latest(task_id, id)`、`idx_trace_tail(task_id, execution_id, attempt, seq, id)`，保留原 `idx_task_attempt_seq`。大表索引建议低峰期评估。

首轮没有自动连接或迁移业务数据库，当时未在真实 MySQL 上验证该 DDL；后续独立测试库验证已通过，见 [MySQL 验收](trace-mysql.md)。`TestTaskTraceExecutionSchema` 校验类型、默认值、索引顺序；`TestFakeTraceColumnsCoverModel` 守卫假库列完整性。模型继续在既有 FrameworkModels 清单内，任务主表未增列。

## 首轮测试与复核（本次复审结果见 rereview.md）

各子域定向 test/race 通过；全仓普通测试通过（2784 个测试/子测试通过、82 跳过，76 个包通过、5 个包无测试）；全仓 vet/build 通过。额外边界修正后 admin/reports 完整 test/race 再通过。全仓 race 批次完成：2784 个测试/子测试通过、82 跳过，唯一失败为下述旧浏览器计时断言；没有 DATA RACE 报告。修正计时后，浏览器整包 race 重跑通过（144 通过、10 跳过），因此各包最终 race 验证均通过。浏览器普通定向连续 3 次通过。原诊断的浏览器时间断言普通全仓通过，但 race 首轮再次触发：惰性浏览器启动/借实例被算入导航失败的 3 秒预算。现把计时起点移入 WithBrowser 回调，只测 CaptureRequest（包含其失败清理），保留 3 秒阈值和原错误/结果断言，没有跳过。最终浏览器重跑结果记录如下。

集成首轮发现 `TestStopWaitsForStartupRecover` 未等待查询进入，就与 Stop 并发。新取消准入允许该查询根本不启动，原断言会误报“提前返回”。测试现在先等待数据库回调进入，再验证 Stop 不能在回调释放前返回；连续 5 次通过。这个调整保留原有排空要求，并消除启动竞态假设。

## 画像更新与剩余边界

九项修复补齐下载调度背压、文件发布/访问边界、治理取消和证据执行身份。它们没有把 Papa 变成固定内存上界的系统：延迟/溢出积压、完成续传索引、m3u8 播放列表及观测全量读取仍需容量规划。硬关停时限、业务回调合作取消、真实 MySQL 迁移与崩溃恢复、ffmpeg、磁盘故障及长稳压力仍是独立验收项。

保留已有改动，未提交，未执行 Git 索引或历史写操作。


原始验证输出保留在 Papa `.review-tmp/remediation/`：`test-final.jsonl`、`race-final.jsonl`、`browser-race-final.jsonl`、`navigation-final.txt`、`vet-final.txt`、`build-final.txt`。全仓初次集成输出 `test.jsonl` 保留启动恢复测试的失败证据，最终结果不可用该旧文件替代。

本轮未配置真实 MySQL 测试连接、未执行 ffmpeg 门控验收、未做断电/磁盘满/大规模长稳测试；浏览器使用现有本机合成 fixture。跳过项是验证边界，不计作已通过。


最终补充检查：全仓 `go vet ./...`、`go build ./...` 通过；`git diff --check`、409 个生产文件职责索引、文档链接和脚手架文档分发回归通过。普通全仓及浏览器包重跑均没有新的失败。全仓 race 首轮的非零退出保留在原始日志中，未将其改写为单次全绿。
