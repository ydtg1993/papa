# 规则运行质量与显式复分析

`pkg/rulequality` 把 RuleFetcher 的实际运行摘要变成固定报告，并把宿主选定的规则异常关联到一次受控 Agent 分析。报告基于已记录的观测，不保证覆盖所有生产任务，也不证明字段语义正确。通过验收的候选仍须人工发布。

完整可运行接线见 [examples/rulequality](../examples/rulequality/README.md)。规则采集见 [RULE_FETCHER](RULE_FETCHER.md)，样本和发布见 [RULE_STORE](RULE_STORE.md)，模型权限、预算及独立验收见 [SITE_AGENT](SITE_AGENT.md)。

## 启用与迁移

```yaml
quality:
  enabled: true
```

默认关闭。此开关只参与框架表清单、显式迁移与启动检查；不自动给 Fetcher 安装 Collector，不生成报告，也不调用模型。纯质量观测无需启用 rules/agent；复分析还需显式构造这两个组件及其存储。

启用后执行 `papa migrate` / `App.Migrate()`，新增：

| 表 | 内容 |
| --- | --- |
| `crawler_rule_observations` | 不可变、无字段值的运行摘要；单调序号和创建时间 |
| `crawler_rule_quality_reports` | 固定窗口、输入摘要、策略、指标、诊断和证据引用 |
| `crawler_rule_reanalysis_requests` | operation ID、报告、预分配 Agent RunID、revision 和结果引用 |

普通启动、`rulequality.New(db)` 不建表。关闭开关不删历史。没有新增任务列。MySQL Store 必须接普通连接，拒绝外层事务、DryRun；返回内容脱离内部对象，不允许通过修改返回值改写历史。

## 观测接线

```go
store, err := rulequality.New(db)
if err != nil { return err }
collector, err := rulequality.NewCollector(store, 500*time.Millisecond)
if err != nil { return err }
fetcherConfig.Quality = collector
fetcher, err := engine.NewRuleFetcher(fetcherConfig)
```

`RuleFetcherConfig.Quality == nil` 时不增加质量记录。启用时在每次有效执行结束记录一份观测，原错误、任务重试与业务副作用不因质量存储失败而改变。写入失败只记录稳定告警并增加 `collector.Stats().Failed`，不暴露数据库错误详情、不自动重放 Save。

Collector 默认超时 500ms，最大 3s，无后台队列、goroutine 或重试。自定义 Recorder 是可信宿主代码，必须响应 context；超时不是强制终止任意 Go 函数。计数仅覆盖该 Collector 实例的生命周期，重启归零不能视为全局“无丢失”。

观测包括站点/阶段/规则版本/release、任务轮次、请求摘要、状态/渲染标记、正文完整性、原始与去重后行数、字段计数、分页停止原因、计划/已提交子任务数、终止环节和稳定错误类别。不保存 URL、Cookie、账号、请求头、正文或字段值。

`RawRows`、字段摘要和校验结果在去重、Next、Save 之前冻结；`NewRows` 单独记录。失败提取仍可产生摘要，但不会因此进入 Save。429、认证/挑战、网络、渲染、保存及任务提交错误分别归类，不凭错误字符串判断。

有任务 ID 时，逻辑键由 Scope、任务 ID、Repeat 和请求摘要组成，不含 Retry/本地 Attempt。同一任务轮次的重试取最后一条观测作为逻辑页面，尝试错误另外计数。ID=0 的直接调用默认各算一次；宿主需要折叠重试时显式使用 `rulequality.WithLogicalKey(ctx, roundKey)`，它只保存 SHA256。键须包含本轮采集身份，不能只用 URL 折叠所有历史轮次。

## 报告与指标

```go
policy := rulequality.DefaultPolicy()
missing := 0.2
policy.MissingRate = &missing // 由业务明确允许的字段缺失比例
report, err := rulequality.BuildReport(ctx, store, rulequality.Query{
    Scope: rulequality.Scope{
        Site: "demo", Stage: "list", RuleID: "titles", Version: "v1",
        ReleaseID: release.ID, ReleaseGeneration: release.Generation,
    },
    From: start, Until: end, Limit: 10000,
    LostObservations: lostInThisWindow,
}, policy, "") // 最后一个参数可指定既有基线报告 ID
```

