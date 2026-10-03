# Papa 监控服务后台手册

> 面向：看懂并用好 OA 监控后台——Dashboard、设置、表格页、动态配置、队列手动触发、日志导出。
> 配置项见 [CORE_CONFIG.md](./CORE_CONFIG.md) 的 `server` 段。

---

## 0. 一句话

框架内置一个 HTTP 监控后台（`server.enabled` + `server.monitor`），访问 `http://localhost:<port>/monitor`，提供 Dashboard、任务队列概览、队列治理、表格页、设置、动态配置与日志导出。

## 1. 鉴权（访问令牌 + 白名单）

所有 `/api/` 接口都过两道校验：

1. **IP/CIDR 白名单**（`server.whitelist` / `whitelist_file`），空 = 不限制；`/monitor` 页面本身只查这一道。
2. **访问令牌**：请求带 `Authorization: Bearer <令牌>`，或 `X-Auth-Key: <令牌>`。
   **不再支持 `?key=`**（凭据进 URL 会落进浏览器历史与访问日志）。

`/api/*` 的响应一律带 `Cache-Control: no-store`：带凭据的数据（仪表盘、日志下载、白名单……）
不该被中间代理缓存 —— 否则令牌停用/删除之后，那份旧的 200 还能被重放出来。

### 令牌存在库里，一条属于一个操作人

凭据不再是配置里的单个 `auth_key`，而是 **`crawler_access_token`** 表里的多条令牌：

| 字段 | 说明 |
| --- | --- |
| `operator` | 令牌归属的人 —— 操作日志据此记「谁干的」 |
| `token_hash` | `sha256(令牌)`，唯一索引；**库里不存明文** |
| `enabled` | 停用而不删 |
| `note` | 备注（哪台机器/哪个人） |

**创建令牌**两条路，明文都只出现一次，务必当场存好：

- 后台 **General →「访问令牌」页**（在「设置」上方）的**「新增令牌」**：填操作人（必填）与备注，
  服务端生成后弹窗显示明文，带「复制」按钮。
- 命令行 **`papa token add --operator 张三 --note "运维机"`**。

**管理令牌**：同一页可以看列表、**停用/启用**、**删除**（停用启用带状态条件，重复点击只有一次生效，
第二次会提示"该令牌状态已变，请刷新后重试"）。页面**不显示令牌本身**——库里只有哈希。

> **删不掉最后一条**：删到一条不剩会让后台只剩 IP 白名单（`auth` 把"表里没有令牌"当成
> "还没配凭据"→ 放行）。所以最后一条会被拒绝并提示「请先新增一条，或改用『停用』」——
> 停用是拒绝访问，删除却会敞开，这个区别是刻意的。要彻底重置成免鉴权只能直接删库里的行。

> 为什么 CLI 还留着：令牌**全部被停用**时 `/api/*` 一律拒绝，后台自己也就进不去了，
> 只能用 `papa token add` 从机器上补一把救回来。

**几条要记住的行为**：

- 令牌表**一条都没有**时：`/api/*` 对白名单内的来源**完全开放**（等同"还没配凭据"），启动时会打一条醒目警告。
- 令牌**都被停用**时：`/api/*` 对所有人**拒绝**（fail closed）—— 不会因为"停用最后一条"把后台悄悄敞开。
- **停用了自己正在用的那把，当前会话立刻失效**（下一个请求就 401、弹回登录页）—— 这符合"停用立即生效"的语义，
  不是 bug；要给对方换令牌时先建新的、让对方登录后再停旧的。
- 查询数据库出错时同样拒绝并记日志。
- 登录界面没有变化：还是那个遮罩，只是服务端从"比字符串"换成了"查令牌表"。

令牌页的写操作（新增/停用/启用/删除）和表格页的动作一样**记进操作日志**（开启 `server.operation_log` 时），
所以「谁给谁建了令牌、谁停用了谁」都查得到。审计里记的是操作人与备注，**不含明文令牌**。

> 写入是**异步**的（后台队列，不占业务响应）：写库失败、或队列堵了（写库跟不上）时，
> 那条记录会**落到日志文件**里（一行 JSON，可捞回来补录），既不丢审计也不反压业务。
> 关停时会先把队列排空再关数据库。

