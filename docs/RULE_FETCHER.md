# 规则驱动的采集流程

> 面向：已经有 HTML/JSON 规则，希望复用框架队列运行列表、详情和分页的业务。配套：[规则与样本](./CRAWL_RULES.md)、[账号运行](./ACCOUNT_RUNTIME.md)、[数据落地](./DATA_LANDING_GUIDE.md)、[运行质量反馈](./RULE_QUALITY.md)。

`papa.NewRuleFetcher` 创建普通 `Fetcher`。每次执行依次获取页面、提取与转换、校验、规划子任务、幂等保存、提交详情和下一页。生产入口走原有 `Engine.SubmitTask`，恢复、错误重试和任务状态仍由引擎管理。

## 先跑完整示例

在 papa 模块根目录执行：

```powershell
$env:GOWORK = 'off'
go run ./examples/rulecrawl
go run ./examples/rulecrawl -mode=json
```

示例只访问自己启动的本地 HTTP 服务。HTML 模式演示列表链接分页，JSON 模式演示同 URL 的只读 POST 游标分页；两种模式均获得两页列表、两页详情、两条业务记录，并重放详情验证幂等写入。源码和生产 StageSpec 接线见框架仓库的 `examples/rulecrawl/README.md`。

示例的 `RunHTTP` 使用有界内存队列，不需要数据库；生产使用 `FetchHandler` 及真实 Engine。测试另覆盖实际引擎 worker/提交/状态写入链路，但测试任务数据库是假库，不能替代 MySQL 部署验收。

## 配置与绑定

```go
fetcher, err := papa.NewRuleFetcher(papa.RuleFetcherConfig{
    Site: "shop", Stage: "catalog", Registry: registry,
    Bindings: map[string]papa.RuleRef{
        "catalog": {ID: "shop-list", Version: "1"},
        "detail": {ID: "shop-detail", Version: "1"},
    },
    Origins: []string{"https://example.com"},
    ItemKey: "id", MaxPages: 100, MaxItems: 10000,
    Links: []papa.RuleLink{{
        Field: "url", Stage: "detail",
        Meta: map[string]string{"product_id": "id"},
    }},
    Save: savePage, // func(context.Context, *papa.Task, papa.RulePage) error
})
if err != nil { return err }
task, err := fetcher.NewTask(papa.RuleRequest{URL: "https://example.com/products"}, nil)
if err != nil { return err }
return engine.SubmitTask(task) // 先为 catalog、detail 登记各自的 StageSpec
```

所有绑定版本必须先在同一 Registry 中注册并通过 fixture 激活。`NewTask` 固定本阶段 ID/版本及整条流程的阶段版本映射；执行、详情派生、分页和重试都不查询 Active。更新配置里的默认绑定只影响新入口任务。构造器复制 bindings/origins/link mappings，外部修改不会改变已创建的 fetcher。

`RuleFetcher` 不自动投入口；显式 `NewTask + SubmitTask`，或自行包装 `SubmitEntries`。详情阶段也要创建 RuleFetcher，并配置自己的 Save、Origins 和传输方式。链接与元数据映射都是显式字段名；映射值为字符串时原样保存，其余 JSON 值编码为 JSON 文本。不自动复制父任务的全部业务 Meta。

### 使用持久 Catalog

需要整组发布和重启恢复时，将已构造的 `rulestore.Repository` 传给 `RuleFetcherConfig.Catalog`，同时省略 `Registry` 与 `Bindings`；两种配置互斥。`rules.enabled` 只控制迁移/缺表检查，不自动创建仓库或切换 fetcher。配置、显式迁移、样本验收和完整示例见 [RULE_STORE.md](./RULE_STORE.md)。

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

`NewTaskContext` 每个新入口读取一次完整发布快照，并验证其中的固定版本；为数据库调用使用有超时的 context。之后执行、派生子任务与重试不查当前指针。任务保留整组 bindings、ReleaseID 和 generation，新发布不会使正在运行的流程混用版本；同一组版本再次发布后的新 generation 也进入入口身份。原 `NewTask` 包装此方法并使用 `context.Background()`，静态 Registry 用法保持兼容。