Scope 全字段精确匹配；空 ReleaseID 也只匹配无 release 的观测，不是通配符。时间窗口为 `[From, Until)`，使用存储创建时间。Limit=0 取 10,000；超过 10,000 拒绝。MySQL 查询在显式 REPEATABLE READ 只读事务里固定序号截止点，与并发写入/清理隔离。超量窗口返回截断标记，不能伪称整窗健康。

报告不可变，保存输入摘要、截止点、完整策略、基线 ID、折叠算法及 `Coverage=recorded_observations_only`。同一报告 ID 同内容幂等，异内容冲突。`BuildReport` 每次生成新报告 ID；只读重查使用 `store.Report(ctx, id)`。

默认策略要求至少 20 个逻辑页面、3 个不同请求摘要；提取漂移判断还要求达到相同数量的完整可评估页面/请求。默认只启用 20% 提取失败比例。其他阈值为 nil，表示未启用；显式 0 表示任何正的错误/下降即触发，不会把 0 个错误判异常。

| 指标/阈值 | 分母和含义 |
| --- | --- |
| `ExtractionFailureRate` | 完整可评估逻辑页面中，真实提取/校验失败的比例 |
| `MissingRate` | 每个字段 `(Rows - Present) / Rows` |
| `EmptyRate` | 每个字段 `(Present - NonEmpty) / Present`；null 算存在但空 |
| `MinRows` | 每个完整可评估页面的平均原始行数，低于此数才触发 |
| `MaxRowsDrop` | 与基线相比，平均原始行数的相对下降幅度 |
| `MaxFieldDrop` | 与基线相比，每个字段 `NonEmpty / Rows` 的相对下降幅度 |

数值 0、布尔 false 是有效非空值；missing、null、空字符串分别计数。Invalid 只统计规则实际报告的字段/行校验问题，不等于语义错误率。最多 64 个字段；截断、缺字段统计或不完整校验显式降为不完整。

| Verdict | 含义 |
| --- | --- |
| `insufficient` | 样本/独立请求不足，或相对下降策略缺少基线 |
| `partial` | 观测丢失、截断、字段不完整，或当前窗口无法建立可比 schema |
| `observed_ok` | 已记录样本未触发当前策略；不是全局健康保证 |
| `suspected_rule_drift` | 样本充分且满足已启用的提取/字段/行数异常阈值 |
| `upstream_failure` | 网络、HTTP、限流、认证、挑战或渲染问题 |
| `processing_failure` | 规则契约、保存、规划/提交、预算、取消或宿主问题 |

多个问题可同时出现在 Signals 和 FailureCounts；主 Verdict 不是唯一诊断。重试后成功仍保留之前的尝试错误。零新增、重复分页、正常结束和零行本身不证明改版；没有最低行数预期时，不自动推断“应该至少一行”。

基线由宿主明确选择，不自动选第一份数据。默认同站点/阶段/RuleID/Version、相同策略、完整字段校验摘要才可比较；不同 release 可以各自报告后比较。跨版本必须两份报告都采用相同的 `CompareAcrossVersions=true` 策略，且整个字段校验契约一致。摘要包含类型、Required、AllowNull、长度、正则及 Unique，排除选择器/JSON Pointer，因此选择器修正仍可比较。真实范围/策略/schema 不匹配返回 `ErrIncompatible`。当前全是上游失败、尚无 schema 时保留诊断报告并标记 `comparison_unavailable`，不执行漂移比较。

## 证据与保留

质量表只保存 EvidenceID 和摘要，复用现有 `.rule-snapshots` 观察清单及按内容去重的正文，不建立第二套原始 HTML 归档。`Observation.BodySHA256` 是原始响应摘要，对应快照的 `OriginalSHA256`；快照的 `BodySHA256` 是脱敏后文件摘要，两者可以不同。

EvidenceID 不等于文件一定已保留：任务结束才执行既有归档策略，成功任务可能未采样，正文可能因脱敏/预算被省略或已过期。应等任务结束再选现场；需要采集成功但质量变差的页面时，配置适当成功采样及 SnapshotPolicy。直接 RunHTTP 没有 Engine 的归档上下文；完整示例使用真正的队列 worker。

