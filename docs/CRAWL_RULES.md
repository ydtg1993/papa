# 离线采集规则与样本验收

`pkg/crawlrule` 把已抓取响应转换为记录，支持 HTML CSS 和 JSON Pointer。规则是带 `schema_version`、规则 ID 与版本号的 JSON，可审查、保存在业务仓库中，并通过显式样本验收后在进程内激活。

本包不发请求、不执行 JavaScript、不登录、不运行模型、不自动生成或发布规则。需要把规则接入获取页面、业务保存、详情与下一页任务时，使用 [RuleFetcher](./RULE_FETCHER.md)。离线执行仍由调用方提供完整响应；认证样本应先脱敏。

## HTML 规则

```json
{
  "schema_version": 1,
  "id": "products",
  "version": "2026-11-06.1",
  "format": "html",
  "rows": "article.product",
  "fields": [
    {
      "name": "id",
      "attribute": "data-id",
      "validation": {"required": true, "min_length": 1, "unique": true}
    },
    {
      "name": "title",
      "selector": "h2",
      "validation": {"required": true, "min_length": 1}
    },
    {"name": "url", "selector": "a.detail", "attribute": "href"}
  ],
  "next": {"selector": "a[rel=next]"},
  "max_rows": 1000,
  "max_body_bytes": 4194304,
  "max_issues": 100
}
```

`rows` 按文档顺序选择记录节点。字段的 `selector` 选择该节点的后代，多个匹配只取第一个；省略选择器则读取当前记录节点。省略 `attribute` 时读取文本，设置后读取指定属性；两者都去掉首尾空白。不存在的节点或属性不会产生该字段；存在但内容为空则产生空字符串。

字段输出为记录的顶层键，`name` 不是输出路径。未声明转换时仍按原字符串输出；需要补全链接或类型转换时，显式配置下文的 `transforms`。HTML 使用容错解析器，能够修复普通标签不闭合；结构漂移依靠必填字段、最小长度和样本断言识别，不要求所有 HTML 都符合严格 XML 语法。

## 确定性转换与字段证据

转换按数组顺序在校验前执行，HTML、JSON、离线样本和在线 RuleFetcher 共用同一条路径。例如：

```json
{"name":"url","selector":"a","attribute":"href","transforms":[{"op":"absolute_url"}],"validation":{"kind":"http-url","required":true}}
```

| op | 输入与输出 |
| --- | --- |
| `trim` | 字符串首尾去空白；JSON 原字符串默认不 trim |
| `absolute_url` | 字符串以最终页面 URL 补全为 HTTP(S) 地址；拒绝 userinfo；是否可访问另由 RuleFetcher Origins 限制 |
| `number` | 严格 JSON 数字字符串或数字，输出精确保留文本的 `json.Number`；不猜千位符/货币，不接受 NaN/Infinity 或超有限 float64 范围 |
| `bool` | 原布尔值或精确的小写字符串 `true` / `false`；不猜 `0/1`、`yes/no` |
| `date` | 必填 Go `layout`，完整年月日；无时区按 UTC，支持数值偏移，输出 UTC RFC3339Nano；拒绝含糊的命名时区和转换后越界年份 |

只有 `date` 可以填写 `layout`。未知转换、过多步骤和非法配置在编译时拒绝。缺失和 null 不执行转换，仍由 required/allow_null 校验；失败可用 `errors.Is(err, crawlrule.ErrTransform)` 识别，错误不含字段实际值。未显式指定 kind 时，带转换的字段由最后一步推导输出类型。

`Result.Evidence` 记录零起始记录索引、字段名、CSS/属性或 JSON Pointer、最终来源 URL、是否存在及已成功执行的步骤，不保存字段值或响应正文。证据最多保存 `max_evidence` 条，默认 10,000、上限 100,000；超出设置 `EvidenceTruncated`，其余字段继续转换与校验，采集记录不会因此截断。它和数据行截断、校验问题截断是三个独立标志。

## JSON 规则

```json
{
  "schema_version": 1,
  "id": "products-api",
  "version": "1",
  "format": "json",
  "rows": "/data/items",
  "fields": [
    {"name": "id", "pointer": "/id", "validation": {"kind": "integer", "required": true, "unique": true}},
    {"name": "title", "pointer": "/name", "validation": {"kind": "string", "required": true, "min_length": 1}},
    {"name": "metadata", "pointer": "/metadata", "validation": {"kind": "object", "allow_null": true}}
  ],
  "next": {"pointer": "/paging/next"}
}
```

`rows` 是从完整文档出发的 JSON Pointer，必须选中数组；空字符串表示根数组。字段 `pointer` 从每个数组元素出发；空字符串提取整个元素，因此也支持标量数组。下一页 `pointer` 从完整文档出发。

