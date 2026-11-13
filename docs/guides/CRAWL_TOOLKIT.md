# 采集工具：网络取证、内容发现、分页、增量和质量校验

> 面向：已经有 Fetcher，需要补齐采集完整性、调试证据和数据质量。配套：[Fetcher 手册](FETCHER_WRITING_GUIDE.md)。这里的工具不接入模型、不代替业务解析、不自动向生产网站发起探索。

## 1. 一次流程的边界

| 步骤 | 入口 | 调用方负责 |
| --- | --- | --- |
| 网站取证 | `Engine.RecordNetwork` / `Browser.RecordNetwork` | 合理时间/正文预算，账号隔离与交互 |
| 脱敏样本 | `NetworkEntry.WriteSample` | 白名单、正文脱敏与文件发布 |
| 链接发现 | `discovery.ExtractLinks` | 选择器、采集范围和访问策略 |
| Sitemap/RSS/Atom | `discovery.ParseXML` | 下载、gzip 解压、索引递归深度与同源过滤 |
| robots.txt | `discovery.ParseRobots` | 请求/状态码策略、缓存与 CrawlDelay 调度 |
| 分页停止 | `discovery.PaginationGuard.Observe` | 业务稳定 ID、下一页判定和持久化 |
| 增量抓取 | `Engine.FetchIncremental` | 按账号/表示保存校验标记，成功落库后更新 |
| 数据校验 | `extract.Validator.Validate` | 字段规则、质量阈值、异常处理 |

这些工具只返回事实和结果，任务投递、结果存储及业务解析由调用方负责。`FetchHTML` / `Do` 将非 2xx 作为错误；增量入口单独处理带有效校验标记的 304。

## 2. 网络录制

```go
record, err := engine.RecordNetwork(ctx, task.URL, browser.RecordOptions{
    Timeout: 15 * time.Second,
    ObserveFor: time.Second,
    MaxEntries: 64,
    MaxBodySize: 1 << 20,
    MaxTotalBodySize: 4 << 20,
    Page: browser.PageOptions{WaitSelector: ".list"},
})
if err != nil { return err }
for _, entry := range record.Entries {
    // 检查 Request.URL、Request.Method、StatusCode、Completed 和 Failure。
    // 完整条目可调用 entry.WriteSample(writer, policy) 显式脱敏导出。
    _ = entry
}
```

默认总时限 30 秒，导航/等待/可选 `Page.Interact` 后额外观察 1 秒；默认最多 128 条交换，每条响应 1 MiB、每条请求体 64 KiB、请求和响应体合计 8 MiB。非正配置值使用默认值。`Page.Timeout` 限制导航与加载等待，整体仍受总时限约束。交互回调必须配合 ctx，不能无限阻塞 Go 代码。

监听先于导航建立。重定向的各跳单独记录；超过条目上限计入 `Dropped`。超限正文被省略并标记 `BodyOmitted`，请求体超限保存前缀并标记 `RequestBodyIncomplete`。观察结束时仍在途的请求保持 `Completed=false`。加载失败或正文不可获取时 `Failure` 给出类别，不能把这些条目当作完整样本。

`NetworkEntry.WriteSample` 拒绝重定向、未完成、有失败或缺少正文的条目。它复用显式白名单/正文脱敏策略，不直接落盘，也不会重新发送请求。若只需元数据，可直接消费条目的字段，不假装已保存完整交换。

这是**单页面 target 的有界录制**，不是全站无限 HAR：不覆盖所有新标签页、worker、WebSocket 帧或完整请求 ExtraInfo。CDP 可能先返回整段正文再检查长度，因此预算限制保留结果，不是 Chrome 内存硬限制。原始头和正文只在内存里返回，自动 trace 只保留条目数/丢弃数。

