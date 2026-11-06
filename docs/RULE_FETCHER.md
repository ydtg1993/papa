# 规则驱动的采集流程

> 面向：已经有 HTML/JSON 规则，希望复用框架队列运行列表、详情和分页的业务。配套：[规则与样本](./CRAWL_RULES.md)、[账号运行](./ACCOUNT_RUNTIME.md)、[数据落地](./DATA_LANDING_GUIDE.md)。

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

## 获取页面与登录状态

| 配置 | 行为 |
| --- | --- |
| `Transport: "http"`（默认） | 通过 Engine.FetchHTTP 复用 HTMLClient；规则决定 HTML 或 JSON |
| `Transport: "browser"` | 隔离 Rod 上下文取得 DOM，仅支持 HTML；`WaitSelector` 可等待目标元素 |
| `AccountID` 非空 | NewTask 固定账号标识；HTTP/Rod 分别走 FetchAccountHTTP / WithAccountBrowser，凭据由已配置的 Manager 管理 |
| `BuildRequest` | 根据固定 RuleRequest 构造 Headers/Body；不得改变 URL，只支持 GET、HEAD、显式可重放 POST |

`Origins` 必填，按 scheme/主机/有效端口限制请求及结果，不是 URL 路径前缀。详情链接和下一页都要通过范围校验。HTTP 一律禁止跟随重定向（包括同源），3xx 明确失败；请配置最终地址。浏览器沿用页面状态工具的 origin 检查，不能充当阻断所有跨源子资源/重定向的网络沙箱。账号 Rod 只导航到账号 bootstrap 所在 origin，回调不替换账号管理器的认证请求头。

HTTP 客户端和浏览器池仍需按原配置启用；规则执行器不自动启动它们。账号信息由任务固定，不会因运行时修改默认 AccountID 而切换已有任务。任务 Meta/规则/cursor 只能存非凭据数据；Cookie、Authorization、CSRF 放入账号管理器或运行时 BuildRequest。账号入口和本执行器不自动归档响应正文。

`RulePage` 带最终 `SourceURL`、规则版本、`BodySHA256`、字段证据及分页序号。HTTP 带实际 StatusCode，Rod 为 `Rendered=true / StatusCode=0`，不把渲染成功冒充 HTTP 200。

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

任务 URL 带 `#papa-rule=<摘要>`，用于把固定版本、账号、业务元数据和请求游标纳入既有数据库 URL 唯一索引。真实请求 URL 在保留元数据中；网络不会收到身份 fragment。必须用 NewTask 建入口，不要覆盖其 URL/IdempotencyKey、`rule_id`、`rule_version` 或 `_rule_*`。普通 Task 的身份语义和数据库结构不变，无需新增迁移。

## 保存、重试与恢复

Save 必须按业务唯一键 upsert，支持并发与重复调用。分页过滤后的 `Result.Records` 与 `Validation.Rows` 对应，字段证据索引也会重排；`EvidenceTruncated` 只表示证据数量达到上限，不表示数据校验被省略。业务可以同时保存来源 URL、规则版本、正文摘要和需要的证据。

保存发生在子任务规划成功之后、提交子任务之前。已经保存但某个子任务提交失败时会重放 Save，并再次提交稳定身份的子任务，由队列去重。业务库与任务库没有跨库原子事务，也不承诺 exactly-once。

非法规则/任务绑定、越界地址、提取/校验失败、数据行或校验报告截断、不可重放方法不重试。HTTP 408/429/5xx、网络失败、Save/提交失败保留重试；其它非 2xx 不重试。账号分类优先于通用 HTTP 状态：明确过期可在下一轮恢复，暂停/未知账号/权限不足/人工验证等不盲重试。BuildRequest、Next、Save 的业务错误保留 Retryable 分类，可显式 WrapNoRetry；回调需要自己保证并发安全。

重启恢复任务前，应重新加载并验证所有尚有任务引用的规则版本；Registry 当前仅在内存中。已回滚的旧版本仍可供固定版本任务执行。不要在同一个版本号下修改规则，任务依赖的传输、Next、链接映射等业务代码也应保持兼容；当前固定的是规则版本，不是整套 Go 代码快照。

## 当前边界

当前具备确定性的执行、验证和追踪基础。尚未包括规则发布持久化/审核后台、分布式规则一致性、自动识别 API 分页结构、HTML 嵌套子列表的声明式映射或模型自动生成/修正规则。JSON 字段可以原样保留嵌套 object/array。后续 Agent 应先产出规则与样本，再通过同一套验证和执行入口。

入口默认一次性任务。当前没有自动的全流程周期批次机制；仅设置入口 Repeatable 不会重采已经成功且命中去重的详情/后续页。周期重采需要后续补充独立批次及子任务身份传播，不能把错误重试或恢复能力当成完整的周期采集。