API 使用根级游标时设置 `"next":{"mode":"cursor","pointer":"/paging/next_cursor"}`，结果写入 `Result.NextCursor`；默认 mode 为 `url`，写入 NextURL。cursor 仅支持 JSON，必须是最多 16 KiB 的字符串，不解析为 URL、不 trim；缺失、null、空串表示结束，非法类型或超限返回 ErrNextCursor。fixture 要填写 `Expectations.NextCursor`，不匹配或遗漏预期不能激活。在线 RuleFetcher 将游标自动放入下一任务的 State，继续请求当前 URL；Body 由 BuildRequest 显式构造。

JSON Pointer 使用 `/` 分段，`~1` 表示键中的 `/`，`~0` 表示 `~`。例如 `/a~1b/~0key/0` 读取 `{"a/b":{"~key":[123]}}` 中的第一个元素。数组索引必须是规范的非负十进制数，`00`、`-1`、`-` 不会匹配数组元素；对象仍可以有这些字面键。这里不支持 JSONPath、通配符或表达式。

JSON 类型完整保留，数字使用 `json.Number`，大整数不会经过 `float64`。输入必须恰好包含一个合法 JSON 值；不接受多个拼接文档、注释或尾随垃圾。JSON 模式不能混用 CSS 和属性提取，HTML 模式不能设置 JSON Pointer。

## 编译、执行与校验

从文件加载规则时使用 `crawlrule.Parse(data)`：它拒绝未知 JSON 属性、额外 JSON 值和超过 1 MiB 的规则文件。代码中构造规则可调用 `crawlrule.Compile(rule)`。两者均拒绝未知 schema 版本、未知格式、空 ID/版本、重复字段名、无字段规则、非法 CSS/Pointer、非法校验配置及越界预算。当前仅支持 `schema_version: 1` 和小写 `html` / `json`。

```go
compiled, err := crawlrule.Parse(ruleJSON)
if err != nil {
    return err
}
result, err := compiled.Evaluate(response) // response 为 *htmlfetch.Response
if err != nil {
    // result.Validation 可供诊断；失败结果不得作为通过验收的数据发布。
    return err
}
if result.Truncated {
    // 明确决定如何处理截断；不要把部分结果当作完整页面。
}
records, nextURL := result.Records, result.NextURL
_ = records
_ = nextURL
```

响应必须有最终绝对 HTTP(S) URL 和 2xx 状态。`Evaluate` 不根据 Content-Type 自动猜格式，格式由规则明确指定。没有记录时返回 `ErrNoRecords`，不会把登录页或失效选择器静默解释为成功的空列表。某条记录所有配置字段都不存在时同样失败；真正的空列表业务应由调用方明确识别处理。

已经取得渲染后 DOM 时使用 `Compiled.EvaluateRendered(finalURL, body)`，仅支持 HTML，同样执行预算、转换和校验；此入口不知道 HTTP 状态，不伪造 200。浏览器页面是否为登录页或受限页仍需由账号策略和规则必填字段识别。

每个字段通过 `pkg/extract.Validator` 校验，支持 `any`、`string`、`number`、`integer`、`bool`、`object`、`array`、`http-url`，以及必填、允许 null、最小长度、Go 正则、批内唯一约束。HTML 字段默认 `string`，JSON 默认 `any`。`required` 只检查字段存在；需要非空字符串应再写 `min_length: 1`。`unique` 只覆盖本次保留的记录，不跨页查重。

校验不通过时返回 `ErrValidation`，同时返回记录和完整计数的 `result.Validation`，其中 `Issues` 不含实际字段值。调用方可用 `errors.Is` 区分 `ErrNoRecords`、`ErrValidation`、`ErrBodyTooLarge` 和 `ErrNextURL`。除非业务明确处理相应错误，任何非 nil 错误都表示本次结果未通过。

## 下一页和资源预算

下一页是可选项。HTML 读取文档中首个匹配节点，属性默认 `href`；JSON 读取字符串。节点/属性/Pointer 不存在、JSON null 或空白字符串均表示末页。JSON 非字符串下一页返回错误。

相对链接以响应的最终 URL 为基准，忽略 HTML `<base>`。解析后去掉 fragment，规范化主机大小写和默认端口，保留查询参数顺序、值和路径转义。默认仅允许相同 scheme、主机和有效端口；跨域、变更协议或端口返回 `ErrNextURL`。确需跨域时显式设置 `next.allow_external: true`，仍只允许无 userinfo 的 HTTP(S) URL。指回当前页面的链接也返回错误，避免直接自循环。手写同步循环可配合 `discovery.PaginationGuard`；持久化任务分页由 RuleFetcher 的任务检查点处理。

| 配置 | 默认值 | 硬上限 | 超出时 |
| --- | ---: | ---: | --- |
| `max_rows` | 1,000 | 100,000 | 保留前 N 条并设置 `result.Truncated` |
| `max_body_bytes` | 4 MiB | 64 MiB | 整个响应拒绝，返回 `ErrBodyTooLarge`，不解析前缀 |
| `max_issues` | 100 | 10,000 | 只截断问题详情，计数和行校验继续，设置 `Validation.Truncated` |
| `max_evidence` | 10,000 | 100,000 | 只截断字段证据，设置 `EvidenceTruncated`，转换与校验继续 |
| 字段数量 | 必须至少 1 个 | 256 | 编译失败 |