> 升级时表要自己建 —— 跑一次迁移（脚手架项目里是 `make migrate`，独立用 CLI 是 `papa migrate`，见 [CORE_CONFIG.md](./CORE_CONFIG.md) 第 2 节）。
> 之后用 `papa token add` 给每个人建一把 —— 原来共用的 `auth_key` 请停用/删掉对应令牌。

## 2. 设置 API

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/settings` | 返回白名单、白名单文件路径与是否存在、日志目录 |
| POST | `/api/settings/whitelist` | 更新白名单并持久化到 `whitelist_file`；body `{"whitelist": ["127.0.0.1","10.0.0.0/8"]}` |
| POST | `/api/settings/shutdown` | 触发优雅退出。**body 必须带 `{"token":"..."}`**（要求操作人重新输一遍自己的访问令牌），校验不过返回 403 且不关停；宿主没注入校验器时该接口返回 404 |

### 访问令牌 API（「访问令牌」页用）

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/tokens` | 列表：`{"tokens":[{"id","operator","enabled","note","created_at","updated_at"}]}` —— **不含哈希与明文** |
| POST | `/api/tokens` | 新增：body `{"operator":"张三","note":"运维机"}` → `{"id":4,"token":"<明文，只此一次>"}`；操作人为空返回 400 |
| POST | `/api/tokens/enabled` | 停用/启用：body `{"id":4,"enabled":false}`；状态已被别人改过返回 409 |
| POST | `/api/tokens/remove` | 删除：body `{"id":4}`；不存在返回 404 |

这几条同样过白名单 + 令牌校验；出错回 `{"error":"..."}`，前端把这句话原样显示给用户。

## 3. 表格页（oao 组件）