## 获取页面与登录状态

| 配置 | 行为 |
| --- | --- |
| `Transport: "http"`（默认） | 通过 Engine.FetchHTTP 复用 HTMLClient；规则决定 HTML 或 JSON |
| `Transport: "browser"` | 隔离 Rod 上下文取得 DOM，仅支持 HTML；`WaitSelector` 可等待目标元素 |
| `AccountID` 非空 | NewTask 固定账号标识；HTTP/Rod 分别走 FetchAccountHTTP / WithAccountBrowser，凭据由已配置的 Manager 管理 |
| `BuildRequest` | 根据固定 RuleRequest 构造 Headers/Body；不得改变 URL，只支持 GET、HEAD、显式可重放 POST |

`Origins` 必填，按 scheme/主机/有效端口限制请求及结果，不是 URL 路径前缀。详情链接和下一页都要通过范围校验。HTTP 一律禁止跟随重定向（包括同源），3xx 明确失败；请配置最终地址。浏览器沿用页面状态工具的 origin 检查，不能充当阻断所有跨源子资源/重定向的网络沙箱。账号 Rod 只导航到账号 bootstrap 所在 origin，回调不替换账号管理器的认证请求头。

HTTP 客户端和浏览器池仍需按原配置启用；规则执行器不自动启动它们。账号信息由任务固定，不会因运行时修改默认 AccountID 而切换已有任务。任务 Meta/规则/cursor 只能存非凭据数据；Cookie、Authorization、CSRF 放入账号管理器或运行时 BuildRequest。账号入口本身不自动归档响应正文；RuleFetcher 的响应快照须开启归档，并显式配置下文的正文策略。

`RulePage` 带最终 `SourceURL`、规则版本、原始响应的 `BodySHA256`、字段证据及分页序号；Catalog 任务还带 `ReleaseID/ReleaseGeneration`，静态 Registry 任务为零值。存在引擎归档上下文时还带 `EvidenceID`。HTTP 带实际 StatusCode，Rod 为 `Rendered=true / StatusCode=0`，不把渲染成功冒充 HTTP 200。

## 失败快照与业务证据关联

生产 `FetchHandler` 由 Engine 执行时，开启 `crawler.archive.enabled` 即接入本次尝试的快照缓冲，沿用 `archive.dir / mode / retention / max_file_mb`，没有新增 YAML 配置或框架数据库列。直接调用 `Run` / `RunHTTP` 不会自行创建归档上下文，通常既不自动归档，也不产生 `EvidenceID`。

`failure` 模式在尝试失败（包括 handler panic）时落盘；`always` 模式也保留成功尝试。覆盖已经获取的 HTTP 非 2xx 页面、解析/校验失败、Save 或子任务提交失败，以及 Rod 等待失败时仍可读取且已确认来源的 DOM。未取得响应、正文读取不完整、无法确认浏览器来源或无法读取 DOM 时，只保留可确认的元信息与省略原因，不伪造正文或 HTTP 状态。

### 明确决定可以保存哪些响应数据

`RuleFetcherConfig.SnapshotPolicy` 使用 `*htmlfetch.SamplePolicy`（导入 `github.com/ydtg1993/papa/v4/pkg/htmlfetch`）：

| 策略 | 快照行为 |
| --- | --- |
| `nil` 或 `RedactBody == nil` | 省略正文；匿名页面也一样。需要保留的尝试仍写元信息清单 |
| `RedactBody` | 收到响应正文的私有副本，`requestSide` 始终为 `false`；只有返回的字节可以落盘 |
| `QueryKeys` | URL 查询参数白名单；默认全部去掉。保留值须由业务确认可公开 |
| `RedactURL` | 在默认去掉 userinfo、fragment 及未允许的查询参数之后处理 URL；路径包含账号、签名等信息时在此脱敏 |
| `Headers` | 此处不使用；请求头、响应头和请求正文均不进入规则快照 |

