# 持久规则仓库、样本与发布

> 面向：希望在重启后保留规则、验收记录与发布历史，并让新入口使用已发布版本的业务。
> 配套：[离线规则](./CRAWL_RULES.md)、[规则执行器](./RULE_FETCHER.md)、[受控网站分析](./SITE_AGENT.md)、[配置与迁移](./CORE_CONFIG.md)、[数据落地](./DATA_LANDING_GUIDE.md)。

`pkg/rulestore` 使用 MySQL 保存规则与发布元数据，使用 `FileBlobs` 保存经明确脱敏策略允许的样本正文。仓库实现 `crawlrule.Catalog`，可交给 `RuleFetcherConfig.Catalog`。原来的内存 `Registry` 与静态 `Bindings` 仍可使用。

## 1. 先运行完整示例

在 papa 模块根目录执行，DSN 指向**已创建的专用示例库**：

```powershell
$env:GOWORK = 'off'
$env:PAPA_RULE_DSN = '<专用 MySQL 示例库 DSN，包含 parseTime=True>'
go run ./examples/rulepublish -migrate -sample-dir ./.rule-demo-samples
```

`-migrate` 是显式建表操作；之后可去掉该参数再运行。示例只访问自己启动的本机 HTTP 服务，创建两组列表/详情规则，验收并发布 v1、v2，重建仓库对象，执行固定的旧任务与新任务，再回滚。输出应包含 `generations: [1, 2, 3]`、旧任务 `Alpha`、新任务 `Beta`、回滚后 `Alpha`，以及样本恢复与子任务固定版本的布尔结果。

源码为框架仓库的 `examples/rulepublish/main.go`。每次运行使用不同站点/规则标识，保留示例元数据与样本，便于检查；它不会自动清空数据库。生产任务仍应走 `FetchHandler` 和引擎队列，示例用 `RunHTTP` 演示同一规则执行逻辑。

## 2. 配置、迁移与构造仓库

```yaml
rules:
  enabled: true
  sample_dir: ./rule-samples
```

| 项 | 行为 |
| --- | --- |
| `rules.enabled` | 默认 `false`；控制框架迁移及启动缺表检查是否包含规则与周期状态的 7 张表 |
| `rules.sample_dir` | 开启时必填，空串或纯空白会在加载配置时失败；不自动补默认值 |
| `papa migrate` / `App.Migrate()` | 显式创建规则表；普通启动、`rulestore.New` 和 `NewFileBlobs` 都不迁移数据库 |
| 宿主接线 | 开关不会自动构造 Repository、注册规则或把 fetcher 切到 Catalog，须由业务代码完成 |

在业务项目中先开启配置，再执行 `papa migrate`。在已拥有 `db *gorm.DB` 与 `cfg *config.Config` 的应用组装代码中：

```go
blobs, err := rulestore.NewFileBlobs(cfg.Rules.SampleDir)
if err != nil { return err }
defer blobs.Close() // 生命周期须覆盖所有使用它的仓库操作与 worker

repo, err := rulestore.New(db, blobs)
if err != nil { return err }
```

`NewFileBlobs` 创建并打开目录；配置加载本身不创建它。元数据使用已有 MySQL 连接，构造器拒绝非 MySQL、DryRun 或 `db.Begin()` 等外层事务连接。发布必须拥有最外层事务的 COMMIT，不能把保存点释放当成持久提交。普通连接与开启 PrepareStmt 的普通连接均可使用。关闭进程时先结束相关操作，再关闭文件库与数据库。

