# Engine 重构后第二轮审查与修复

<!-- version-scope -->
> **适用版本/阶段：Papa v4 演进过程中的历史工作区快照（具体基线以正文为准；未记录精确发布版的条目不推定版本号）。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 当前使用方式以 [v4 手册导航](../../README.md) 为准。
<!-- /version-scope -->


范围：`engine/` 及其三个 internal 包。以职责收口前保存的源码作行为对照，同时检查当前源码调用链、状态所有权、锁序、事务及关停。基线位于 `.review-tmp/engine-boundaries/baseline/`；本轮验证记录位于 `.review-tmp/engine-second-review/`。

## 确认问题

| 问题 | 归类与证据 | 修复边界 |
| --- | --- | --- |
| 探针结算提前采样 context.Err | 重构回归。`stage_probe_passed` 事件中取消后仍能进入 ramping；失败事件中取消也可能被误判为超时 manual | 取消必须在决策时读取；事件回调之后重新核验当前恢复轮次与取消状态 |
| INSERT 成功但回读失败时，日志先于 spill 接管 | 重构回归。错误日志 sink 阻塞或 panic 时，已提交任务尚未进入 spill；测试失败原文 `committed task was not retained before logger call: spill=0` | submissionStore 返回错误报告标记及投递计划；门面先接管，再记录原错误，不吞 panic |
| Stop 在 ticker 收尾之前取溢出/未确认快照 | 基线已有。ticker 最后的保留/确认未反映在返回统计中；回归分别报 `lost ticker's final retained work` 和 `reported persistence that ticker confirmed` | worker/ticker 继续共用原 deadline，等待后再读取队列与确认状态 |
| Stop 未统计延迟堆 | 基线已有。三个延迟任务仍在堆中，却返回 drained=true | Delay 提供独立容器快照，Engine 锁外按阶段累计 Queued；不丢弃延迟任务 |
| 登记配置仍由调用方间接持有 | 基线已有的职责漏洞。修改原 Overlay 指针/标量、延迟 map 会绕过组件接口改写已登记状态；复现值为 `(replaced,99)` 和 `1m0s` | 下载配置复制所有标量指针，治理安装复制 map；保留显式更新、nil/零值及阶段覆盖优先级 |
| 创建池事件在池登记锁内回调 | 基线已有。自定义 Emit 同步调用 Stop，卡在同一 Registry 锁；Stop 的 timeout 也无法解除锁等待 | 池创建与登记保持同一临界区，登记完成后由实际创建者发一次 created |
| 熔断事件在 breaker/pause 锁内回调 | 基线已有。自定义 Emit 同步调用 BreakerStatuses，重入 breakerMu 死锁 | 锁内准备事件，完成状态/暂停发布后锁外同步派发；探针结果通过两阶段重验防止回调期间控制变化被旧结果覆盖 |
| 队列轮次事件在统计锁内回调 | 基线已有。自定义 Emit 查询 GetQueueStats 时重入 queueRunState.mu；子进程复现 `fatal error: all goroutines are asleep - deadlock!` | 完成队列统计后释放锁，再同步派发独立事件值 |

这次没有为缩短目录列表增加空壳子包。Downloads 不再只把调用方指针藏进私有 map；submissionStore 的输出明确承载已提交任务的责任；Engine 继续拥有跨暂停、持久化与投递的执行顺序。

## 原子性和锁序

- 任务流服务继续拥有 flow 行锁、CAS、子任务 INSERT 与 lineage 更新的整个事务；取消中途登记时真实 MySQL 回滚，不留下半个任务流节点。
- 批量提交仍逐行 reload 后立即交接，部分成功项继续接管；重复项的 ID/repeat 回填在异常路径保留。任意用户回调 panic 仍向上传播，不声称后续批次项会在同一调用中继续投递；已入库项仍可启动恢复。
- 熔断与暂停继续采用 `breakerMu → pauseMu → overflow.mu/e.mu`；事件派发不充当锁或状态真相。恢复结果必须核验事件身份、轮次及控制变化。
- 熔断事务事件只在锁外同步派发；嵌套控制操作不会重新等待外层锁。探针里程碑派发后只重验原来要求的父 context 取消，不新增局部 probe deadline 的判定时点；异常退出清理自身结算 token。回调内 Stop 仍服从原超时，当前 probe 尚未退出时如实报告未排空。
- 池登记的停止位与活池快照仍在同一锁内发布；关闭之后不能懒建逃出快照的新池。created 事件只代表登记已经完成，不承诺不同并发调用之间的事件全序。
- ticker 超时后的 StopStats 是最新观察值，不代表后台已静止；排空失败不能据此关闭尚在使用的依赖。
- 站点声明、账号限频、trace/archive 单次缓冲及窄配置服务进一步对照后，没有发现需要改动的确定回归。

## 新增验证点