例如，给已确认不含凭据、账号信息或用户数据的公开页面显式允许正文归档，在创建 fetcher 前设置：

```go
cfg.SnapshotPolicy = &htmlfetch.SamplePolicy{
    QueryKeys: []string{"page"},
    RedactBody: func(_ bool, body []byte) ([]byte, error) {
        return body, nil // 原样保留的明确授权，不执行自动脱敏。
    },
}
```

登录页面或业务 API 应按已知结构删除敏感字段，再返回允许保存的内容；普通关键词替换不能保证任意页面已脱敏。URL/正文脱敏回调报错或 panic 时不写原文，只记录固定的省略原因。快照策略仅处理归档副本，`Save` 收到的 `SourceURL`、记录和字段证据仍是采集结果，业务落库时须另行决定保留范围。回调需要支持并发调用。

### 用同一 ID 找到业务记录、Trace 和文件

Save 可以把 `page.EvidenceID` 和业务记录一起保存；引擎在尝试结束时写入 Trace 的「规则页面证据」步骤和快照清单，三处使用同一个 ID。每次重新抓取生成新的观察 ID；幂等 upsert 应明确是覆盖当前证据引用，还是另存每次尝试的关联历史。

`EvidenceID` 是关联键，不是落盘承诺：Save 执行时尚不知道最终结果，`failure` 模式的成功尝试只在 Trace 留 `not-retained` 引用，不创建清单或正文。开启 `crawler.trace.enabled` 才写 Trace；即使 Trace 关闭，已启用的快照归档仍独立工作。成功尝试只额外保留这条框架生成的最小引用，普通 `Step` 的 data 仍沿用既有保留规则。

| Trace 的 `state` | 含义 |
| --- | --- |
| `stored` | 清单与脱敏正文均已发布，`files` 给出相对路径 |
| `metadata-only` | 清单已发布，正文因策略、预算或不可取得等原因省略；查看 `body_omitted` |
| `not-retained` | `failure` 模式的成功尝试，没有归档文件 |
| `write-failed` | 归档发布失败，只保留最小引用并记 WARN，不提供未发布文件的链接 |

文件均在 `archive.dir` 下：

```text
.rule-snapshots/observations/<32位十六进制 EvidenceID>.json
.rule-snapshots/bodies/<脱敏正文 SHA-256>.body
```

`.rule-snapshots` 是保留的点号前缀目录，避免与既有按站点命名的 HTML 归档目录冲突。

清单记录规则 ID/版本、Catalog 任务的 release ID/generation、任务/站点/阶段、attempt/retry/repeat、脱敏 URL、规则声明的 HTML/JSON `format`、状态码、是否渲染、失败标记和受限校验报告。`RulePage.BodySHA256` 对应原文；清单在来源处理成功、原文可取得且未超过输入预算时记录同一 `original_sha256`，`body_sha256` 则只计算脱敏后的实际文件字节，两者可能不同。`.body` 保存实际响应或 DOM 字节；失败响应可能不符合规则格式，例如 JSON 接口返回 HTML 登录页。清单不是 `htmlfetch.WriteSample` 的样本格式，不能直接交给 `ReadSample`。

相同脱敏正文在不同尝试间共用一份内容文件；不同观察各有清单，Trace 仅保存 ID、状态、摘要和路径，不复制正文。清单最后原子发布；写盘失败不改变任务结局。正文输入与脱敏输出分别受 `archive.max_file_mb` 限制，每次尝试最多 32 个规则观察，单份清单最多 256 KiB，校验问题最多保留 100 条。超限不截断正文冒充完整响应。

保留期清理先删除过期观察，再检查存活清单的引用；仍被引用的正文不会因自身修改时间较旧而删除。无法读取或识别存活清单时，本轮保守跳过正文回收。发布与清理只在同一进程内协调，多进程必须使用各自的归档目录。现有 `FetchHTML` 等原始 HTML 归档仍保留原来的文件名和行为，不追溯去重；RuleFetcher 使用上述快照存储，不额外生成一份旧式 HTML 归档。