预算为 0 时取默认值，负数或高于硬上限时编译失败。行预算限制输出和字段校验数量；正文仍需要完整解析，不能用它代替正文预算。`Truncated` 和 `Validation.Truncated` 是两个不同信号。

## 样本验收、激活与回滚

`Registry` 是线程安全的进程内版本表，零值可用，也可用 `NewRegistry()`。规则 ID 与版本号唯一；再次注册相同 ID/版本，即使内容完全相同也返回 `ErrVersionConflict`。更新内容必须使用新版本。

```go
registry := crawlrule.NewRegistry()
if err := registry.Register(rule); err != nil {
    return err
}

sampleURL, _ := url.Parse("https://example.test/products?page=1")
fixtures := []crawlrule.Fixture{{
    Name: "normal-first-page",
    Response: &htmlfetch.Response{
        URL: sampleURL,
        StatusCode: 200,
        Body: []byte(`<article class="product" data-id="42"><h2>示例商品</h2><a class="detail" href="/products/42">详情</a></article><a rel="next" href="?page=2">下一页</a>`),
    },
    Expect: crawlrule.Expectations{
        RowCount: 1,
        Fields: []crawlrule.FieldExpectation{
            {Row: 0, Pointer: "/id", Value: "42"},
            {Row: 0, Pointer: "/title", Value: "示例商品"},
        },
        NextURL: "https://example.test/products?page=2",
    },
}}
report, err := registry.Activate(rule.ID, rule.Version, fixtures)
if err != nil {
    // report.Results 给出各样本是否通过及失败类别。
    return err
}
_ = report
active, err := registry.Active(rule.ID)
if err != nil {
    return err
}
result, err := active.Evaluate(nextResponse)
_ = result
_ = err
```

每个样本必须有非空且集合内唯一的名称、正数精确行数和至少一个字段值断言。字段断言中的 `Row` 从 0 开始，`Pointer` 针对提取后的输出记录；缺失字段不等于 null。大整数断言使用 `json.Number("9007199254740993")`，不能先放进已丢精度的 `float64`。数值在受限长度/指数内按精确数学值比较，`1000` 与 `1e3` 相等。

`Expect.NextURL` 总是精确比较，空字符串明确要求没有下一页。默认 `Expect.Truncated: false` 禁止未预期的部分结果；确需测试行预算可显式设置 true。`compiled.Verify(fixtures)` 可只做预验收；`Activate` 仍要求本次提供完整样本集，空集返回 `ErrNoFixtures`。多个样本会全部执行，任一提取、校验或断言失败都返回 `ErrFixtureFailed`，现行版本与回滚历史保持不变。

只有样本全部通过才原子切换现行版本。`registry.Get(id, version)` 可以查看未激活版本，生产读取使用 `Active(id)`。`registry.Rollback(id)` 回到前一次成功激活的版本；重复回滚逐次向前回退，不会切到未通过样本的版本。只有一个成功版本时返回 `ErrNoRollback`。当前版本重复成功激活不增加一条虚假回滚记录；回滚后再激活将建立新的历史末尾。

注册和编译会复制规则，`compiled.Rule()` 返回独立副本。修改输入规则、返回的规则快照或某次提取结果，不会改动已注册版本和后续提取结果。编译规则可并发执行，Registry 的注册、查询、激活、回滚也可并发调用；并发激活按完成验收并获取锁的顺序生效。调用期间不得并发修改传入响应或样本预期。

Registry 不持久化样本、激活状态或历史，也不跨进程协调。进程重启后应重新加载规则并用显式样本激活。通过有限样本不能证明网站所有页面都兼容，建议至少覆盖正常页、末页、不同字段形态，并对运行中的 `ErrNoRecords`、校验错误和截断建立业务监测。

`Registry.Get(id, version)` 允许读取尚未验证的候选；生产固定版本用 `Registry.Verified(id, version)`，候选返回 `ErrUnverifiedRule`。只有成功的 `Activate` 会登记已验证版本；直接调用 `Compiled.Verify` 不发布。回滚 Active 不撤销曾验证版本，已排队任务仍能执行原版本；重启后须重新加载并验证这些版本。

## 验证命令

```powershell
go test ./pkg/crawlrule
$env:CGO_ENABLED = "1"
go test -race ./pkg/crawlrule
```

测试覆盖真实 HTML/JSON 提取、JSON Pointer 转义与数组索引、大整数、空结果与非法输入、同源下一页策略、预算、字段校验、样本失败不激活、版本冲突、输入/输出隔离、回滚和并发调用；不依赖网络、浏览器、数据库或真实账号。
