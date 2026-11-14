# obs 隔离调整复审

<!-- version-scope -->
> **适用版本/阶段：独立 Papa v4 的 obs 辅助报告设计/实施阶段（2026-11-14 修订定位）。**
> 在线信号旁路与显式离线分析的当前契约见 [观测手册](../../operations/OBSERVABILITY.md)；正文测试与实施步骤保留其当次适用范围。 当前使用方式以 [v4 手册导航](../../README.md) 为准。
<!-- /version-scope -->


日期：2026-11-14。复审对象为当前未提交工作区，保留全部原有改动。依据 [实施记录](2026-11-14-obs-sidecar-implementation.md) 逐链检查，而非仅核对目录搬迁。

## 边界结论

- App 在线报告装配仅传静态配置/Inputs，Runner 没有业务 DB、Engine、Logger 或 Section/Pattern 回调字段；历史扫描与显式 DB 分析仍限于离线入口。
- Engine 与 breaker 事件均经过异步安全边界；观测队满丢弃，不等待报告、归档或任意消费者。在线 collector/writer 数量固定，写盘阻塞不会创建替代执行单元。
- 业务关停仍以 HTTP、Engine、业务 hook 和运行组件的实际排空判定为准；观测只共用 2 秒等待预算，不是业务资源关闭条件。
- 逐文件比对 pkg/incident 与迁移前 obs/incident：实现和测试内容相同，仅包契约注释更明确。可靠控制状态、未确认历史与数据库快照协议没有迁入可丢队列。probeEvidence 保留原有清洗/界限，只避免为取 Evidence 重建完整报告。
- 修正文档宣称而代码漏列的一项守卫：在线 Runner 也明确禁止直接导入 obs/pattern。

## 发现与修复

| 问题 | 修正归属与行为 |
| --- | --- |
| 缺省事件时间在消费时才补全，堵塞归档会把历史事件标成排空时间 | pkg/events 在安全入口采标准时钟；显式注入时钟仍在消费者执行，避免用户回调重新进入生产者路径 |
| 并发 Seq 分配与入队分离，可能按 2、1 顺序落盘 | pkg/events 用同一个短临界区完成序号分配与非等待入队；复制、回调、文件 I/O 不在该锁内 |
| 午夜快照先推进日期后，积压旧信号把日桶回退，随后新信号再清空当天累计 | Runner 日桶只前进；旧日迟到信号计入运行累计并单列 late，不污染当前日，也不假装补齐历史报告 |
| 同日乱序接收导致窗口起点晚于已经计入的信号 | Runner 窗口起点取纳入当前日信号的最早接收时间，最后接收时间单调前进 |
| 两个长维度共享前 256 字节时被静默混算 | Runner 不把截短键当身份；超长或已截断信号不进入精确分组，显式记 overflow，运行总计仍保留 |
| 不可写报告测试只证明阻挡文件存在，没有证明实际写入失败 | App 夹具断言 Runner.Status().Failures > 0；归档故障断言实际进入故障且 Dropped > 0 |
| App 测试早期断言失败存在资源清理空窗；defer 可能在清理前解除阻塞故障 | 成功 NewApp 后立即登记启动前清理；归档释放纳入有序 Cleanup，让 App 先经历真实观测退出预算 |
| panic 消费者测试可能把 Close 超时当作成功隔离，阻塞夹具断言失败时可能未释放消费者 | 明确拒绝 DeadlineExceeded；失败路径也释放阻塞并等待消费者真正结束 |

三条 Runner 回归先红后绿。修前原文分别为 `daily bucket lost or mixed signals`（当天仅剩 1 条，运行累计 4 条）、`window excludes counted signal`（12:00 起点漏掉已计入的 11:59 信号）、`uncertain dimensions were counted as exact`（257 字节站点混为一个 256 字节键且 overflow=0）。对应测试为 TestMidnightBacklogDoesNotRollDailyBucketBackward、TestSameDayBacklogExpandsReportWindow、TestLongAndTruncatedDimensionsNeverMerge。

