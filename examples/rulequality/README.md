# 运行质量反馈的完整本地示例

从 Papa 仓库根目录运行。示例使用**真实 MySQL、真实 Engine 任务队列、真实 RuleFetcher 和其归档快照**；网站由本进程在 loopback 启动。默认且唯一模型是确定性 Replay，不需要 API Key，也不访问外部网站。

先由用户创建专用数据库，名字必须含 `quality_test`。普通运行不建表，首次运行显式加 `-migrate`：

```powershell
$env:PAPA_QUALITY_DSN = 'root@tcp(127.0.0.1:3306)/papa_quality_test_example?charset=utf8mb4&parseTime=True&loc=Local'
go run ./examples/rulequality -migrate -data-dir ./quality-example-data
```

后续运行去掉 `-migrate`。每次生成独立站点身份，保留之前的报告、候选和审计；不删除数据库或目录。`-data-dir` 包含长期样本 `samples/` 和 Engine 的 `archive/`，保留它才能再次检查引用证据。

迁移启用 `rules.enabled`、`agent.enabled`、`quality.enabled`。Engine 显式配置 `crawler.archive.enabled=true`、`mode=always` 和只适用于本示例公开页面的正文批准策略。`quality.enabled` 本身不会采集或调用模型，代码另外连接 Collector 并显式执行复分析。

示例先验证并发布宿主写好的 v1，然后以默认最少 20 页、3 种请求身份的质量门槛运行：

| 本地场景 | 任务结果 | 固定质量报告 |
| --- | --- | --- |
| 20 个正常页面 | 全成功 | `observed_ok`，保存为显式基线 |
| 20 个页面把 `.title` 改成 `.heading` | 全成功，optional title 丢失 | 显式 MissingRate=25% 识别 `suspected_rule_drift` |
| 20 个 HTTP 429 | 全失败 | `upstream_failure`，不建议规则修正 |
| 20 组重复分页，共 40 次任务 | 全成功，20 次 repeated-content 正常停止 | `observed_ok`；原始行 40、新增行 20 |
| 20 次业务 Save 故障 | 全失败 | `processing_failure`，不建议规则修正 |

每个窗口使用数据库时间，报告固定输入截止序号、策略和完整 release/规则身份。成功/失败任务数从实际任务表核对。统计基于已记录观测，不等于完整生产覆盖。

随后单独注入一次质量 Recorder 写故障：业务 Save 仍执行一次，任务成功，Collector.Failed 增加 1。这个故障发生在上述报告冻结之后；未来报告必须通过 `Query.LostObservations` 明确带入损失，不能声称整段健康。示例的业务 Save 是内存幂等 upsert；MySQL 持久化的是任务、规则、质量与 Agent 元数据。

只有字段丢失报告可创建复分析请求。宿主从该报告选取一个**真实 worker EvidenceID**，桥接层读已有快照、重新批准后传给 Agent；不写假快照，也不为了缺失证据重新抓页面。训练和隐藏验收集由宿主分别预先创建，具有不同正文和明确期望，不从线上错误结果推导“正确值”。

质量报告里的 `BodySHA256` 是原始完整响应摘要，对应快照 manifest 的 `OriginalSHA256`；归档 manifest 的 `BodySHA256` 校验的是脱敏后的保存正文，两者可能不同。复分析同时验证报告引用与原始摘要的对应关系，以及保存正文自身的完整性。本例页面完全由宿主编写，批准策略保留正文，所以两种摘要恰好相同。

Replay 共 4 次决定：读取已批准证据、提交 `.title,.heading` 候选、检查固定训练集、结束并交给宿主检查隐藏集。通过后状态为 `awaiting_review`，输出包含 report/request/run/candidate/verified、初始与当前 release、Replay 调用数。初始 v1 release 始终不变；没有自动发布、回滚或暂停。

同一 operationID 重复 Prepare 得到同一个请求。示例重新打开持久 Store 后再次 Execute 已完成请求，确认被拒且调用数不增加；另建一次“归档目录不可用”的显式请求，结果是 `needs_evidence`、0 次新增模型调用。没有盲目重放 running 请求。

可以只读重开刚输出的请求；此命令不迁移、不创建 Agent、不执行模型：

```powershell
go run ./examples/rulequality -inspect-request recheck-替换为输出的请求ID
```

集成测试显式启用专用数据库，未提供时会标记 skip：

```powershell
$env:PAPA_QUALITY_EXAMPLE_TEST_DSN = $env:PAPA_QUALITY_DSN
go test ./examples/rulequality -count=1
$env:CGO_ENABLED = '1'
go test -race ./examples/rulequality -count=1
```

Replay 用于验证状态机、真实持久化、快照接线和独立验收，不衡量真实模型修正规则的能力或计费。此示例没有真实模型开关。