- `TestSubmitTaskRetainsCommittedTaskBeforeErrorLogging`：普通/异常日志 sink 前必须已保留任务。
- `TestSubmissionBatchBackfillsDuplicatesWhenHandoffPanics`：交接 panic 后重复项回填仍执行，原 panic 保留。
- `TestTaskFlowServiceMySQLRegistrationCancellationRollsBack`：INSERT 与 lineage UPDATE 之间取消，全事务回滚。
- `TestEngineStopSnapshotsAfterTickerFinalization`、`TestEngineStopReportsDelayedTasksByStage`：后台最终状态与延迟存留都进入统计。
- `TestDownloadRegistrationOwnsOverlayValues`、`TestInstallGovernanceOwnsDelaySnapshot`、`TestInstalledConfigurationDoesNotRaceCallerReuse`：输入复用不影响已登记配置，显式重新安装有效。
- `TestCloneDownloadsOwnsEveryOverlayField`：所有覆盖项复制，nil/显式零值语义不丢失，未来新增字段不能漏掉。
- `TestStagePoolCreatedEmitterCanStopEngine`、`TestStagePoolConcurrentLookupEmitsCreationOnce`：同步重入 Stop 无死锁，并发只发一次创建事件。
- `TestQueueRoundEmitterCanReadCompletedStats`：同步观察者能查询完整队列结果，不被统计锁阻塞。
- `TestBreakerProbeCancellationDuringResultEventCannotEnableRamping`、`TestBreakerProbeCancellationDuringFailureEventUsesShutdownTransition`：成功/失败里程碑期间取消不被提前采样掩盖。
- `TestBreakerEventsPublishPauseBeforeReentrantQueries`、`TestBreakerProbeObserverRetryCannotBeOverwrittenByOldResult`：回调读到完整暂停状态，人工重试不被旧结果覆盖。
- `TestBreakerObserverCanStopAfterCommittedTransition`、`TestBreakerProbeObserverStopHonorsTrackedDrainDeadline`、`TestBreakerRestoreObserverStopDoesNotStartMaintenanceAfterDrain`：同步回调关停遵守跟踪/超时，不在已停机后新增维护循环。
- `TestBreakerManualPauseObserverResumeKeepsLatestSiteProjection`：外层暂停不覆盖回调恢复后的新投影。
- `TestBreakerProbeLocalCancellationAfterMilestonePreservesOriginalDecision`、`TestBreakerProbeObserverPanicDoesNotStrandSettlementToken`：保留局部 context 的原判定时点，异常不遗留结算标记。
- `TestBreakerTransactionsNeverInvokeEmitterDirectly`、`TestBreakerQueuedEventsDetachMutableMaps`：防止重新引入锁内派发及共享事件 map。

## 验证记录

- 全量普通测试复跑通过：`GOWORK=off go test -p 2 ./... -count=1 -json`，75 个测试包、2681 个测试/子测试通过；日志 `full-test-final.jsonl`。其后追加的两条探针回归另跑普通测试通过，并纳入最终全仓 race。
- 全仓 `go vet ./...`、`go build -mod=readonly ./...` 通过；最终探针调整后补跑 `go vet ./engine/...` 通过，Grab 的 GOWORK=off 兼容构建通过。
- 配置 MySQL/Chrome/FFmpeg/live Rod 的最终全仓 race 通过：`CGO_ENABLED=1 GOWORK=off go test -race -p 2 ./... -count=1 -json`，75 个测试包、2756 个测试/子测试通过，零数据竞争报告；engine 完整 race 用时 297.970s，日志 `full-race.jsonl`。
- 独立 DSN 的 auth/frameworkdata 两条 MySQL 用例已另建隔离临时库跑 race 通过并清理，仅迁移自建测试库；日志 `optional-mysql.log`。
- 最终仅 `TestWriteKeepsFileMode` 因 Windows 不支持 Unix 文件权限语义未执行。普通测试未配置的外部依赖用例已在上述配置环境的 race 中执行，不以跳过代替验收。
- API AST 比较确认原 172 个导出声明/签名一致；原 481 个 engine 测试声明全部保留。69 个根层生产 Go 文件均列入目录说明，Go 格式与差异格式检查通过。

首轮全量普通测试仅 `TestStopWaitsForTickers` 失败。复现 50 次后确认是 fixture 的最后一次 ticker 在关停时保留了一条 spill，旧断言仍强求 drained=true；新统计没有错。假库不解析 WHERE，反复返回同一 pending 行，配合固定 sleep 放大了这一时序。测试改为通道控制在途 SQL，以失败 CAS 和不再返回候选模拟竞争者先完成更新，明确断言阻塞时不排空、释放后排空且只有一次 UPDATE。修正后关联 race 连跑 50 次通过，原失败及诊断保留在日志中。

熔断/暂停定向 race 通过（147.720s），最终新增短定向回归通过（9.450s）。一次为观察耗时而设为 60s 的诊断复跑因预算过短超时，堆栈处于历史脱敏/快照的 CPU 工作；没有据此放宽验收或删减测试，最终完整 engine race 与全仓 race 均通过。该诊断记录保留在 `.review-tmp/engine-boundaries/second-review/breaker-events-review.txt`。

没有执行 Git 写操作、部署或真实站点业务验收；原工作区修改保留。目录职责、输入所有权和关闭协议同步到 `ENGINE_STRUCTURE.md`、`DEVELOPMENT.md`、`ARCHITECTURE.md`。