选中的已保留快照可通过 `rulestore.Repository.ImportSnapshot` 提升为长期样本。它验证文件与摘要，并要求新的显式脱敏策略，将正文保存到独立 `rules.sample_dir`；后续 archive 清理不影响该样本。短期快照不会自动变成长期样本或验收凭据，见 [RULE_STORE.md](./RULE_STORE.md)。

## 分页、游标和预算

默认跟随规则输出的 `Result.NextURL`。根级 API 游标设置规则 `next.mode: cursor` 和 `next.pointer`，执行器将 `Result.NextCursor` 自动写入下一任务 State，并继续请求当前 URL。也可配置 `Next(ctx, task, page)` 返回 `*RuleRequest{URL, State}` 覆盖默认策略，nil 表示结束。State 是最多 16 KiB 的非秘密游标，BuildRequest 将它转换为请求体。读取型 POST 必须设置 `ReplaySafe: true`，因为请求成功后 Save 或提交子任务失败也会重跑整个 handler。账号管理器不在同一次调用内自动重放 POST；确认过期后由下一轮任务尝试刷新。

Next 只收到已通过校验、去重和条目预算过滤的数据副本，以及规则提取的 NextURL/NextCursor。该接口不暴露原始响应；请求体仍由明确的 Go 回调构造，不执行动态表达式。不要在 Next 内修改数据以代替字段 transforms。

| 限制 | 默认 / 上限 | 达到后的行为 |
| --- | --- | --- |
| 当前阶段分页链页数 | 100 / 1000 | 保存当前有效数据，不提交下一页 |
| 当前阶段分页链唯一条数 | 10000 / 10000 | 保留预算内新条目，保存后停止；不为被裁掉的条目派生详情 |
| 请求循环、重复完整正文、无新条目 | 启用 | 停止继续翻页；无新数据时不再次 Save |
| 任务元数据总量 | 2 MiB | 拒绝创建/执行超预算任务 |
| 单页子任务数 MaxChildren | 1000 / 10000 | 规划阶段失败，不保存也不提交部分子任务；详情和下一页合计 |
| 单页子任务元数据及 URL 总量 | 8 MiB | 规划阶段失败；避免多链接映射放大内存占用 |
| 任务 URL（含身份 fragment） | 65535 字节 | 对齐现有 MySQL TEXT 容量，提交前拒绝超长地址 |

ItemKey 指定业务身份字段，缺失时失败；省略则以完整记录的规范 JSON 摘要作为身份。分页检查点保存请求、正文和条目摘要，随下一页任务进入原 Meta 列；当前任务的输入不推进，保存或提交失败后仍从相同位置重试。条目去重作用于当前分页链，不代替数据库业务唯一键，也不是整个多阶段任务图的全局预算。

任务 URL 带 `#papa-rule=<摘要>`，用于把固定版本、Catalog 发布身份、账号、业务元数据、请求游标及可选批次纳入既有数据库 URL 唯一索引。真实请求 URL 在保留元数据中；网络不会收到身份 fragment。必须用 `NewTask` 或 `NewTaskContext` 建入口，不要覆盖其 URL/IdempotencyKey、`rule_id`、`rule_version` 或 `_rule_*`。普通 Task 的身份语义和任务表结构不变；可选规则与周期状态的 7 张表须按 `rules.enabled` 显式迁移。

## 保存、重试与恢复

Save 必须按业务唯一键 upsert，支持并发与重复调用。分页过滤后的 `Result.Records` 与 `Validation.Rows` 对应，字段证据索引也会重排；`EvidenceTruncated` 只表示证据数量达到上限，不表示数据校验被省略。业务可以同时保存来源 URL、规则版本、正文摘要和需要的证据。

保存发生在子任务规划成功之后、提交子任务之前。已经保存但某个子任务提交失败时会重放 Save，并再次提交稳定身份的子任务，由队列去重。业务库与任务库没有跨库原子事务，也不承诺 exactly-once。

