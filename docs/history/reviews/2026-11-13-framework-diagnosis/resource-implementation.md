# R1–R7 分阶段落地与复核

<!-- version-scope -->
> **适用版本/阶段：Papa v4 演进过程中的历史工作区快照（具体基线以正文为准；未记录精确发布版的条目不推定版本号）。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 其中基于文件/数据库的报告预算属于当时方案；当前在线信号预算与显式离线扫描边界以 [观测手册](../../../operations/OBSERVABILITY.md) 为准。 当前使用方式以 [v4 手册导航](../../../README.md) 为准。
<!-- /version-scope -->


日期：2026-11-13。返回 [框架诊断](README.md) / [原始资源审查](resource-budget.md)。状态：**R1–R7 已完成实现、回归与文档闭环**。本轮范围是单实例框架内的输入、调度和下载预算，保留公开接口及既有工作区改动；无 Git 索引/历史写操作。

## 实施与责任归属

| 项目 | 落地方式 | 责任与兼容边界 |
| --- | --- | --- |
| R1 | residentSet 覆盖提交、pool、running、spill、delay、detached；全局/站点/阶段非阻塞准入；spill 按身份索引及轮转批取 | Engine 持有交接协议；持久 pending 满额后由持续补充发现，不因满额标失败；非持久/未交接 Meta 不无声卸载 |
| R2 | 延迟堆按身份去重，远期 durable pending 不常驻；近期待执行窗口与同一 resident 预算共享 | scheduler 只管容器，Engine 管轮次及持久交接；breaker 试探等瞬态责任不可直接释放 |
| R3 | 每批有限回灌、时间片、循环取消和 SQL context；注册阶段 keyset 轮转持续补充，独立于 RecoverQueue | taskstore/pending.go 管查询谓词；轮次 CAS 保留；Stop 按已注册阶段有截止时间地计数持久 pending，失败不能报已排空 |
| R4 | 行、解压扫描字节、事件、键、文件、保留内容、临时分桶、日期全部有有限默认值；原始源扫描一次，逐日消费 | obs/inputbudget 拥有计量，eventlog/logidx 拥有解析，obs 拥有本次临时文件；旧 LoadDailySources 仍提供有界收集接口 |
| R5 | Context 贯穿发现与读取；报告超时覆盖读取和分析；Stop 取消后仍等 Section 清理 | reportrunner 只管理互斥与生命周期；CLI/App 接线不接管解析；输入超限预检发生在消费/发布前 |
| R6 | 状态解码前限字节/条目，兼容旧 JSON/v1；私有 v2 位图移除重复路径；单写者合并、定时快照和最终 flush | middleware/checkpoint 管写者；下载包管格式与快照；resumeMu 内只复制，盘写在锁外，失败传播且不发布成功 |
| R7 | 播放列表与段数预算，媒体/init/key 独立上限；普通媒体 32KiB 流读并在读取阶段限速；加密数据受共享缓冲令牌约束 | 两种下载器共享 DownloadBudget；长度已知提前拒绝，未知长度探测超一字节，禁止截断伪成功；产物暂存后校验发布 |

新增装配 API 为 UseReportInputLimits、SetSchedulerLimits、SetDownloadResources、WithResources；原 Config/Overlay/ResumeState/SourceConfig 布局保持。预算非正值选择有限默认值，未新增 YAML 键。普通用量内保留行为；输入或并发超过新额度时会返回可识别资源错误，这是本轮明确增加的边界，不承诺继续接受任意大输入。默认数值、准入口径及调整方式见 [资源手册](../../../configuration/RESOURCE_BUDGETS.md)，职责索引见 [目录说明书](../../../package_structure/README.md)。

## 复核中补修的协议问题

- 旧投递轮次 CAS 被拒后，带非空且已过期 NotBefore 的副本可能反复延迟，并用 ID 成员索引挡住新轮次；现在明确核对版本、释放旧副本，由 DB 补充当前轮次。
- batch 重复提交同一 Task 指针可能把 delay 成员状态改成 submission，再在去重时释放原令牌；重复驻留指针不再重新抢占/覆盖归属。
- flow 等待前置条件和暂停任务不可长期占住最后一个预算槽，阻塞本可解除条件的其它任务；持久交接后释放，保留瞬态试探与未持久状态责任。暂停不允许业务回调执行。
- 启动前已接受的远期任务也可能卸载到 DB；Stop 的持久计数不再依赖 ApplyRegisterStage 是否完成，避免误报已排空。
- 远期首轮 repeatable 任务从数据库补充时仍走幂等首轮初始化，不因卸载把 repeat=0 直接带入执行；旧轮次不重复累计。
- 下载磁盘清点及报告文件发现使用目录分批读取，避免先一次性展开整个目录再检查数量；磁盘准入清点支持取消。
- M3U8 文件暂存发布保留已有普通文件权限位，新文件 0644；外部 FFmpeg 诊断输出限为 64KiB，最终大小校验后才发布。

## 回归与验证

各域回归覆盖精确上限/超一单位、gzip/无长度响应、旧状态、取消与最终清理、共享预算、旧产物保留、重复指针和轮次、暂停与单令牌递归提交。

