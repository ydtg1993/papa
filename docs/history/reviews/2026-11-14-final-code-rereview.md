# 收尾修复复审与最终代码验收

<!-- version-scope -->
> **适用版本/阶段：独立 Papa v4 的 2026-11-14 工作区验证快照。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 当前使用方式以 [v4 手册导航](../../README.md) 为准。
<!-- /version-scope -->


日期：2026-11-14。延续 [整体收尾审查](2026-11-14-framework-final-review.md)，此次要求是修正全部稳定后，在同一份最终代码上完成普通全仓与全仓 race，不复用修前的整体结果。

后续对本报告 124 项 skip 的逐项分类和本地环境补验已完成，见 [补验报告](2026-11-14-skipped-tests-verification.md)；其中还发现并修复 FFmpeg 输出选项顺序。本文保留当批最终代码的原始测试结果，不把后续修复追记为已经过本文的全仓 race。

## 复审发现与处理

| 问题 | 证据与修正 |
| --- | --- |
| 多个 InitBreakerRuntime 可重复加载恢复，并可能越过第一次尚未完成的可靠 flush | 新 TestBreakerConcurrentStartupWaitIsCancelable 修前报 `concurrent initializer entered storage instead of waiting with cancellation`。coordinator 新增可取消恢复 gate，覆盖加载、持久化及循环交接；ready 在可靠 flush 和维护登记后发布 |
| NewEmitter 消费阶段覆盖 runID 绕过总字符串预算 | 修前 TestEmitterIncludesConfiguredRunIDInStringBudget 报 `bytes=18432 truncated=false`；归档身份改在 Detach 前设置，合法身份值及预算共同受控 |
| 在线报告再次把错误截成 256 字节，破坏凭据结构 | 合成 URL 在事件预算内、报告样本预算外；真实 report.json 修前报 `failure sample shortening leaked credential into report.json`。Runner 样本及静态文本整值省略，质量计数包含样本省略；同一信号不因传输与样本两处省略而重复计数 |

变更按职责归属 Engine 初始化、pkg/events 输入边界、internal/reportrunner 报告样本。没有把脱敏扫描放回爬虫生产者，不改变任务 URL、身份或结算 SQL。保留原有所有未提交修改，不执行 Git 写操作。

## 并发与取消核对

Engine.mu 只协调取消和初始化在途登记，既不跨存储也不等恢复 gate；gate 首次创建由专用 sync.Once 负责，不直接释放事务的 breakerMu。等待恢复 gate 的调用也持初始化 tickerWG 槽，Stop 取消可结束等待；不合作 Store 未返回仍报告 partial。初始化槽保持至维护循环已登记，避免 WaitGroup 归零后再次启动后台工作。失败/取消释放 gate，不把 ready 留为真；成功重试仍可执行。

回归覆盖 Load 阻塞、停止后拒绝存储、恢复事件异步触发 Stop 时的 Save 阻塞、合作式 Store 取消、并发等待超时、flush 未完成不可提前返回成功及失败后重试。阻塞夹具有释放清理，锁回归通过子进程超时隔离。相关定向 race 已通过，不以仅观测到取消代替实际排空。

事件测试覆盖单字段/总预算、多字节精确边界、重复 Detach 的标记保留、配置 runID 后的实际最终 JSONL，以及指标键和数字类型。Runner 回归读出四份真实产物，同时检查无合成秘密、短错误仍存在、截断计数准确。只用合成凭据与自有临时目录，不触碰业务记录。

## 最终验证方法

代码和内嵌手册同步完毕后冻结 1,039 个可见文件（排除工具输出和不分发的历史报告目录），清单为 `.review-tmp/final-recheck-source.json`；结束后用同一脚本重新计算并比较。普通与 race 都显式 `-count=1`、`GOWORK=off`，race 开启 CGO，命令为：

```text
go test -p 2 -count=1 -json ./... -timeout 12m
go test -race -p 2 -count=1 -json ./... -timeout 12m
go vet ./...
go build ./...
```

红灯证据为 `final-recheck-lifetime-red.txt`、`final-recheck-events-red.log`、`final-recheck-report-red.log`；修后定向证据为同前缀 lifetime-race、events-race、report-green 文件。全仓和最终 MySQL 结果见下方结论；跳过的环境门控不会算作验收通过。

### 首次完整运行暴露的遗漏