非法规则/任务绑定、越界地址、提取/校验失败、数据行或校验报告截断、不可重放方法不重试。HTTP 408/429/5xx、网络失败、Save/提交失败保留重试；其它非 2xx 不重试。账号分类优先于通用 HTTP 状态：明确过期可在下一轮恢复，暂停/未知账号/权限不足/人工验证等不盲重试。BuildRequest、Next、Save 的业务错误保留 Retryable 分类，可显式 WrapNoRetry；回调需要自己保证并发安全。

使用内存 Registry 时，重启恢复任务前应重新加载并验证所有尚有任务引用的规则版本。使用持久 Catalog 时，重建 Repository 后按任务固定版本读取 MySQL，不需要重新 Activate，也不重新读取当前发布指针。无 release 元数据的旧任务保持原绑定语义；缺失版本、内容损坏等情况明确失败，不改绑到最新版本。曾发布的旧版本在回滚后仍可供固定任务执行。不要在同一个版本号下修改规则，任务依赖的传输、Next、链接映射等业务代码也应保持兼容；当前固定的是规则版本，不是整套 Go 代码快照。

把旧 Registry fetcher 换成 Catalog 前，先按原 ID/版本将所有待执行任务引用的旧规则导入 MySQL，验收并发布，保证每个旧引用都能 `Resolve`；再发布最新版作为 Current。不会自动迁移 Registry 的内存内容。若旧版本仅在 Registry 中、在 MySQL 缺失或从未发布，Catalog 执行明确返回不可重试错误，不发 HTTP 或调用 Save。

Catalog 新入口和恢复执行会提前检查当前阶段的固定规则与宿主配置：browser 必须配 HTML，ItemKey、Links.Field/Meta 引用的字段必须存在，详情 URL 不能声明为非字符串或最终转换为 number/bool。不兼容返回不可重试错误，不触发页面请求、Save 或提交。内容损坏通过 `crawlrule.ErrInvalidVersion` 归为永久失败；数据库故障与超时仍可重试。此检查使用任务固定版本，不会借此改绑当前发布。

## 可选运行质量反馈

将 `*rulequality.Collector` 传入 `RuleFetcherConfig.Quality`，即可在任务身份校验通过后为 HTTP、JSON 和 Rod 执行记录有界质量摘要；`nil` 保持原有行为。MySQL 持久化先开启 `quality.enabled` 并显式迁移三张质量表，再由宿主构造 Store 和 Collector。这个开关独立于规则仓库与 Agent，也不会自动接线。

摘要分开保存原始提取行数、去重后的新增行数、字段存在/非空/校验问题、分页停止原因和处理失败类别。提取失败的记录仍不会交给 Save；质量写入失败或 panic 只计入旁路失败，不改变业务返回错误、重试和任务终态，不因此再次执行 Save 或子任务提交。质量记录只复用已有 EvidenceID 与正文摘要，不另存一份 HTML。

报告由宿主显式选择固定站点、阶段、规则版本/release、时间窗口和策略生成，仅代表已记录观测；任务成功率不是字段准确率。重试按逻辑页面折叠，零新增、正常分页结束及默认零行不等于规则漂移；截断、观测丢失或样本不足不能声称健康。嵌入式调用没有数据库任务 ID 时默认各次独立；需要折叠同轮重试可用 `rulequality.WithLogicalKey` 提供轮次键，内部只保存其摘要。

任务身份校验与规则可用性检查分开：合法旧任务在内存 Registry 重启后找不到固定版本时，会记录 `rule_contract` 失败并保持原来的不可重试结果。任意损坏或身份不匹配的任务元数据仍在建立观测前拒绝，不从坏数据猜测站点和版本，也不增加 Collector 的 attempted/failed 计数；此类错误结合任务错误和 Trace 排查。Collector 的零失败计数不证明所有任务都已被观测。

报告和显式复分析接线见 [RULE_QUALITY.md](./RULE_QUALITY.md)。只有符合条件的疑似规则漂移报告可创建复分析请求；Agent 产出候选后仍须独立验收与人工发布。