关键用例：TestReadContextResourceBoundaries、TestConsumeDailySourcesEquivalentIndependentDays、TestConsumeDailySourcesPreflightPreservesOutput、TestStopWaitsForReportBeforeStorageClose、TestCheckpointCoalescesAndDrainsFinalState、TestCheckpointErrorSurvivesDrain、TestResumeLegacyCompactAndLimits、TestAssemblyLimitAndCancellationPreserveOldOutput、TestSchedulerMySQLRecursiveDurableRefillWithOneToken、TestSchedulerMySQLStaleRoundAndDelayedBatchPointer、TestSchedulerMySQLPausedSpillReleasesCapacityForOtherSite、TestSchedulerMySQLPauseOffloadsExistingPoolAndDelay、TestSchedulerMySQLStopCountsDurableBeforeApply、TestSchedulerMySQLDelayRefillInitializesFirstRound。

下载合成 10000 段检查点：旧表示 1,178,977 字节，新私有位图 1,793 字节。这只是格式样本，不代表所有任务压缩比例或总写入渐近复杂度。

隔离 MySQL 验证通过：主关联 race 组 24 项测试/子测试、0 跳过；既有池/延迟暂停转移另 3 项、启动前 Stop 另 1 项均通过；首轮初始化及关联轮询组另 14 项通过。这些批次有重复用例，不相加作为总覆盖数。覆盖单令牌递归提交、轮次更新/丢应答、补偿、任务时间、任务流及 TestTraceMySQLLegacyMigrationAndLatestTail 的旧表升级、重复迁移和最新执行尾部读取。

连接仅由测试配置提供，compensationMySQL 清空业务库名，再创建随机 papa_compensation_test_* 独立库；t.Cleanup 删除并把清理错误计为测试失败，本轮未出现清理错误。未迁移或查询配置里的业务库；这不同于另做一次服务器全库残留盘点。

测试修订保持真实约束：旧假库对插入 ID=1 返回固定 ID=7，不适用于批量按 ID 回查和持续补充，现按查询身份/游标/状态模拟；暂停测试仍要求恢复后恰好一次完成。Stop 测试等终态确认后才断言排空，不以 handler 已进入冒充完成。旧暂停门阻塞单测保留在无预算内部模式，新预算模式由实库 pool/delay 转移回归验证。

稳定版本验证（Windows amd64，GOWORK=off；race 启用 CGO）：

- `go test -json ./...`：2853 项测试/子测试通过，92 项跳过；77 个包通过、5 个包无测试，无失败。日志 `.review-tmp/resource-implementation/test-verified.jsonl`。
- `CGO_ENABLED=1 go test -race -json ./...`：同样 2853 项通过、92 项跳过，77 个包通过、5 个包无测试，无失败或 DATA RACE；Engine 包 288.751s。日志 `.review-tmp/resource-implementation/race-verified.jsonl`。
- `go vet ./...`、`go build ./...` 均通过；各域定向 race 及上述隔离 MySQL 回归通过。422 个生产文件名称职责索引、文档相对链接、gofmt 与 git diff --check 通过；TestScaffoldAndUpgradePreserveStructureManuals 通过。
- 跳过集中于未启用的真实浏览器、数据库、FFmpeg 环境，以及 Windows 不支持的 POSIX 权限场景。部分 MySQL 项已在独立显式批次执行；其它跳过不计作通过，也不把 Trace/调度验收外推为所有基础设施验收。

本轮初次全仓失败与修订记录保留在同目录的 test.jsonl、test-final.jsonl、race.jsonl；最终结论只使用修后批次。所有批次计数均含子测试，不跨批次累加。

## 剩余边界与未验证项

1. 引用/编码大小/受管缓冲预算不是 RSS 硬上限；Task Meta、插件分配、并发独立报告仍需宿主治理。报告事件与日志分别有扫描额度，最大原始扫描量为两类额度之和。逐日预检通过后的 I/O/回调失败可留下已经发布的前几天报告。
2. 临时磁盘清点保守重复计算活动文件/预留及嵌套根；拒绝后不自动删除可恢复状态。外部写入、成功产物保留及恶意路径并发替换不是此准入的硬隔离能力。
3. FFmpeg 的 -fs 可按包稍超阈值，最终检查拒绝过大产物；绝对物理卷上限依赖操作系统配额。已有 OutputSegments 仍逐文件 Rename，非整目录事务；ACL/属主及断电持久性不承诺。
4. 快照合并减少频率和重复路径，但每次仍 O(N)，不是增量日志。报告正常/错误/取消清理临时分桶，强杀可能遗留文件。
5. 未做生产 RSS 平台期、长稳吞吐、真实磁盘满、断电/强杀、真实业务站点及其它 MySQL 版本验收；合成 fixture 不能替代这些。动态代理 transport 缓存的容量/空闲回收仍是原审查列明的相邻议题，不属于 R1–R7 已实现范围。
6. 取消仍为合作契约，Stop 不用强制关 DB/浏览器伪装排空。持久 backlog 未清空与内存运行者安全退出是不同观察值，维持原保守停止结果。

框架画像：具备持久任务事实源、有限调度工作集和按组件输入准入的单实例模块化框架；下一步容量验收应记录硬件、配置、载荷大小、backlog、吞吐、P95、峰值 RSS、恢复与关停时间。