首次 `final-recheck-all.jsonl` / `final-recheck-race.jsonl` 失败，明确不算验收通过：事务事件守卫指出 `breaker_restore_state.go bypasses transaction event dispatch`；普通测试还遇到 `TestRuntimeAccountFailureEndsTaskWithoutWaitingForReplacement: failed task terminal event missing`。

前者通过专用 sync.Once 初始化 gate 修正，没有放宽守卫；后者保持任务执行耗时断言不变，在检查终态事件前显式排空异步边界，避免把调度延迟误当成业务失败。守卫及生命周期定向无缓存 race 10.728s 通过，账号回归连续 20 轮 race 通过。

这两处修正后曾重新冻结并使用 `final-recheck-all-final.jsonl` / `final-recheck-race-final.jsonl` 重跑；第二轮普通测试仍发现 `TestSiteChallengeTaskExecutesOnceWithoutAccountSwitch: failed terminal event missing`，因此同样不作为最终验收。随后统一核对所有 collector 计数/快照，包括“不发事件”的反向断言：完成执行后排空，重启恢复前后各排空，需继续收事件的熔断报告先等待真实事件而不提前关闭消费者。测试没有改成同步投递，也没有新增 sleep。相关非实库目标 race 连续 20 轮通过（53.599s）；专属 MySQL settlement 另行补验。

最终冻结后再次从头完整运行的唯一验收文件为 `final-recheck-all-verified.jsonl`、`final-recheck-race-verified.jsonl`、`final-recheck-vet-verified.log`、`final-recheck-build-verified.log`。前两轮记录保留，不用局部重跑覆盖旧失败，也不借用它们的绿灯作为最终全仓结果。

## 最终验收结果

最终冻结版本的所有命令均退出 0；普通测试和 race 的计数分别统计，不累加，含子测试。

| 验证 | 实际结果与证据 |
| --- | --- |
| 普通全仓，无缓存 | 77 个有测试包，2,925 项通过、124 项环境门控跳过、0 失败；01:43:15–01:47:04，`final-recheck-all-verified.jsonl` |
| 全仓 race，无缓存 | 同为 77 个有测试包，2,925 项通过、124 项环境门控跳过、0 失败；01:43:28–01:50:51，Engine 包 309.160s；`final-recheck-race-verified.jsonl` |
| 静态检查与构建 | `go vet ./...`、`go build ./...` 均通过；对应 `*-verified.log` |
| 独占 MySQL 定向 race | Engine 的任务计时、串行熔断、结算恢复及调度补充通过（26.208s）；App 活跃排空与多站点多阶段生命周期通过（31.535s），包括正常退出、取消、关停超时、强杀重启；`final-recheck-mysql-final.log` |
| 最后断言修正后的实库补验 | Engine settlement 全组在最终代码上再次通过（19.269s），覆盖结算失败、重启、丢失确认、计时失败、检查点不可写与轮次冲突；`final-recheck-settlement-verified.log` |
| 一致性与清理 | 最终 1,039 文件 SHA256 清单比较无差异；相对文档链接核对无缺失，`git diff --check` 通过。独占库清理回调无报错，App 明确断言随机库及夹具目录不存在，Engine 无遗留 traces 目录 |

时间为本地 UTC+8。日志及冻结清单均位于 `.review-tmp/`，不含连接凭据。全仓中的 124 项跳过仍如实保留；独占 MySQL 补验只补充上述选择的场景，不代表所有环境门控都执行过。MySQL 主批次后仅加强事件消费完成的测试断言，涉及的 settlement 实库断言已在最后批次复验；最终普通全仓和全仓 race 均在这些修改完成后运行。

因此，本次发现的三个实现问题及异步事件测试遗漏均已修复并验证。结构说明、诊断入口和框架画像已同步；最终测试完成后只补写不嵌入程序的验收报告，未再改动源码或内嵌手册。未操作业务数据，未执行 Git 写命令，已有工作区改动保留。

## 证明边界

本轮不改变单实例定位，不增加分布式租约或横向扩容要求。obs 的有界队列/等待不等于物理 CPU、内存或磁盘隔离；文件 I/O 和离线不合作回调不能被强杀，报告仍可能不完整。没有生产站点、24/72 小时长稳、断电或数据库服务崩溃实测；业务副作用幂等要求不变。独占 MySQL 测试只创建/删除自有随机库，不修改业务库。