## 当前边界

当前具备确定性的执行、验证、追踪、可选持久发布与运行质量反馈：MySQL 事务保护完整站点发布，generation 检查发布竞争，任务固定版本。宿主可显式调用 [Agent](./SITE_AGENT.md) 生成或修正规则候选，再通过同一套独立验收和人工发布入口。尚未包括审核后台、自动识别 API 分页结构或 HTML 嵌套子列表的声明式映射；JSON 字段可以原样保留嵌套 object/array。质量报告不自动修复、暂停或回滚站点。

入口默认一次性任务。需要周期重采整个任务图时使用下面的 `RuleSchedule`；仅给普通入口设置 Repeatable 仍不会重采已命中去重的详情/后续页。

## 完整周期采集批次

`RuleSchedule` 是独立控制阶段，沿用 `repeat_queue`。每次新批创建一个新的入口身份，列表、详情、多级子任务及分页都继承同一 `BatchID`；同批错误重试和恢复保持身份不变。`RulePage.BatchID` 可和业务记录、规则版本、`EvidenceID` 一起保存，普通一次性任务该字段为空。业务 upsert 的唯一键仍使用业务身份，不能改用 BatchID，否则每批都会插入重复业务记录。

先开启 `rules.enabled` 并显式运行 `papa migrate`，创建 `crawler_rule_schedules`；即使原来已启用规则仓库，升级后也须再迁移一次。使用内存 Registry 的周期流程也需要这张 MySQL 状态表。自动运行还要开启并配置 `repeat_queue`。

重启续跑已入库的 pending/processing 成员还需启用 [recover_queue](./RECOVER_QUEUE.md)，并在恢复前登记控制阶段及全部采集阶段。RuleSchedule 不接管旧成员的 worker 责任：它只重放尚未入库的固定根任务、判断整批进度；已入库但提交回执丢失的成员沿原启动恢复机制处理。关闭恢复时，重启后旧批可能一直等待未被重新投递的成员。失败成员需要原 error_queue 或显式失败重试。

```go
schedule, err := papa.NewRuleSchedule(papa.RuleScheduleConfig{
    Stage: "catalog_schedule", Entry: catalogFetcher,
    Interval: 10 * time.Minute,
})
if err != nil { return err }
// 将 schedule 注册为 catalog_schedule 的 Fetcher；catalogFetcher 和
// detailFetcher 仍分别注册在 catalog、detail，三个阶段名称不能冲突。
task, err := schedule.NewTask(papa.RuleRequest{
    URL: "https://example.com/products",
}, nil)
if err != nil { return err }
return engine.SubmitTask(task)
```

`Interval` 必须是至少 10 秒的整秒时长，只负责首次提交时播种控制任务的周期；之后用原有任务轮询开关/周期 API 调整。只让控制任务 Repeatable，采集子任务保持一次性。不要让有限的 `RepeatLimit` 意外停掉控制阶段；该计数包含控制器等待、恢复和轮询，不能当成成功批次数。

| 场景 | 行为 |
| --- | --- |
| 首次执行 | 创建序号 1；先持久化整批入口快照，再提交入口 |
| 当前批仍有 pending/processing | 等待下一次控制器检查，不创建新批 |
| 当前批有 failed | 阻止新批；通过原错误队列或失败任务重投修复同批 |
| 整批全部 success，后续轮询到来 | 创建下一批；Catalog 读取最新完整发布，Registry 使用构造时的绑定 |
| 提交入口报错、回执丢失、进程重启或重复 SubmitEntries | 恢复已保存的相同入口和规则快照，不重新绑定 Current |
| 状态损坏、根任务缺失但仍有成员、未知任务状态或查询失败 | 明确失败，不将空集合当作已完成，也不静默跳批 |