需要登录态的低层录制，应在 `Pool.WithIsolatedBrowser(ctx, func(b *browser.Browser) error { ... })` 的同一隔离浏览器内先完成业务登录或状态恢复，再调用 `b.RecordNetwork`；录制新页面共享该浏览器上下文的 Cookie，LocalStorage 等状态仍需业务正确恢复。这里的接收者是 `*browser.Browser`，`pkg/account.Manager` 没有 `RecordNetwork` 方法。`Engine.RecordNetwork` 当前使用公共浏览器池，不自动选择任务账号、恢复 Manager 凭据或固定账号身份；账号采集应优先使用已接入账号运行时的 Render / Capture 入口，不能把公共录制结果视为该账号的证据。

## 3. 链接、订阅源与访问范围

```go
links, err := discovery.ExtractLinks(page, discovery.LinkOptions{
    Selector: ".list a.detail[href]",
    MaxLinks: 500,
})
if err != nil { return err }
if links.Truncated { return fmt.Errorf("列表链接超过预算，需要检查分页规则") }
```

默认选 `a[href]`，最多 10000 个不重复 URL。支持页面第一个 `<base href>`，但同源判断始终锚定原页面 URL，外站 base 不会扩大默认范围。忽略空链接、纯锚点、非 HTTP(S)、带 userinfo 和无法解析的 URL；非法 CSS 选择器报错。

`CanonicalURL(base, ref)` 去 fragment、规范 host 大小写和默认端口；不改查询参数顺序、值及路径转义，以免破坏签名。`SameOrigin` 严格比较协议、主机和有效端口，不把 www、子域或不同端口当作同源。它不同于现有 `core.SameHost` 的业务站点判断，不改变旧函数语义。

`AllowExternal=true` 可以允许外站链接，`Allow(url)` 可额外接业务策略。链接过滤不是网络出口隔离：后续 HTTP 重定向、浏览器子资源仍需独立控制。

`ParseXML(data, baseURL, maxTargets)` 支持 Sitemap urlset/index、RSS 2 的 item/link、Atom entry 的 alternate link；输入最多 8 MiB，默认 10000 条目标。索引条目标记 `Sitemap=true`，不自动递归下载；达到数量上限标记 `Truncated`。不支持 RSS 1/RDF、xml:base 继承或自动 gzip 解压；相对链接按传入 baseURL 解析。XML 不加载外部实体。返回结果仍须做采集范围过滤。