7 张表分别为 `crawler_rule_versions`、`crawler_rule_samples`、`crawler_rule_sample_sets`、`crawler_rule_verifications`、`crawler_rule_releases`、`crawler_rule_sites`、`crawler_rule_schedules`。最后一张供 [RuleSchedule](./RULE_FETCHER.md#完整周期采集批次) 保存控制任务的批次序号、固定入口和摘要；不会保存页面正文。已启用 rules 的旧库升级后也要再显式迁移一次补表。关闭开关后再迁移不会删除已有表或发布历史，也不会使手工持有的 Repository 停止工作。

周期采集每个新批读取一次当前完整发布；上一批重试或重启恢复继续使用已持久化的版本快照。发布和回滚均不改写运行中的批次，也不会自动使失败批次跳过。

如需模型辅助生成候选，再开启 `agent.enabled`（依赖 `rules.enabled`）并显式迁移两张审计表 `crawler_agent_runs`、`crawler_agent_steps`。宿主通过 `crawlagent.New` 复用此仓库的样本、草稿与 Verify，通过 `agentstore.New` 保存运行/步骤；构造器不建表，开关不自动分析。接线和回放/真实模型示例见 [SITE_AGENT.md](./SITE_AGENT.md)。

样本 `metadata_json` 使用 LONGTEXT，应用仍限制元数据大小。若已经用第三批早期代码建成 TEXT 列，需再显式运行一次 `papa migrate` 扩容；已在 MySQL 验证保留原记录。普通启动不会执行类型升级，已有较大样本表应安排低峰期迁移。

## 3. 从草稿到验收

每次写入必须传入 `rulestore.Actor{Operator: ..., Source: ...}`：两项均非空、首尾不能有空白，长度分别最多 128/256 字节。它们记录操作者与来源；调用 API 的宿主负责认证与授权。

| 步骤 | API | 保证 |
| --- | --- | --- |
| 创建草稿 | `repo.CreateDraft(ctx, rule, actor)` | 编译后保存不可变规则；相同 ID/版本再次创建返回 `ErrConflict`，修改内容必须换版本 |
| 保存样本 | `repo.AddSample(ctx, input, policy, actor)` | 先完成脱敏正文的文件发布，再写 MySQL 元数据 |
| 固定样本集 | `repo.CreateSampleSet(ctx, setID, fixtures, actor)` | 固定样本 ID、正文/元数据摘要与显式预期；相同集合 ID 不可覆盖 |
| 验收 | `repo.Verify(ctx, ref, setID, actor)` | 使用固定样本离线执行，保存成功或失败报告；不发布 |
| 查看候选 | `repo.Get(ctx, ref)` | 读取规则、状态、摘要；可查看未发布的草稿 |
| 查看记录 | `repo.Sample` / `SampleSet` / `Verification` / `Verifications` | 查询样本元数据、集合和报告；列表查询必须传 1..100 的 limit |

例如，对已明确可保留的公开测试正文建样本和预期：

```go
actor := rulestore.Actor{Operator: "operator-name", Source: "fixture-review"}
sample, err := repo.AddSample(ctx, rulestore.SampleInput{
    URL: "https://example.com/page", StatusCode: 200,
    Body: []byte(`<h2>Alpha</h2>`),
}, htmlfetch.SamplePolicy{
    RedactBody: func(_ bool, body []byte) ([]byte, error) {
        return body, nil // 仅用于已确认可公开的测试正文；不是自动脱敏
    },
}, actor)
if err != nil { return err }
err = repo.CreateSampleSet(ctx, "list-fixtures-v1", []rulestore.StoredFixture{{
    Name: "normal-page", SampleID: sample.ID,
    Expect: crawlrule.Expectations{
        RowCount: 1,
        Fields: []crawlrule.FieldExpectation{{Pointer: "/title", Value: "Alpha"}},
    },
}}, actor)
if err != nil { return err }
verification, err := repo.Verify(ctx,
    rulestore.Reference{ID: "list", Version: "v1"}, "list-fixtures-v1", actor)
if err != nil { return err }
// verification.Passed 为 true 仍不等于已经发布。
_ = verification
```

在这段之前应通过 `CreateDraft` 创建包含 `title` 字段的规则；可编译运行的完整流程见示例。

验收报告绑定规则摘要、样本集摘要和验收器版本。样本预期失败或样本正文损坏时返回报告与 `crawlrule.ErrFixtureFailed`，报告仍可按 ID 查询；数据库读取/写入错误直接失败，不冒充验收成功。每个版本最新一次成功验收作为普通发布的凭据，后续失败验收会清除该凭据。

并发验收按完成事务提交的顺序更新最新凭据，不按调用开始时间排序。失败或取消的数据库事务不会改写凭据；验收已经提交后，调用方取消不代表撤销。已发布记录固定的历史凭据不会被后续验收替换。CreateSampleSet 会复制并归一化预期，不改写调用方值；调用期间仍不得并发修改传入的对象。

| `Version.State` | 含义 |
| --- | --- |
| `draft` | 已保存，尚未完成一次持久验收 |
| `pending` | 未发布版本最近一次验收未通过 |
| `verified` | 未发布版本最近一次验收通过 |
| `published` | 这个版本曾经发布；不能用它判断当前站点正在使用哪个版本 |

当前线上完整绑定看 `repo.Current(ctx, site)`。曾发布版本的 `EverPublished` 保留，即使回滚或再次验收失败，已经固定该版本的任务仍能解析它。

## 4. 样本与短期快照分开保存

`SampleInput` 只接收 URL、真实状态码、Rendered、正文和可选 EvidenceID，没有 Cookie、请求头、响应头或请求正文。`policy.RedactBody` 必填，只允许其返回的字节入库；URL 默认移除 userinfo、fragment 与未列入 `QueryKeys` 的查询参数，路径中的敏感部分由 `RedactURL` 处理。任一回调失败时不回退保存原文。EvidenceID 和允许保留的 URL 部分也不能包含凭据。渲染 DOM 必须是 `Rendered: true, StatusCode: 0`；HTTP 保留实际状态，非 2xx 可留作失败样本，但不能通过要求成功响应的规则验收。

正文只写到 `<sample_dir>/<SHA-256>`，数据库 JSON 不含正文。相同脱敏字节复用文件；每次读取验证摘要和文件类型。文件库需要支持硬链接，目录与文件在 POSIX 下使用 0700/0600，Windows 部署还应按服务账号设置目录 ACL。`repo.Sample` 返回元数据；显式读取正文用 `blobs.Get(ctx, sample.BodySHA256, rulestore.MaxSampleBodyBytes)`。

样本目录独立于 `crawler.archive.dir`，不受 `archive.retention` 管理，也没有自动 GC。正文发布成功而数据库写入失败时允许留下无引用文件，避免误删并发操作共用的正文。备份和恢复应同时覆盖 MySQL 元数据与长期样本目录。

把已保留的失败快照提升为长期样本，必须显式选择一个 EvidenceID，再应用长期样本策略：

```go
sample, err := repo.ImportSnapshot(ctx, archiveDir, evidenceID, samplePolicy, actor)
```

`ReadSnapshot` 检查清单格式、EvidenceID、实际正文长度与摘要；不重新请求网站。没有正文、已过期、尚未发布完整或损坏的快照会失败。`ImportSnapshot` 即使面对已经脱敏的快照仍要求 `RedactBody`，保存独立正文并保留 EvidenceID 来源；之后 archive 清理不影响样本。不会批量自动导入全部失败快照，也不承诺与 archive 物理共用同一份文件。

## 5. 完整站点发布、并发与回滚

一次发布提交本站完整的 `stage → ID/version` 映射；它是替换整组绑定，不会把遗漏阶段从旧发布中自动补回来。每个引用版本必须已有匹配的成功验收记录。

```go
release, err := repo.Publish(ctx, operationID, "shop", expectedGeneration,
    map[string]rulestore.Reference{
        "catalog": {ID: "list", Version: "v1"},
        "detail":  {ID: "detail", Version: "v1"},
    }, actor)
if err != nil { return err }
```

首次发布的 `expectedGeneration` 为 0；后续使用 `Current` 返回的 generation。MySQL 事务将发布记录、完整绑定、站点指针和递增 generation 一起提交。

每个 `Release.VerificationIDs` 还固定 `stage → 验收报告 ID`，与该次完整绑定一起保存。后续重新 Verify 改变版本的最新凭据时，原发布仍能追溯当时使用的报告；`Current`、`History`、`LookupOperation` 返回的 Release 均带此信息。回滚保留并校验目标发布的历史验收凭据。

发布和回滚会核对报告是否覆盖固定样本集的全部条目、名称与成功预期，缺项或内容不一致返回 `ErrCorrupt`，事务不改变当前发布。Publish 会复制绑定供事务使用，不改写调用方 map；初始复制期间调用方仍不得并发写入。相同只读 map 可供多个调用使用。

| 情况 | 处理 |
| --- | --- |
| generation 已被另一个发布/回滚改变 | 返回 `ErrGenerationConflict`；重新读取 Current 并决定是否重做 |
| 相同 operationID 和完全相同参数重试 | 返回先前已提交的发布；expectedGeneration 和 Actor 也属于参数，重试时保持不变 |
| 相同 operationID 被用于不同参数 | 返回 `ErrOperationConflict`；operationID 在仓库中唯一 |
| 事务是否提交未知 | 用同一 operationID 调 `LookupOperation`，或原样重试；不要立即换 ID 当成新发布 |
| 未验收/验收不匹配 | 返回 `ErrUnverified`；不能用 Shadow 代替正式验收 |

回滚调用 `repo.Rollback(ctx, operationID, site, expectedGeneration, actor)`，也创建新发布记录并递增 generation。它沿当前版本的前驱继续回退：先发布 A、B、C 后，两次回滚依次得到 B、A，不会在 B/C 之间来回切换。无更早前驱返回 `ErrNoRollback`。回滚目标必须曾经发布；它不撤销旧版本的可执行资格。`History(ctx, site, limit)` 返回按 generation 倒序的发布与回滚历史。

人工后台发布可使用 `PublishReviewed(ctx, operationID, site, expectedGeneration, bindings, verificationIDs, actor)`：`verificationIDs` 必须为每个阶段固定本次审阅的验收 ID。事务持有版本锁后核对最新指针，重验导致依据变化会返回 `ErrReviewConflict`，即使新报告也通过也须重新审阅。此映射进入幂等摘要，同 ID 改变审阅依据会冲突；原 `Publish` API 保持原语义。

`rulestore.NewAdmin(db)` 提供元数据管理门面，包括 `Versions`、`Inspect`、`Releases`、`Verification`、`Preview`、`Change`、`Operation`；需要普通 MySQL 连接，不接受事务或 DryRun，不创建样本目录、不暴露导入/验收方法。列表固定每页 20 条，`Inspect` 另给最近 5 条验收；预览与当前发布/历史使用只读一致快照。调用方仍须提供有界 context 与鉴权，`Actor` 只是审计信息。标准 App 的后台用法、操作 ID 恢复与 HTTP 接口见 [MONITOR §13](MONITOR.md#13-规则运维)。


## 6. 接到 RuleFetcher，并恢复旧任务

```go
fetcher, err := papa.NewRuleFetcher(papa.RuleFetcherConfig{
    Site: "shop", Stage: "catalog", Catalog: repo,
    Origins: []string{"https://example.com"},
    Links: []papa.RuleLink{{Field: "url", Stage: "detail"}},
    Save: savePage,
})
if err != nil { return err }
task, err := fetcher.NewTaskContext(ctx,
    papa.RuleRequest{URL: "https://example.com/products"}, nil)
if err != nil { return err }
return engine.SubmitTask(task)
```

Catalog 与 `Registry`、静态 `Bindings` 互斥。新入口调用 `NewTaskContext` 时读取一次完整发布快照，再验证引用版本；这不表示整个构造过程只有一条 SQL。为数据库访问传入有超时的 context；`NewTask` 仍可用，但内部使用 `context.Background()`。

新入口及恢复执行都会检查固定规则是否兼容当前阶段的宿主配置：browser 只能用 HTML；ItemKey、详情链接及其 Meta 字段必须存在；详情链接不能声明为非字符串类型或最终转换为 number/bool。不兼容时在页面请求、Save 和子任务提交前返回不可重试错误。缺失、未发布、内容损坏的版本同样不可重试；自定义 Catalog 可返回 `crawlrule.ErrInvalidVersion` 表示永久无效版本，数据库故障和超时保持可重试。

任务将完整绑定、release ID/generation 和请求身份写入既有 Meta。执行、分页、详情派生、重试与重启恢复都沿用固定版本，不再读取站点当前指针；同一规则重新发布的新 generation 也形成不同的新入口身份。`RulePage.ReleaseID/ReleaseGeneration` 及快照中的对应信息可用于业务记录与证据关联，原静态 Registry 任务这两项为空/0。

重启后重新连接数据库、打开相同样本目录并构造 Repository/Catalog 即可，规则与发布历史从 MySQL 读取，不需要重新 `Activate`。未知格式、摘要不匹配、缺失版本或无有效发布明确失败，不猜测回退到其它版本。原内存 Registry 用法仍须自行重载并验证任务引用的版本；没有 release 元数据的旧任务保持原绑定语义。传输方式、链接映射、Save 等 Go 回调仍由部署代码提供，发布并不固定整套业务代码。

从 Registry 切换到 Catalog 前，必须将所有尚有待执行任务引用的旧规则按原 ID/版本导入 MySQL，完成样本验收并至少发布一次，确保它们都能被 `repo.Resolve` 读取。之后可再发布最新完整绑定作为 Current；旧版本的 EverPublished 保留。仅在旧 Registry 中注册/Activate 不够，系统不自动迁移内存规则。缺失或从未发布的旧版本会在任务执行时明确失败，不发送页面请求、不 Save，也不会临时选用当前版本。

## 7. 结构对比与影子评估

| API | 输入与输出 |
| --- | --- |
| `rulestore.CompareRules(current, candidate)` | 比较两份已编译规则的规范结构，包括字段、选择器、转换、校验、分页及预算；返回摘要与变化路径/类型，不复制规则值，也不推断不同 CSS 是否等价 |
| `rulestore.Shadow(current, candidate, input)` | 两份规则顺序执行于同一份已取得的响应或 DOM 私有副本，返回成功与错误类别、行数、缺字段数、结果摘要及分页变化 |

HTTP 使用 `ShadowInput{Response: response}`；渲染 DOM 使用 `Rendered: true` 并保持响应 StatusCode 为 0。Shadow 没有请求客户端、Save 或任务提交接口，不额外抓取网站，不写业务结果，不改变发布状态。规则执行失败归入各侧的 `ErrorKind`；输入非法或预算超限返回顶层错误。

对比与影子报告目前只作为函数返回值交给宿主，不自动持久化。报告不复制正文或提取字段值，但仍应按业务数据管理摘要。即使两侧结果一致，也须用显式样本预期 `Verify` 后再显式发布。

## 8. 预算与职责

| 对象 | 上限 |
| --- | --- |
| 单个样本输入/脱敏正文 | 各 8 MiB，超限拒绝，不截断冒充完整样本 |
| 样本 URL / EvidenceID | 16 KiB / 256 字节 |
| 样本集 | 1..64 个 fixture，正文合计 32 MiB，序列化元数据最多 1 MiB；每份 fixture 最多 1024 个字段预期，集合合计最多 4096 个 |
| 站点完整绑定 | 1..32 个阶段 |
| 历史列表查询 | limit 为 1..100 |
| 规则结构差异 | 最多 128 项，超出用 Truncated 标记 |
| 影子结果 | 10 万字段单元、16 MiB 序列化预算，另有嵌套和遍历预算 |

仓库负责持久数据与事务一致性，宿主负责身份授权、选择样本、脱敏、备份、上线策略和调用时机。当前提供 Go API、完整示例及 [OA 规则运维中心](MONITOR.md#13-规则运维)的人工发布/回滚入口；HTTP 操作需要具名身份、当前代次和操作 ID，不自动授予生产发布权限。尚无专用发布 CLI。

[受控分析 Agent](./SITE_AGENT.md) 可在宿主范围和预算内生成/修正规则候选，使用预先固定的训练样本与独立验收集。它只调用仓库的样本、草稿、查询和 Verify 接口；`awaiting_review`、`verified=true` 表示独立验收通过待审阅，缺少独立样本则为 `needs_fixtures`。发布仍由宿主审阅后显式调用 Publish，并使用实际 generation 与新 operationID；Agent 不改变 Current。