发布或回滚发生在一批执行期间时，该批仍固定原 release；新批才采纳新 generation。控制器以数据库 `LastRepeatAt` 识别新的轮询，不使用可能因恢复而增长的 `Task.Repeat`。当前轮询写入的时刻精度为秒，同秒的多次强制触发会保守合并，不能将每次点击视为一次新批。到期时上批未完成会等待，周期不是严格日历调度，也不补跑错过的所有时段。

调度状态独立于控制任务 Meta，因此启动时重新提交相同入口不会覆盖正在进行的批次。保持控制阶段、入口 URL、账号和业务 Meta 稳定；修改这些身份会创建另一个控制任务。切换入口时应先停用旧控制任务，避免两套计划并行。Catalog 的当前发布不参与控制任务身份，可以正常发布新版本。

控制器须由真实 Engine worker 调用，不能直接调用 `FetchHandler` 来模拟执行；worker 认领时绑定的 Repeat/Reprocess 版本会与数据库核对，旧执行轮次不获得新批派发权限。查询 `Status` 不要求执行上下文，仍可由宿主直接调用。

`schedule.Status(ctx, engine, taskID)` 返回当前 `Sequence`、`BatchID`、`RootID`、release 和各任务状态计数，以及 `Complete`；未开始时 BatchID 为空。使用有超时的 context。状态只描述已持久化的当前批，控制任务自己成功不代表采集已完成。

当前批成员通过任务 Meta 中受保护的 `_rule_batch` 查询，配合站点和根任务 ID 下界；没有独立批次成员索引。任务历史很大时应评估查询成本并制定归档策略；活动批完成前不能清理其成员。状态载荷上限 2 MiB，不保存正文或运行时凭据。

保证范围为正常单进程引擎流程，不提供分布式执行租约或跨库 exactly-once。Engine 的通用任务操作已保护批次：禁止删除控制任务或成员（含历史批次），禁止给成员开独立轮询或设周期，只允许失败且不可轮询的成员原批重投；状态条件进入 UPDATE，重复请求不会重复投递。若旧版本误开启了成员轮询，仍可先「停轮询」修复。直接修改数据库、改写 Meta 或绕开 Engine 的业务清理不在保护范围内；需要清理历史批次时应另行设计完整生命周期。原失败快照和 Trace 仍按已有策略工作，批次不额外复制 HTML。

后台「周期批次」按站点列出持久控制任务，点击后查看当前批次、固定发布/规则、成员计数和失败成员；操作复用任务 API 的保护与审计。列表每页 20 条、成员每页 50 条，按需读取，不随监控的 3 秒刷新反复聚合。详情是单次一致快照，分页期间新批已启动则返回 409，需刷新第一页。没有批次历史列表，也没有强制越过失败或自动发布。入口与 API 见 [MONITOR.md §11](MONITOR.md#11-周期批次中心)。

控制标记和成员标记不能同时出现；这类异常 Meta 受到保守保护，不允许借控制角色绕开成员约束。成员原批重投在同一 UPDATE 中检查失败状态、不可轮询标记和 reprocess，读后发生竞争也会拒绝过期操作。后台 JSON 的任务 ID 和批次代次使用十进制字符串；Go 状态查询的字段类型不变。

完整真实 Engine/MySQL 两批示例见框架仓库 `examples/ruleperiodic/README.md`；示例只访问本地 HTTP，验证两批列表、详情、分页均执行，业务记录按原 ID 更新，并重开连接读取批次状态。

### 周期状态查询的一致性

`RuleSchedule.Status` 在一个显式 REPEATABLE READ 只读事务中读取控制任务、批次快照、根任务与状态计数。恢复线程在查询期间插入根任务时，本次查询仍返回同一时点的数据，下次查询才看见提交；不会把新旧数据混合误报为孤儿批次。该快照不阻止后续状态变化，不代表采集完成；真实缺失根、损坏快照仍返回错误。本次修复不新增表、不需要迁移。

执行前的周期状态判断也使用同一只读快照函数，避免恢复插根期间误报损坏；快照事务结束后才调用规则目录或提交任务，批次写入仍由原行锁/CAS 检查当前 claim。读取一致性不等于锁定后续执行状态。