robots 使用 `github.com/temoto/robotstxt v1.1.2` 的规则解析。`ParseRobots(origin, agent, body)` 要求非空 agent、正文不超过 1 MiB；`Allowed(url)` 对非同源 URL 返回 false，`CrawlDelay()` 只返回间隔，不自动 sleep。只对成功取得的 robots 正文调用它：404/403/5xx/超时如何处理、缓存多久由业务明确决定，不能把下载失败当空 robots。规则包上游：[官方仓库](https://github.com/temoto/robotstxt)。

## 4. 分页停止条件

```go
guard, err := discovery.NewPaginationGuard(50, 10000)
if err != nil { return err }
// 每次页面解析成功后调用，itemKeys 为业务稳定 ID：
decision, err := guard.Observe(finalURL, rawBody, itemKeys, nextURL != "")
if err != nil { return err }
if !decision.Continue {
    // Reason: url-cycle / repeated-content / no-new-items /
    // no-next-page / page-limit / item-limit
}
```

一个 Guard 对应一次进程内顺序遍历，停止后保持停止，不持久化进度。Continue 表示允许抓下一页，不代表本页数据质量合格。调用前必须已确认页面抓取和业务解析成功。

`body=nil` 关闭正文指纹检查，非 nil 的空切片是真实空内容。`itemKeys=nil` 关闭新增条目检查，非 nil 的空切片表示明确没有条目，会触发无新增停止。正文哈希按原字节比较，动态时间戳可能使它失效，应优先配合稳定 ID。ID 数量和页数上限必须为正，返回 NewItems 是预算内首次发现的 ID 数。

## 5. 增量采集

```go
result, err := engine.FetchIncremental(ctx, url, previous)
if err != nil { return err }
if result.NotModified {
    // 304 没有正文；复用已保存的业务结果，不把空 body 当新数据。
    return nil
}
if result.Changed {
    // 解析、质量验证、成功写入业务数据。
}
// 数据确认成功之后，再保存 result.Representation。
```

`Representation` 包含精确请求 URL、ETag、Last-Modified 和 SHA-256；缓存必须按账号、请求头、语言及表示类型隔离，不仅按 URL。工具不会持久化结果或替业务处理 Vary，也不应把敏感账号的记录共享给另一账号。

同 URL 优先发送 ETag，否则 Last-Modified；未匹配 URL 不发送旧标记。200 计算原始正文 SHA-256，服务器不支持条件请求时也能判断正文是否变化；304 只在确实发送匹配标记时接受，保留原内容指纹。调用方必须确实保存了对应业务结果，不能只有标记而丢失数据。

这条入口不跟随重定向，不转发标记到另一资源；3xx/206/204 等返回错误，先由业务解析迁移地址再建立新记录。它清除继承的 If-None-Match/If-Modified-Since/Range，以本次 representation 为准。其他非 2xx 仍通过既有错误类型返回。数据库写入失败不能先更新指纹，否则下一次 304 可能掩盖未落库的数据。

## 6. 数据质量验证

```go
validator, err := extract.NewValidator([]extract.FieldRule{
    {Pointer: "/id", Kind: "integer", Required: true, Unique: true},
    {Pointer: "/title", Kind: "string", Required: true, MinLength: 1},
    {Pointer: "/url", Kind: "http-url", Required: true},
})
if err != nil { return err }
report := validator.Validate(records, 100)
if report.InvalidRows > 0 {
    return fmt.Errorf("数据校验失败：%d/%d 行，%d 项问题", report.InvalidRows, report.Rows, report.TotalIssues)
}
```

records 为 `[]map[string]any`，使用标准 JSON 值类型，推荐 `htmlfetch.DecodeJSON` 保留 json.Number 精度。Kind 支持 any/string/number/integer/bool/object/array/http-url。字段路径使用 JSON Pointer，支持嵌套对象、数组索引、`~0`/`~1` 转义；空 Pointer 表示整条记录。Go struct/int 等非标准 JSON 值须先转换后再验证。

Required 检查字段是否存在；null 默认拒绝，AllowNull 显式放行；空字符串要用 MinLength 限制。长度按 Unicode 字符数或数组元素数计算。Pattern 使用 Go regexp，若要整串匹配需自己加 `^...$`。

Unique 在当前批次内判重，缺失和允许的 null 不参与；数值字段将 1e3 和 1000.0 视作同一数。number/integer 拒绝 NaN/Inf，json.Number 最多 512 字符、指数绝对值最多 4096，防止异常数字消耗过多资源。批次外唯一性仍由业务数据库约束保证。

Report 给出 Rows、InvalidRows、TotalIssues 和限量 Issues（默认 100）；细节达到上限置 Truncated，仍统计全部问题。Issue 只含行号、Pointer 和错误类别，不包含字段值。它不修改记录、不补造数据、不自动判定某个缺失字段可以忽略。

## 7. 验证与未覆盖范围

离线样本→同一解析器→Validator 可以组成不访问网站的回归链路。真实接口/浏览器测试在本地 httptest 上验证，并不能证明生产站点的选择器、登录流程和字段业务语义正确。

离线 JSON 重放应解码到新的结果变量。标准 `encoding/json` 复用非空 map 时，会保留本次 JSON 中没有出现的旧键；这会让已经脱敏删除的字段在旧内存结果中残留。`TestIncrementalValidatedSamplePipeline` 演示从 HTTP、校验、脱敏样本到离线重放与条件 304 的完整链路。

账号统一运行与恢复见 [ACCOUNT_RUNTIME.md](../accounts/ACCOUNT_RUNTIME.md)。这些工具不自动识别网站业务语义或迁移业务数据库。实际网络访问由配置的网络出口策略约束，链接提取过滤不能替代出口检查。PDF/OCR/额外 JS 引擎属于有目标站点需求时选配的组件。