报告最多保留 64 条引用。清理调用 `store.PruneObservations(ctx, before, limit)`，一次最多 10,000 条，只删过期观测，不删固定报告和复分析请求。快照仍按原策略回收；报告与证据保留是独立期限。没有自动清理任务。

## 显式复分析与恢复

```go
bridge, err := rulequality.NewReanalyzer(rulequality.ReanalysisConfig{
    Store: store, Agent: agent, Audit: audit, Rules: repository,
    ArchiveDir: archiveDir, SamplePolicy: samplePolicy, Actor: actor,
})
if err != nil { return err }
request, err := bridge.Prepare(ctx, report.ID, hostOperationID)
if err != nil { return err }
out, err := bridge.Execute(ctx, request.ID, request.Revision, rulequality.ReanalysisInput{
    Goal: goal, EvidenceIDs: selectedEvidenceIDs,
})
```

`Audit` 必须是该 Agent 的同一权威存储，Rules 必须与其 Repository/Blob 存储一致，这是宿主配置责任。模型、当前 SamplePolicy、访问范围和预算仍通过 Agent 配置；报告不扩大权限。

Prepare 只接受已保存的 `suspected_rule_drift` 报告，固定唯一请求 ID、operation ID 和 Agent RunID，不调用模型。重复 operation ID 且报告/操作者/来源相同返回原请求；参数不同冲突。Execute 要求 pending＋正确 revision/操作者，目标只能含报告的一个 stage，Existing 必须是报告版本且仍绑定当前 release。原发布已变化时，应重新观察并生成对应报告。

Execute 先检查输入和报告内选定证据，再 CAS 到 running、固定输入摘要；成功提交才有执行权。最多选 8 份快照，使用 `rulestore.ReadSnapshotMatching` 核对原始摘要及留存正文完整性，再以当前 SamplePolicy 批准进入样本库。缺少、损坏、被替换或不允许披露的现场返回 `needs_evidence`；不会为了补现场重抓生产 URL。

这些导入样本通过 `Goal.EvidenceSamples` 进入 Agent，只有观察数据，不自动生成字段正确值、训练预期或隐藏验收。`Goal.Context` 接受宿主批准的非敏感说明；桥接额外注入有界类别/计数。真正的 TrainingSet、HoldoutSet 由宿主提供，隐藏样本仍按正文摘要和 URL 隔离，没有独立验收时返回 `needs_fixtures`。完整目标/请求有预算，长反馈显式截短并记录省略数量。

```text
pending → running → awaiting_review / needs_evidence / needs_fixtures
                  / failed / canceled / interrupted
pending → dismissed
```

Agent `RunWithID(ctx, id, goal)` 只接受 1–64 个 ASCII 字母、数字、下划线或连字符。它复用原子 Audit.Start；已有 ID、未知提交结果都不能重复模型调用。原 `Run` 继续自行生成新 ID。

遇到 `ErrUnconfirmed` 先按请求 ID/operation ID 查询，不能因为读到 running 就取得执行权，也不能换 operation ID 重试付费请求。重启不自动接管。宿主确认原执行者已经结束后，显式调用 `bridge.Reconcile(ctx, id, revision, true)`：读取 Agent 审计，必要时中断未完成审计，或回填已完成的候选/样本引用；绝不重放 Complete。`workerStopped=true` 是宿主事实声明，不是分布式租约检测。

即使返回 awaiting_review 且 Verified=true，也只产生候选。现行 Current、版本发布和回滚均由人工调用原规则仓库接口决定。

## 按需扩展边界

`CapabilityGaps(report)` 根据已有错误提供 `needs_authentication`、`insufficient_evidence`、`budget_exhausted`、`manual_investigation` 等建议；复分析也附加缺验收/预算诊断。不从普通渲染失败猜测“需要点击”，当前没有证据就不产生 needs_interaction / unsupported_source 结论。

本批没有新增点击/滚动/验证码处理、账号权限、代理轮换、任意 JS、自动模型调度、自动发布/回滚、外部通知、后台页面或通用质量 CLI。需要这些能力时按实际站点证据单独扩展权限、预算和回归。