事件测试也取得修前失败：TestEmitterConcurrentAdmissionKeepsSequenceOrder 报 `event 10 has seq 12`；TestEmitterPreservesAdmissionTimeAcrossBlockedArchive/outer_isolation 报时间超出 `admission interval`。仅临时撤回本次局部修复取得证据，并恢复原样；不使用 Git 回退，不覆盖其他工作区修改。修后事件与 Engine 入口相关定向 race 连续 10 轮通过。

新增 TestRetentionCursorClosedAtEOFAndWriterExit 同时核对游标置空与底层句柄不可再读取，不以指针丢失冒充 Close 完成。App 的 setup-failure 在 NewApp 后、Run 前故意失败，复用 shutdownRun(false) 关闭构造时已启动的 delayDispatcher；检查 DB/浏览器池、子进程、目录和独占库清理。普通 FailureCleanup 继续覆盖活跃 worker 的失败收尾。

## 验证记录

- `.review-tmp/obs-review-engine-mysql.log`：任务计时、调度、熔断串行和结算恢复真实 MySQL race 通过，21.394s。夹具先清空连接库名，再创建随机独占测试库，未使用业务库。
- App 专属实库两批 race 分别 114.211s、26.496s，通过活跃排空、持续饱和、关闭观测/归档阻塞/归档 panic/报告不可写、启动前和运行中故意失败清理，以及正常/取消/超时/强杀恢复。两批完整输出仅保留在本轮工具执行记录，未另存本地日志；不提供不存在的文件链接。
- 十秒合成补充负载最终 654 条 task/effect/attempt 一一对应；511 个独立样本驻留峰值 4、最长组进展空窗约 377ms。数字包括补充停止后的排空，不能当作十秒吞吐。不可写报告实际记录 9 次失败；归档阻塞关闭约 2.02s。每例检查子进程回收、自有目录删除及独占库不存在。
- MixedLifecycle：正常 288 条任务/尝试，cancel 96/98、timeout 96/99、crash 96/99；恢复场景允许尝试多于任务。timeout 的两个在途 handler 在 App partial 返回后仍成功写库，验证共享依赖没有被观测收尾误关。
- `.review-tmp/obs-review-race.log`：App、Runner、events、incident、obs 全子包通过；最终 vet/build 通过。Runner 原始边界修复另跑无缓存 race，事件并发修复另跑 10 轮定向 race。
- `.review-tmp/obs-review-all.jsonl`：最终全仓 77 个有测试包、2,906 项测试/子测试通过，124 项跳过、零失败；不把普通命令跳过的实库测试计入通过。
- `.review-tmp/obs-review-engine-race.log`：Engine 全量及三个内部包通过，主包 291.894s。该命令在审查初期启动，后续 events 修正由 10 轮 Engine 事件入口定向 race、最终相关包 race 及全仓普通测试覆盖，不冒称全量 Engine race 在最后编辑后重跑。
- 最终 diff 空白与本地文档链接检查通过，internal 文件数与新增两份 Runner 测试一致；engine/traces 不存在。未操作业务数据或 Git 索引/历史，原有改动保留。

## 证明范围

本轮修复涉及观测时间、统计准确性、并发投递和测试验收质量，不修改任务 CAS、SQL 结构、熔断控制锁序或恢复协议。内存锁仅覆盖有限本地操作，“非阻塞入口”指不等待消费者/队列容量/外部 I/O，不是操作系统调度的硬实时保证。

离线不合作回调和永久文件 I/O 不能被 Go 强杀；同进程 CPU/内存/磁盘仍共享。报告允许丢失、截断或延后，四文件发布不是整组事务；未做生产站点、24/72 小时长稳、断电或磁盘设备挂死实验。没有使用业务数据，未执行 Git 写操作。
