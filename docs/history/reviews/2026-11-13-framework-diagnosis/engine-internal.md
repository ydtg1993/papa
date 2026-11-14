# Engine 与 internal 诊断证据（2026-11-13）

<!-- version-scope -->
> **适用版本/阶段：Papa v4 演进过程中的历史工作区快照（具体基线以正文为准；未记录精确发布版的条目不推定版本号）。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 当前使用方式以 [v4 手册导航](../../../README.md) 为准。
<!-- /version-scope -->


> 本文保留修复前诊断证据与当次验证。D1–D9 后续修复和最终验证见 [修复记录](remediation.md)。
范围：engine 全部功能分组；engine/internal；internal 下除 app（由主审负责）的基础组件。已读工作区 AGENTS.md、ENGINE_STRUCTURE.md、INTERNAL_STRUCTURE.md，并沿提交—认领—attempt—终态—治理重投—关停和熔断控制/存储路径核对源码。仅报告，没有生产代码或 Git 写操作。本报告为源码审查，不把测试文件存在等同于实际通过；全仓测试/race 结果由主线程汇总。

## 确认问题

### E1 [P2] 错误/轮询队列批处理在取消后仍继续扫描和写入所有后续批次

- 位置：`engine/batch.go:54-75`、`engine/batch.go:80-83`；调用点 `engine/errorqueue.go:48`、`engine/repeatpoll.go:151`；写入例 `engine/errorqueue.go:77`。
- 触发：错误队列或轮询队列正在处理多批积压时 Engine.Stop 取消 e.ctx；两处调用都硬编码 pause=0。
- 证据：processInBatches 在每批前没有取消检查；query().Find 未绑定 e.ctx；唯一的取消路径是 sleepBatchPause，但 pause<=0 直接返回 true，未检查 ctx；单条 requeue 同样使用 e.db 而非带生命周期 context 的句柄。因此取消后仍会查询后续批次、推进重投代次并尝试投递到已停止的池，最终进入内存 spill。
- 影响：大量失败/轮询积压把正常停止拖到全量扫描结束；超过 Stop deadline 时必须保留 DB/浏览器，增加退出耗时和额外写入。已有 Stop 等 ticker 的设计能避免谎报 drained=true，因此不是“无条件关库导致丢数据”，也不是多实例问题。
- 修复方向：批次前及批内明确检查生命周期取消；给查询和状态写入传递 context；pause=0 仍要检查取消。治理协调归 engine，数据库谓词仍由 taskstore 管。
- 验证缺口：`TestProcessInBatchesUsesKeysetCursor` 验证分页不死循环，未覆盖 pause=0 在第一批后取消不再读取第二批；需要该用例和真实 MySQL 阻塞查询取消测试。

### E2 [P2] 页面归档跨重投/轮询轮次覆盖旧证据

- 位置：`engine/archive_service.go:94-97`（文件名只用 Task.ID、Task.Retry 和 URL 哈希），`engine/archive_service.go:62`/`:81`（WriteFile 覆盖正文和元信息）；`internal/taskstore/store.go:71`（重投将 retry 重置为 0）。
- 触发：显式开启 crawler.archive 后，同一个任务 ID 在错误队列/人工重投中再次抓同一个 URL，且 Retry 与历史尝试相同；无重试的周期轮询同样一直使用 Retry=0。failure 模式需前后轮次均归档失败页面；always 模式每轮均可触发。
- 证据：生成路径未包含 repeat、reprocess 或独立 attempt ID，task ID 在重投中不变；同样的 ID/Retry/URL 必然得到同一路径；os.WriteFile 会截断覆盖现有文件。
- 影响：尚在保留期内的历史故障页面及 JSON 被新轮次静默替换，旧 Trace 中的同一路径会指向新页面，故障时间线可能被误解。不是“同一次尝试同 URL 取最后一页”的已明确设计语义。
- 修复方向：归档路径带准确执行轮次及尝试标识；元信息同步记录；同一次尝试内相同 URL 的合并可保留。
- 验证缺口：`TestArchiveKeepsEveryAttemptSeparate` 只测试 Retry 从 0 到 1；没有 Retry 重置、轮询或重启恢复导致相同 ID/Retry 的跨轮次用例。

### E3 [P2] Trace 缺执行轮次，跨轮次步骤混排且 500 条上限可能隐藏最新失败

- 位置：`engine/stage.go:164`（每轮 attempt 从 0 开始）；`models/task_trace.go:27-29`（只有 TaskID/Attempt/Seq）；`engine/trace_service.go:46-48`（按 attempt,seq 排序并 LIMIT 500）。
- 触发：显式开启 crawler.trace.enabled，同一持久任务通过错误重投、人工重投、周期轮询或恢复再次执行。
- 证据：多个轮次重复使用 attempt=0、seq=0…；Trace 模型未存 repeat/reprocess；查询没有轮次过滤或时间排序。先按 attempt 再按 seq 会把不同轮次的步骤交叉排列，同键之间无稳定顺序。若历史 attempt=0 的步骤已超过 500 条，新一轮 attempt=1 的失败步骤一定排在截断之后。
- 影响：无法可靠还原一次执行的步骤顺序；长期轮询任务的当前失败可能在后台不可见，页面归档引用也难以与正确轮次对应。
- 修复方向：Trace 持久化准确轮次/执行标识，默认查询最近一次执行并可分页看历史；对上限给出截断提示。
- 验证缺口：现有 trace_test 主要覆盖单次缓冲、flush、panic、warn、urgent；未见跨重投轮次和超过 500 条后最新失败可见性的回归。

## 功能与架构成熟度