后台的表格页由独立组件 [github.com/ydtg1993/oao](https://github.com/ydtg1993/oao) 渲染。
**组件是纯展示层，不碰数据层**：业务声明"显示什么、怎么显示"，并实现 `oao.Source` 提供数据。

> **例外**：「访问令牌」页**不是**表格页。它要「新增」并把服务端生成的明文令牌交给操作人看一次，
> 而表格组件的动作只回 `{"status":"ok"}`、不回数据（组件不给"动作返回数据"这个口子）。
> 所以这一页是后台自带的原生模块：`/api/tokens` 三条接口 + `mo.js` 里自己渲染的表格，接口在 `internal/tokenadmin`。

> **内置表格页的菜单位置**：框架自带的表格页（目前只有开启 `server.operation_log` 后的「操作日志」）
> 菜单固定在侧边栏 General 分组（「访问令牌」上方），**不跟业务表格挤在同一个分组**——
> 映射写在 `mo.js` 的 `BUILTIN_TABLES`（表 key → 按钮 id）。表没注册时按钮自动隐藏，
> 不会留一个点了报错的死菜单项。

```go
app, err := papa.New(papa.WithModels(&models.Episode{}))

// 在 New 之后、Run 之前注册（这样能用 app.DB 构造 Source）
app.UseTables(oao.Table{
    Key: "episode", Label: "剧集", Group: "业务",
    Source:  myEpisodeSource,          // 业务实现，负责查库/调接口
    Columns: []oao.Column{
        {Field: "id", Kind: oao.KindNumber, Width: "70px"},
        {Field: "title", Label: "标题"},
        {Field: "downloaded", Label: "已下载", Kind: oao.KindBool},
        {Field: "cover", Label: "封面", Render: oao.RenderImage, Size: 56},
        {Field: "remark", Label: "备注", Render: oao.RenderInput, MaxLen: 30},
    },
    Filters: []oao.Filter{
        {Field: "title", Label: "标题", Op: oao.OpLike},
        {Field: "downloaded", Label: "已下载", Kind: oao.KindBool, Op: oao.OpIn},
        {Field: "created_at", Label: "创建时间", Kind: oao.KindTime, Op: oao.OpBetween},
    },
    DefaultSort: "-id",
})
```

**操作列**（可选）：声明 `Actions` 才有，不声明就是只读表、不注册任何写路由。动作请求转发给业务的 `Handler`，
业务用 `oao.Fail` 返回带状态码的提示语（其他错误统一 500，细节不回前端）：

```go
Actions: []oao.Action{
    {Key: "retry", Label: "重投", Tone: oao.ToneInfo,
     Confirm: "重置该任务并重新投递？",          // 非空 → 先弹确认框
     Handler: func(ctx context.Context, req oao.ActionRequest) error {
         id, err := strconv.ParseUint(req.ID, 10, 64)   // 主键是字符串，取 Table.IDField 指定的字段
         if err != nil {
             return oao.Fail(http.StatusBadRequest, "无效的 ID")
         }
         // 防重复提交：把行快照里的版本值写进 UPDATE 的条件，并检查影响行数。
         // req.Row 是客户端回传的、不可信的展示快照，只适合当"版本号"用。
         if err := engine.RetryTask(uint(id), wasReprocess(req)); err != nil {
             return oao.Fail(http.StatusConflict, "%v", err)
         }
         return nil
     }},
    {Key: "fail", Label: "标失败", Tone: oao.ToneWarn, Confirm: "确认？",
     Form: []oao.Field{{Name: "reason", Label: "失败原因", Widget: oao.WidgetTextarea, Required: true}}},
    oao.EditAction(...),    // 表单字段由列声明推导（跳过图片/JSON/链接与 NoEdit 列）
    oao.RemoveAction(...),  // 自带二次确认
}
```

两条容易踩的约定：

- **主键列必须声明**：前端靠 `Table.IDField`（默认 `id`）指定的字段定位目标行，而列表下发时
  只保留**声明过的列**——没声明这一列，点按钮只会弹「这一行没有 id 字段」。不想显示就用
  `Hidden: true`（仍会下发）。`oao.New` 在注册时就会校验，声明不一致直接报错。
- **防并发/重复提交是业务的事**：组件只透传，不做去重（见 oao README 的「组件的职责边界」）。
  正确做法是把前置条件写进**自己的写语句**并检查影响行数，别用"先查再写"（TOCTOU 等于没锁）。

动作声明得多了也不会撑行：**超过 3 个时前两个平铺、其余自动收进「更多 ▾」**（面板支持键盘与点外部关闭）。

内置「任务」表（`internal/tasksource`）是动作如何接线的参考实现，它带了五个动作（见下表）——
写操作都走 `crawler.Engine` 的条件更新，
`url` 列用 `NewTab: true` 在新标签页打开。成功与失败都会调 `Config.OnAction`，papa 用它写操作日志
（见 [CORE_CONFIG.md](./CORE_CONFIG.md) 的 `server.operation_log`，不开就不记）。

数据源只需实现一个方法：

```go
type Source interface {
    List(ctx context.Context, q oao.Query) (rows []map[string]any, total int64, err error)
}
```

`Query` 是组件把 HTTP 参数规范化后的结果（`Page/Size/Search/Sort/Filter`），
筛选算子（`In` 用 `"a,b,c"`、`Between` 用 `"a..b"`）由业务自己解释 —— 组件只透传。
papa 的 `internal/gormsource` 把算子落成 SQL：`OpLike` → `LIKE '%值%'`（前后通配，用不上索引），
`OpPrefix` → `LIKE '值%'`（只有后通配，能走索引），`OpEq`/`OpIn`/`OpBetween`/`OpGt`/`OpLt` 各对应同名条件。
参考实现看 `internal/tasksource`（内置「任务」表）或 hg2 的 `tablesource`。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `{prefix}/tables` | 列出已注册的表格及列/筛选/操作元数据（侧边栏菜单用） |
| GET | `{prefix}/{table}` | 分页数据；query：`page`、`size`（默认取表声明，上限 200）、`search`、`sort`（`col`/`-col`）、`filter[col]=val` |
| POST | `{prefix}/{table}/action/{key}` | 转发操作给业务 Handler（表未声明该动作一律 404） |

`prefix` 默认 `/api/oao`；静态资源挂在 `/static/oao/`。两者都必须过监控后台的白名单 + 密钥校验（组件复用宿主注入的中间件）。

- **列怎么显示**：`Render` 可选 `text`（省略号 + 悬停看全文）/ `input`（只读输入框，
  长度可控、横向滚动看全，**不是编辑**）/ `link` / `image` / `enum`（彩色标签）/ `time` / `json` / `custom`。
  留空按 `Kind` 推断。
- **哪些能筛**：只声明需要的字段，未声明的字段不可筛；列名与排序同样走白名单，杜绝注入。
- **菜单自动出现**：按 `Group` 分组渲染到侧边栏，注册即出现，前端无需改代码。
- **刷新策略**：表格页只在用户操作（筛选/排序/翻页）时刷新，不参与 Dashboard 的 3 秒轮询。

内置的「任务」表（`internal/tasksource`）展示的就是 `crawler_task`，可作为完整示例。
它带四个写操作加一个只读入口（组件超过 3 个动作时只平铺前两个，其余收进「更多 ▾」）：

| 动作 | 位置 | 说明 |
| --- | --- | --- |
| **重投** | 平铺 | 重置为待处理并重新投递（队列满时与正常投递一致溢出到 DB 回灌），带 `reprocess` 版本条件防重复点击 |
| **追踪** | 平铺 | 打开该任务的**步骤时间线**：按「第 N 次尝试」分段，每段列出步骤名、耗时、失败步的错误分类与信息、以及该步采集到的数据（可折叠） |
| **加急** | 更多 ▾ | 把该任务投一份到所属阶段的**快车道**，插到排队任务前面执行。见下方说明 |
| **标失败** | 更多 ▾ | 对非终态的行生效，必填原因写进任务的错误信息 |
| **删除** | 更多 ▾ | 二次确认；「处理中」的行拒绝删除，请改用标失败 |

写操作都是条件更新，业务错误经 `oao.Fail` 带状态码返回，成败都记入操作日志。

> **防重复提交有三层**：① 前端 —— oao 组件自己拦：同一「表 + 动作 + 行主键」在上一次请求
> 结束前再次触发会被 `runAction` 直接忽略，请求根本不发出去（oao v1.2.4 起；见其 README
> 「防重复提交」）；② 服务端 —— 每个动作的 `WHERE` 里都有版本守卫，重复请求影响 0 行、
> 返回 409，**这一层才是最终防线**（防的不只是手抖，还有两个人同时点和重放）；
> ③ 数据 —— 动作要改的状态本身写在小表里（如 `urgent`），看得见也查得到。
>
> 注意「加急」收在「更多 ▾」里，而那个面板是 `closeOnPick`（点完就收），所以它其实很难被双击命中；
> 防连点主要护的是平铺的「重投」。

**关于「加急」**：它只省**排队**时间 —— 该阶段 worker 全在忙的时候插队也快不了，
所以别指望它能把一条任务从 30 秒的浏览器抓取里"抢"出来。
实现上是给该阶段的池子加了一条快车道队列，worker 取任务时优先看它（`internal/workerpool`）。
几个边界：任务已在执行中 → 409「已经加急过了，或已被取走」；任务已成功/失败 → 409「已结束，
要重跑请用重投」；重复点两次 → 第二次拿到 409（`urgent` 列本身就是版本守卫）。
worker 认领时 `urgent` 自动归零，是**一次性**的，不会让失败重投的加急任务越积越多。
归零之后表上就看不出它加急过 —— 所以引擎同时在**步骤追踪的第一步**记一条「加急执行」
（只记第一次尝试），点「追踪」就能看出这条曾经加急跑过。这条依赖 `crawler.trace.enabled` 打开，
没开追踪时就只剩认领前后的 `urgent` 列可看。
任务表的「加急」列（是/否）显示当前状态。业务侧想在提交时就加急，用 `papa.Task{..., Urgent: true}`
（见 [FETCHER_WRITING_GUIDE.md](./FETCHER_WRITING_GUIDE.md) 1.2）。

**关于「追踪」**：这个动作在服务端**不做事**，只回一句成功 —— 因为表格组件的动作成功时只能回
`{"status":"ok"}`、带不回数据，而组件也没有对外的事件钩子。真正的展示由
`/static/trace.js` 接管：它包了一层 `window.fetch` 嗅探这次动作请求，从请求体里取出任务 id，
再自己拉 `GET /api/task/trace?id=<id>`。所以点「追踪」时那句「追踪成功」的 Toast 是组件的
固有行为，不是多余提示。

步骤追踪需要在配置里显式开启（`crawler.trace.enabled`，见 [CORE_CONFIG.md](./CORE_CONFIG.md)），
handler 侧的用法见 [FETCHER_WRITING_GUIDE.md](./FETCHER_WRITING_GUIDE.md) 1.3。
**未开启时点「追踪」不会白屏**，抽屉里会直接显示「步骤追踪未开启」。
要先跑一次迁移把 `crawler_task_trace` 建出来（脚手架项目里是 `make migrate`，CLI 是 `papa migrate`）；开关开着但表不存在时，
启动会打一条醒目的错误日志，不会静默丢数据。

## 4. 自定义页与注入（逃生舱）

声明式的表格页覆盖不到的场景（自定义看板、审核页、特殊展示），可以往后台加**自定义页**，
或直接注入受信任的 JS/CSS。四个 API 都在 `Run` 之前调用：

```go
// 一页 = 侧边栏一个菜单项 + 一段渲染脚本
app.UsePage(papa.Page{
    Key: "review", Label: "审核", Group: "业务",   // Key 只允许字母数字下划线连字符
    Script: `
        Papa.page("review", function (el, meta) {
            // el 是页面容器；meta 是清单项 {key,label,group}
            el.innerHTML = '<h2>' + esc(meta.label) + '</h2>';
            apiFetch('/api/my/review-list').then(r => r.json()).then(function (d) {
                el.innerHTML += '<p>共 ' + d.total + ' 条待审</p>';
            });
        });`,
})

app.UseScript(`window.__myFlag = true;`)      // 全局注入一段 JS
app.UseScriptFile("assets/admin.js")          // 读文件注入（读不到直接 panic）
app.UseCSS(`.my-badge { border-color: var(--primary); }`)
app.UseCSSFile("assets/admin.css")
```

前端契约：页脚本在加载后调 `Papa.page(key, fn)` 注册渲染函数，点菜单时框架调用 `fn(el, meta)`。
页面脚本里可以直接用后台的全局件：`Toast` / `Dialog` / `skeletonRows` / `esc` / `tipAttr` /
`apiFetch` / `apiPost`（都带密钥）。没注册渲染函数的页会显示一句明确提示，不会白屏。

| 方法 | 路径 | 鉴权 | 说明 |
| --- | --- | --- | --- |
| GET | `/api/pages` | 白名单 + 密钥 | 自定义页清单（侧边栏菜单用），`{"pages":[{key,label,group}]}` |
| GET | `/static/custom.js` | **免鉴权** | `UseScript` / `UsePage` 的脚本拼在一起 |
| GET | `/static/custom.css` | **免鉴权** | `UseCSS` 的样式（在 mo.css / oao.css 之后加载，可覆盖） |

> **注入内容按"非机密"对待**：浏览器加载 `<script src>` / `<link>` 时带不了自定义请求头，
> 所以这两个端点走的是和 `oao.js` / `mo.js` 一样的免鉴权静态路由 —— **别把密钥、令牌之类的敏感值写进注入的 JS/CSS**。
> 数据接口仍然全部在白名单 + 密钥后面（清单 `/api/pages` 就是要密钥的）。
>
> 声明有问题（Key 非法 / 重复 / Script 为空 / 注入文件读不到）会在启动时 **panic**，不会等到点了菜单才发现。

## 5. 自定义路由与中间件（Go handler）

表格页与自定义页覆盖不到的场景（自定义看板的后端接口、审核动作、对接外部系统的回调），
可以直接往后台挂自己的 **Go HTTP handler**，并串上自己的中间件：

```go
app.UseRouter(func(r *papa.Router) {
    r.Use(accessLog)                       // 中间件：注册顺序正序执行，只对之后注册的路由生效
    r.Group("/api/review", func(g *papa.Router) {
        g.Get("/list", listHandler)        // GET  /api/review/list
        g.Post("/approve", approveHandler) // POST /api/review/approve
    })
    r.Handle("PATCH /api/review/note", noteHandler) // 其余方法用 Handle；pattern 是 Go 1.22 的 ServeMux 语法
})
```

| 方法 | 说明 |
| --- | --- |
| `Use(mw ...Middleware)` | 追加中间件（`func(http.Handler) http.Handler`），正序执行，只影响之后注册的路由 |
| `Group(prefix, fn)` | 子路由组：前缀相加、继承父组中间件 |
| `Get/Post/Put/Delete(pattern, h)` | 常用方法的快捷方式 |
| `Handle(pattern, h)` | 任意方法；pattern 可带方法前缀（`"PATCH /api/x"`） |
| `NoAuth()` | 返回一个**不带后台鉴权**的组视图（见下） |

**鉴权默认开着**：业务路由和后台内置接口一样过「IP 白名单 + 访问令牌」，自定义页里用
`apiFetch` / `apiPost` 调它会自动带上凭据。后台鉴权套在**最外层** —— 白名单/令牌没过的请求
根本不会进你的中间件，所以日志、计数这类带副作用的中间件不会被未鉴权流量污染。

**要对外公开的接口**（webhook、OAuth 回调、健康检查）用 `r.NoAuth()` 显式声明：

```go
r.NoAuth().Post("/webhook/github", webhookHandler) // 对白名单外、没带令牌的来源也开放
```

它是有意为之的逃生舱 —— 默认松掉鉴权才是危险的，所以要求明确写出来。`NoAuth()` 返回的是副本，
不影响原组；在子组上调用时前缀照旧生效。

几条约定：

- **必须在 `Run` 之前注册**（路由在 `Run` 时挂载），且要求 `server.enabled` 与 `server.monitor`
  都开着。注册了却没挂上**不会静默**：启动会打一条醒目错误日志说明有 N 组路由没生效。
- 回调传 `nil` 直接 panic（与 `UsePage` / `RegisterStage` 同风格：声明有问题启动即失败）。
- pattern 与现有路由冲突时是 `http.ServeMux` 的 panic（挂在同一个 mux 上），不会悄悄覆盖。
- 路径以 `/api/` 开头的业务路由走的是同一道 `Monitor.Auth`，`Cache-Control: no-store`
  由它统一加上；`NoAuth()` 的路由没有这层，需要的话自己在 handler 里设置。

`papa new` 生成的工程把这套东西放在 `monitor/` 包里分层：`router.go` 声明路由与中间件、
`controller/` 放 handler、`middleware.go` 放中间件、`model/` 放模型与表格页、`view/` 放自定义页。
`main.go` 只需要 `monitor.Register(app)` 一行，各层分工见每个文件顶部的包注释。

## 6. 动态配置 API

运行期热更爬虫参数。**哪些字段能热更、哪些需要重启，以 [CORE_CONFIG.md](./CORE_CONFIG.md) 第 3 节的清单为准**（这里只讲接口语义，不重复维护一份字段表）：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/config` | 返回当前覆盖层 + 热更字段 / 需重启字段清单 |
| PUT | `/api/config` | 应用运行期配置；body 只收热更字段，需重启字段返回 400 |

### PUT 的合并语义：改你提到的字段

PUT 收的是一份**增量**，按字段并进现有覆盖层，**没提到的保持原样**：

| body 里 | 效果 |
| --- | --- |
| 字段**缺失** | 保持当前覆盖值 |
| 标量**给了值** | 覆盖该字段 |
| 映射**缺失** | 保持当前覆盖的那组 |
| 映射给 `{}` | **清空**这组覆盖 |
| 映射**给了键** | **整组替换**（不是往旧 map 里逐个 merge） |
| 整个 body 是 `{}` | 什么都不改 |

> 早先这里是**整体替换**：只提交一个 `html.timeout`，会把之前设的 `browser.headers`、
> 各队列的 `interval` 全清掉，而响应上看不出任何异常。
>
> **标量没有「清除」这一说** —— JSON 里 `null` 和字段缺失解出来都是 nil，分不开。
> 想让某个标量回落成 `config.yaml` 里的值，直接把它设成那个值即可（覆盖层里会多留一条，行为一致）。
> 要彻底清空覆盖层，删掉 `configs/runtime.yaml` 再重启。

热更只写内存，关停时落盘到 `configs/runtime.yaml`，重启后叠加生效（详见 [CORE_CONFIG.md](./CORE_CONFIG.md)）。

### 优雅退出要求再输一遍令牌

后台「优雅退出」是**高危操作**：点下去会弹一个要你填**自己的访问令牌**的框，服务端拿它去查令牌表，
不对就返回 403 并且不关停。

它的作用是**让人在场**（误点、开着页面走开都不至于把服务停掉），顺带把操作人记进日志
—— 它**不是新的安全边界**：这个令牌和中间件校验用的是同一个，调用方本来就持有。
（库里一条令牌都没配时放行，与中间件的语义一致，否则"还没配凭据"的部署会关不掉服务。）

## 7. 队列治理

「队列治理」模块展示 `error_queue` / `recover_queue` / `repeat_queue` 三个后台治理队列的运行情况，并可逐个手动触发：

| 列 | 含义 |
| --- | --- |
| 状态 | `运行中`（附带本轮已执行时长）/ `空闲` / `已停用`（`*.enabled=false`）；有上次执行错误时鼠标悬停可见 |
| 上次执行 | 上次执行完成距今多久（悬停看绝对时间）· 上次执行耗时 |
| 处理量 | 空闲时=上次处理数，运行中=本轮已处理数；副行显示累计处理数 |
| 待处理 | 当前排队待处理的任务数（低频采样，副行显示采样时间） |
| 操作 | 立即执行一次该队列 |

手动触发的 HTTP 接口（等价于页面上的「立即执行」）：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/api/errorqueue/process` | 手动触发失败任务重投；返回 `{"status":"ok","processed":N}` |
| POST | `/api/recoverqueue/process` | 手动触发卡死任务恢复；返回 `{"status":"ok","recovered":N}` |
| POST | `/api/repeatqueue/process` | 手动触发周期轮询（重投已完成的 repeatable 任务）；返回 `{"status":"ok","repolled":N}` |

数据来源分两类，均**不实时**，且不占用监控页刷新路径：

- 运行状态/耗时/处理量：引擎内存计数，随队列执行即时更新。
- 待处理积压：`COUNT` 查询，由后台按 `server.queue_sample_interval`（默认 `1m`）低频采样；每轮队列执行结束也会立即补采一次。采样频率越低，DB 压力越小。
- 采样仅在 `server.monitor=true` 时启动。

对应的 `/api/monitor` 响应字段：

```jsonc
"queues": {
  "error_queue": {
    "name": "error_queue", "enabled": true, "running": false, "runs": 12,
    "started_at": "...", "last_finish_at": "...", "last_duration": 4000000000,
    "last_processed": 37, "run_processed": 0, "total_processed": 421,
    "backlog": 128, "backlog_at": "...", "last_error": ""
  },
  "recover_queue": { /* ... */ },
  "repeat_queue":  { /* ... */ }
}
```

`last_duration` / `started_at` 等时间字段：`duration` 为纳秒，时间为 RFC3339。

## 8. 日志导出

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/logs` | 列出日志目录文件 |
| GET | `/api/logs/download` | 下载日志；`?file=name` 下载单个，缺省打包全部为 zip |

## 9. 业务自定义数据展示

fetcher 里调 `engine.RecordMetric("key", value)`，监控页「自定义数据」模块实时展示（配合 `engine.GetMetrics()` 读快照）。

`GetMetrics()` 除了业务自定义数据，还会附带**框架级队列治理计数**（同样展示在「自定义数据」模块）：

| 键 | 含义 |
| --- | --- |
| `queue_spilled` | 累计溢出任务数（队列达高水位被回灌 DB 的次数） |
| `queue_spill_backlog` | 当前待回灌的溢出任务数（>0 说明队列持续满） |
| `recover_total` | 累计恢复任务数（recover_queue） |
| `error_retry_total` | 累计失败重投任务数（error_queue） |
| `repeat_repoll_total` | 累计周期轮询重投任务数（repeat_queue） |

> 这三个累计值同时也是「队列治理」模块「处理量」的累计数，同一份引擎内存计数，不会重复统计。