| 子域 | 当前成熟度与实际边界 | 仍应核实的验收 |
|---|---|---|
| Engine 提交/状态持久化 | submissionStore 拥有身份查找与引用保护入库；taskstore 收口 claim/complete/retry/repeat 条件；execution.State 只跟踪未确认持久化，状态重试不重跑业务 handler；部分成功提交责任保留较完整 | 真实 MySQL 丢应答、断连、重启恢复；业务副作用仍需要幂等，不能宣称 exactly-once |
| 延迟/溢出/workerpool | scheduler 泛型包无 Engine 依赖；池注册/停止位与快照原子；workerpool 提交与 Stop 共锁，不关闭任务通道；spill 锁不跨 SQL | E1；大积压资源预算（见下）；持续加急流的常规队列等待分布 |
| 暂停/熔断 | coordinator 独立拥有状态；breaker→pause→overflow/e.mu 锁序明确；事件在锁外发；报告存储另有 cancellable gate；纯 detector 与 store 分离；待结算失败防止自动误恢复 | 启用真实 DSN 的 MySQL 事务/历史回滚/重启测试；进程强杀与真实浏览器失联演练 |
| attempt/抓取资源 | attemptExecution 不持有 Engine；资源注册归 fetchruntime.Resources；网络工作不持注册锁；账号反馈、归档/trace 收尾与终态层分工明确 | E2/E3；浏览器执行不响应取消时必须如实未排空 |
| TaskFlow | 同 flow 行锁串行化注册/策略/认领；任务数 10000 和深度 32 有明确预算；作为可选功能受 feature gate 保护 | 真库并发分支策略及业务同步提交子任务契约；不扩大成通用分布式工作流 |
| runtimecontrol | Service 拥有 revision/document/uncertain，CAS 写入丢应答先核对；DeletePool 与策略验证同一锁；历史是 pinned revision 的选字段恢复 | 真实库断连、提交成功响应丢失与池删除交叉验证；同步 mutex 等待本身不受请求 context 控制 |
| sitedecl | 注册表快照深复制声明 map/slice/overlay；计划校验在装配前完成；业务 Fetcher 保持自身身份 | 直接 Engine.AddStage 属较低层声明期 API，校验能力比 PlanSites 弱；不能声称运行期可并发注册 |
| browserdiagnostics | 诊断单飞、共享 Begin/Shutdown 准入与 pending 排空；manager 启动期注入；网络/浏览器操作受生命周期 context | 真 Chromium/账号浏览器的取消和清理；合成单测不能证明启动器或浏览器异常退出 |
| database | 普通 NewDB 不迁移；显式 Migrate 维护统一模型目录、身份回填，冲突停止不合并；业务模型由宿主提供 | 老 schema 到新唯一索引的真实 MySQL 演进、冲突恢复与备份回滚演练 |
| reportrunner | 生成互斥与 ticker 生命周期独立，Stop 等当前报告返回；Sections/Patterns 容器复制 | Stop 无 deadline 且不取消当前 Generate，加载文件及回调也未必遵守 timeout，文档已明确，列为设计限制而非新增缺陷 |
| track/msgqueue/gormfake | 观测队列可丢弃；track 不作为执行完成依据；gormfake 只作 SQL/错误注入设施 | 不拿观测计数或 fake 事务当真实持久化/隔离证明 |

## 已证实的限制/风险，不冒充新增正确性缺陷

1. 全局积压内存未形成端到端预算。`engine/scheduler_spill.go:42-54` 在每阶段 slice 中线性扫描去重并追加所有溢出任务；`engine/internal/scheduler/delay.go:48` 对延迟任务直接 heap.Push，无容量上限。workerpool 有界不能推出整个 Engine 内存有界；同阶段持续 N 条积压的 Put 可达累计 O(N²) 比较，暂停站点或批量恢复时值得做百万级任务压力测试。持久任务已有数据库行，因此后续可以设计有界驻留索引/按数据库回灌，但这属于单实例资源预算改进。
2. `engine/trace_service.go:87` 清理 DELETE 没有 WithContext(ctx)，ctx 只在批间检查；`engine/stage.go` releaseClaim 也故意采用非取消 e.db。依靠外层 Stop 超时报告未排空保住依赖，不等于所有 SQL 能及时取消。
3. 生命周期调用契约仍要求装配完成后才运行、运行期不并发 AddStage/ApplyRegisterStage。workerpool.Start 没有重复启动 once；这在 internal 文档明确，不按错误的运行期热注册承诺报告缺陷。
4. DB 写入失败会保持 worker 并重试到取消，这是有意背压，不应改成后台无限重放业务 handler。强杀发生在失败意图首次落库前无法恢复原始错误文本，不能靠内存设计消除。

本轮未额外创建测试代码、未连接业务数据库、未运行真实站点。确认问题是源代码可直接推出的行为；应由主线程测试记录补充本次环境实际验证结论。

## 本轮 race 长耗时补充观察

主线程全仓 race 执行到 `TestBreakerReviewTimelineTruncationMustNotInvalidateHealthyStages` 时耗时数分钟。审查该用例（breaker_review_fixes_test.go:174 起）为65阶段×3次成功，admitSmartTrial 每次执行1至2次 flush；snapshot 经 reportSnapshot().Clean、Snapshot.Clean，内存仓库 Save 再 Clean，同一批65个检查和最多128条含检查的时间线被重复脱敏。脱敏 Text 执行多轮正则，race 会放大成本。现场只读采样 engine.test PID35376 的累计 CPU 从244.78秒增加到266.20秒，工作集约633MB，说明仍在计算；没有获取 goroutine 栈或CPU profile，因此不能把具体性能占比说成已证实，也不报告成死锁。建议主线程等本轮结果，后续必要时针对快照重复清洗做 profile。

当前收尾状态见 [闭环清单](closure.md)；本文的修复前发现和当次未验证项作为历史证据保留，不能代替最新验证结论。
