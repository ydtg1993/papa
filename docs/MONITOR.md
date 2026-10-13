# Papa 监控服务后台手册

> 面向：看懂并用好 OA 监控后台——Dashboard、设置、表格页、站点 Tab、队列手动触发、熔断横幅、日志导出。
> 配置项见 [CORE_CONFIG.md](./CORE_CONFIG.md) 的 `server` 段。

---

## 0. 一句话

框架内置一个 HTTP 监控后台（`server.enabled: true`），访问 `http://localhost:<port>/monitor`，提供 Dashboard、任务队列概览、队列治理、站点 Tab 与概要、表格页、设置、熔断横幅与日志导出。

> **多进程部署时注意**：Dashboard 上的**阶段概览**（排队数、在跑数、各 worker 状态）读的是
> **本进程**的内存快照 —— 任务表是共享的，统计**不是**。跑多个进程（或容器多副本、多机器）时，
> 每个进程的后台只显示自己那部分，「排队中 N」加不起来，任务量也会低于表里的真实值。
> 要全局口径请直接查 `crawler_tasks` 表（按 `stage` / `status` 分组），或按进程分别看。

## 1. 鉴权（访问令牌 + 白名单）

所有 `/api/` 接口都过两道校验：

1. **IP/CIDR 白名单**（存在 `crawler_config` 表的 `cfg_key = ip_whitelist` 行，运行期在设置页改；**不在 config.yaml 里**）。
   语义：**行不存在 / 空数组 / 值解析不出来 → 不限制来源**；有值 → 只允许列出的那些来源。`/monitor` 页面本身只查这一道。
2. **访问令牌**：请求带 `Authorization: Bearer <令牌>`，或 `X-Auth-Key: <令牌>`。方案名按 RFC 7235 **大小写不敏感**（`bearer` / `BEARER` 也收）；令牌本身逐字节精确匹配，不做大小写归一。
   **不再支持 `?key=`**（凭据进 URL 会落进浏览器历史与访问日志）。

**哪些路径要过哪道门**（三类，别混）：

| 谁 | 白名单 | 令牌 |
| --- | --- | --- |
| 后台内置数据接口（`/api/*`，含 oao 表格 `/api/oao/*`、`/api/tokens/*`、`/api/pages`） | 要 | 要 |
| `/monitor` 这张 HTML 壳（还有 `/static/*`） | 页面要、静态资源不要 | **不要** —— 令牌是在页面里填的，填之前也得拿得到页面 |
| **挂在同一个 mux 上的业务路由**（`Router` 默认套的就是 `Monitor.Auth`） | 要 | **要，且不看路径前缀** —— 要公开就显式 `NoAuth()` |

第三行是 2026-10-13 修的：`Monitor.Auth` 以前复用的是内置端点那条"只看 `/api/` 前缀"的判断，
于是**注册在非 `/api/` 路径上的业务路由只剩白名单那一道**（白名单内的任何人都进得来）。见 `changes.md`。

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

队列治理面板的「立即执行」（`POST /api/{error,repeat}queue/process`）与 Dashboard 熔断横幅的「恢复抓取」
也**记进操作日志**（与令牌页同一处境：这三处是后台自带的原生接口、不走表格组件，但都会批量改数据 ——
审计不该因为"这不是表格动作"就断档）。审计行里：`table` = 队列名（`repeat_queue` / `error_queue`）
或 `breaker`，`action` = `process` / `resume`，`values.processed` = 实际处理条数，失败原因在 `error` 列。

> 写入是**异步**的（后台队列，不占业务响应）：写库失败、或队列堵了（写库跟不上）时，
> 那条记录会**落到日志文件**里（一行 JSON，可捞回来补录），既不丢审计也不反压业务。
> 关停时会先把队列排空再关数据库。

> 升级时表要自己建 —— 跑一次迁移（脚手架项目里 `make migrate` 与 `papa migrate` 等价，
> 独立使用 CLI 时运行 `papa migrate`，见 [CORE_CONFIG.md](./CORE_CONFIG.md) 第 2 节。
> 之后用 `papa token add` 给每个人建一把 —— 原来共用的 `auth_key` 请停用/删掉对应令牌。

## 2. 设置 API

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/settings` | 返回当前白名单（回显**原样**输入）、它持久化在哪（`whitelist_source`，空串 = 该宿主没接配置存储）、日志目录 |
| POST | `/api/settings/whitelist` | 更新白名单并交给宿主持久化（papa 落在 `crawler_config.ip_whitelist`），**成功即生效**；body `{"whitelist": ["127.0.0.1","10.0.0.0/8"]}`。写时严格：有条目不是 IP/CIDR 就整单 400 不落库；落库失败 500 且旧名单继续生效 |
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
> 所以这一页是后台自带的原生模块：`/api/tokens` 三条接口 + `mo.js` 里自己渲染的表格，接口在 `admin/tokenadmin`。

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

内置「任务」表（`admin/tasksource`）是动作如何接线的参考实现，它带了八个动作（见下表）——
写操作都走 `engine.Engine` 的条件更新，
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
papa 的 `admin/gormsource` 把算子落成 SQL：`OpLike` → `LIKE '%值%'`（前后通配，用不上索引），
`OpPrefix` → `LIKE '值%'`（只有后通配，能走索引），`OpEq`/`OpIn`/`OpBetween`/`OpGt`/`OpLt` 各对应同名条件。
参考实现看 `admin/tasksource`（内置「任务」表）或 hg2 的 `tablesource`。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `{prefix}/tables` | 列出已注册的表格及列/筛选/操作元数据（侧边栏菜单用） |
| GET | `{prefix}/{table}` | 分页数据；query：`page`、`size`（默认取表声明，上限 200）、`search`、`sort`（`col`/`-col`）、`filter[col]=val` |
| POST | `{prefix}/{table}/action/{key}` | 转发操作给业务 Handler（表未声明该动作一律 404） |

`prefix` 默认 `/api/oao`；静态资源挂在 `/static/oao/`。**这几条 API** 走监控后台的白名单 + 密钥校验（组件复用宿主注入的中间件）；`/static/oao/` 与 `/static/mo.js` 一样**免鉴权**（浏览器 `<script src>` 带不了自定义请求头），所以别往组件自带的静态资源里放机密。

- **列怎么显示**：`Render` 可选 `text`（省略号 + 悬停看全文）/ `input`（只读输入框，
  长度可控、横向滚动看全，**不是编辑**）/ `link` / `image` / `enum`（彩色标签）/ `time` / `json` / `custom`。
  留空按 `Kind` 推断。
- **哪些能筛**：只声明需要的字段，未声明的字段不可筛；列名与排序同样走白名单，杜绝注入。
- **菜单自动出现**：按 `Group` 分组渲染到侧边栏，注册即出现，前端无需改代码。
- **刷新策略**：表格页只在用户操作（筛选/排序/翻页）时刷新，不参与 Dashboard 的 3 秒轮询。

内置的「站点」表（`admin/sitesource`）展示 `crawler_sites`（引擎启动时按站点声明播种、熔断状态按列回写），
**只读**（原来的「轮询任务 / 暂停·恢复自动轮询」随按站点队列一起移除了）。列里的 `base_url` / `stage_count` 是
启动时按声明抄的：**改声明要重启，直接改库不生效**。

内置的「任务」表（`admin/tasksource`）展示的就是 `crawler_tasks` 表，可作为完整示例。
它带七个写操作加一个只读入口（组件超过 3 个动作时只平铺前两个，其余收进「更多 ▾」）：

| 动作 | 位置 | 说明 |
| --- | --- | --- |
| **重投** | 平铺 | 重置为待处理并重新投递（队列满时与正常投递一致溢出到 DB 回灌），带 `reprocess` 版本条件防重复点击 |
| **追踪** | 平铺 | 打开该任务的**步骤时间线**：按「第 N 次尝试」分段，每段列出步骤名、耗时、失败步（红）与非致命步（橙，`task.Trace.Warn`）的错误分类与信息、以及该步采集到的数据（可折叠）。引擎自己也会补两条：「任务失败」（失败尝试的原因）与「归档页面」（失败那一刻的页面，带「下载这一页」按钮） |
| **加急** | 更多 ▾ | 把该任务投一份到所属阶段的**快车道**，插到排队任务前面执行。见下方说明 |
| **开轮询** | 更多 ▾ | 让该任务参与周期轮询：只改 `repeatable` 标记并把「下次轮询」置为现在，下一轮扫描才生效；已经开着再点 → 409 |
| **停轮询** | 更多 ▾ | 停掉该任务的周期轮询，已经跑起来的这一轮不受影响；已经停着再点 → 409 |
| **设轮询周期** | 更多 ▾ | 弹表单填周期（秒，0 = 用全局 `repeat_queue.interval`）；只改周期，不立刻重跑一次；带行快照版本守卫 |
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

**关于「开 / 停轮询 / 设轮询周期」**：某条任务**要不要周期性重跑**由任务行上的 `repeatable` 列决定、
**多久一次**由 `repeat_interval`（秒，0 = 用全局 `repeat_queue.interval`）决定 —— 粒度都是任务，不是阶段。
首次提交时由 `papa.Task{Repeatable: true, RepeatInterval: 10*time.Minute}` 播种，之后就用这三个动作改
（业务里等价的入口是 `engine.SetTaskRepeatable(id, on)` / `engine.SetTaskRepeatInterval(id, was, seconds)`，
见 [REPEAT_QUEUE.md](./REPEAT_QUEUE.md) 第 3 节。任务表上有对应的三列：**轮询周期(秒) / 上次轮询 / 下次轮询** ——
队列只按「下次轮询」到点投，改周期会连带重算它。
三个动作都**只改标记**：下一轮扫描时才生效（开轮询会把"下次轮询"置为现在，所以它就是下一轮），
**不会**顺手立刻重跑一次（想立刻跑请用「重投」）；已经开着再开、已经停着再停都返回 409
（`repeatable` 这一列本身就是版本守卫，「设轮询周期」用的是行快照里的旧周期值，与 `urgent`/`重投` 同一思路）。
「停轮询」也不打断已经跑起来的那一轮 —— 它跑完这次就没有下一次。
注意**停轮询 ≠ 放开去重**：行还在（`success`/`failed`），同 `site:stage:url` 再提交仍会被静默吞掉，
要立刻重跑只能用「重投」。

**关于「追踪」**：这个动作在服务端**不做事**，只回一句成功 —— 因为表格组件的动作成功时只能回
`{"status":"ok"}`、带不回数据，而组件也没有对外的事件钩子。真正的展示由
`/static/trace.js` 接管：它包了一层 `window.fetch` 嗅探这次动作请求，从请求体里取出任务 id，
再自己拉 `GET /api/task/trace?id=<id>`。所以点「追踪」时那句「追踪成功」的 Toast 是组件的
固有行为，不是多余提示。

抽屉里失败尝试的最后一条是**引擎补的**「任务失败」（错误分类 + 消息）—— handler 不必自己再上报一次；
若这一次抓到过页面，前面还有一条「归档页面」，那一步带「下载这一页」按钮（`GET /api/task/page?file=`，
回的是文件本身），拿下去对着真实页面改选择器最省事（配置见 [CORE_CONFIG.md](./CORE_CONFIG.md) 的 `archive` 一节）。

步骤追踪需要在配置里显式开启（`crawler.trace.enabled`，见 [CORE_CONFIG.md](./CORE_CONFIG.md)），
handler 侧的用法见 [FETCHER_WRITING_GUIDE.md](./FETCHER_WRITING_GUIDE.md) 1.3。
**未开启时点「追踪」不会白屏**，抽屉里会直接显示「步骤追踪未开启」。
要先跑一次迁移把 `crawler_task_trace` 建出来（脚手架项目里 `make migrate` 与 `papa migrate` 等价）；开关开着但表不存在时，
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

**鉴权默认开着**：业务路由和后台内置接口一样过「IP 白名单 + 访问令牌」（**不看路径前缀** ——
注册在 `/internal/x` 这种路径上照样要令牌），自定义页里用
`apiFetch` / `apiPost` 调它会自动带上凭据。后台鉴权套在**最外层** —— 白名单/令牌没过的请求
根本不会进你的中间件，所以日志、计数这类带副作用的中间件不会被未鉴权流量污染。

**要对外公开的接口**（webhook、OAuth 回调、健康检查）用 `r.NoAuth()` 显式声明：

```go
r.NoAuth().Post("/webhook/github", webhookHandler) // 对白名单外、没带令牌的来源也开放
```

它是有意为之的逃生舱 —— 默认松掉鉴权才是危险的，所以要求明确写出来。`NoAuth()` 返回的是副本，
不影响原组；在子组上调用时前缀照旧生效。

几条约定：

- **必须在 `Run` 之前注册**（路由在 `Run` 时挂载），且要求 `server.enabled: true`。
  注册了却没挂上**不会静默**：启动会打一条醒目错误日志说明有 N 组路由没生效。
- 回调传 `nil` 直接 panic（与 `UsePage` / `RegisterSites` 同风格：声明有问题启动即失败）。
- pattern 与现有路由冲突时是 `http.ServeMux` 的 panic（挂在同一个 mux 上），不会悄悄覆盖。
- 业务路由走的是同一道 `Monitor.Auth`：**不管路径是否以 `/api/` 开头**，白名单与令牌都要过
  （`NoAuth()` 的组除外），`Cache-Control: no-store` 也由它统一加上 —— 需要的话自己在 handler 里覆盖。

`papa new` 生成的工程把这套东西放在 `monitor/` 包里分层：`router.go` 声明路由与中间件、
`controller/` 放 handler、`middleware.go` 放中间件、`tables.go` 放表格页、`view/` 放自定义页。
表模型不在这层 —— 和 `fetcher/` 同级放在项目根的 `models` 包（建表清单 `models.Models()`），
爬虫业务与后台服务共用。`main.go` 只需要 `monitor.Register(app)` 一行，各层分工见每个文件顶部的包注释。

## 6. 队列治理

「队列治理」模块展示**两个全局治理队列**：错误队列（`error_queue`）与轮询队列（`repeat_queue`），
**各占一行**，都能单独手动触发 —— 不再按站点拆（错误队列重投所有站点符合条件的失败任务，轮询队列投所有已到点的 repeatable 任务）：

> `recover_queue` **不在这个面板上**：它只在进程启动那一刻跑一次，没有周期、没有积压可看，
> 也就没有「立即执行一次」的意义。其公开方法仅供引擎启动恢复流程使用；不要把它接成运行期按钮或定时任务，以免恢复语义与正在执行的任务生命周期交叉。
> 见 [RECOVER_QUEUE.md](./RECOVER_QUEUE.md)。

| 列 | 含义 |
| --- | --- |
| 状态 | `运行中`（附带本轮已执行时长）/ `空闲` / `已停用`（配置里没写这段，或 `enabled: false`）；有上次执行错误时鼠标悬停可见 |
| 上次执行 | 上次执行完成距今多久（悬停看绝对时间）· 上次执行耗时 |
| 处理量 | 空闲时=上次处理数，运行中=本轮已处理数；副行显示累计处理数 |
| 待处理 | 当前排队待处理的任务数（低频采样，副行显示采样时间） |
| 操作 | 立即执行一次该队列（**写一条操作日志**） |

手动触发的 HTTP 接口（等价于页面上的「立即执行」，同样写操作日志）：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/api/errorqueue/process` | 手动触发失败任务重投；返回 `{"status":"ok","processed":N}` |
| POST | `/api/repeatqueue/process` | 手动触发周期轮询（重投**到点的** repeatable 任务；想强制某一条立刻重跑用任务表的「重投」）；返回 `{"status":"ok","repolled":N}` |

数据来源分两类，均**不实时**，且不占用监控页刷新路径：

- 运行状态/耗时/处理量：引擎内存计数，随队列执行即时更新。
- 待处理积压：`COUNT` 查询，由后台按 `server.queue_sample_interval`（默认 `1m`）低频采样；每轮队列执行结束也会立即补采一次。采样频率越低，DB 压力越小。
- 采样仅在 `server.enabled: true` 时启动。

对应的 `/api/monitor` 响应字段：

```jsonc
"queues": {
  "error_queue":  { "name": "error_queue",  "enabled": true, "running": false, "runs": 12, /* … */ },
  "repeat_queue": { "name": "repeat_queue", "enabled": true, "running": false, "runs": 0,  /* … */ }
}
```

队列的参数（`enabled` / `worker_count` / `interval` / …）取自 `config.yaml` 的全局段
（见 [CORE_CONFIG.md](./CORE_CONFIG.md)）。

`last_duration` / `started_at` 等时间字段：`duration` 为纳秒，时间为 RFC3339。

### 站点 Tab：多站时按站点把面板分开

多站点时，内容区顶部会多一排**站点 Tab**（「概览（全部）」+ 每个站点一个；数据来自 `/api/monitor` 的 `sites`，
由声明启动时播种到 `crawler_sites` 表）：

| 面板 | 切到某个站点 tab 时 |
| --- | --- |
| 阶段/池子（仪表盘与「执行队列」） | 只显示**该站**的阶段（阶段快照里现在带 `site`） |
| 队列治理 | **不过滤** —— 两条队列是全局各一条，切到哪个 tab 都显示 |
| 站点概要（tab 下面那行） | BaseURL / 阶段数 / 是否被手动暂停 |
| **熔断横幅** | **不过滤** —— 它是告警，切到哪个 tab 都显示全部被闸住的站 |
| **阶段入口**（卡片） | **跟着 tab 走**：切到某站只留那一张卡；概览显示全部站点（当总控台用） |
| 任务表（表格页） | **跟着 tab 走**：切到某站再打开任务表，会带着 `site=<该站>` 的**预置筛选**打开（oao 的 `opts.filter`，筛选栏里填好、可改可清）；已开着表时切 tab 会按新站点重画。概览 → 不带筛选。**需要 oao ≥ v1.3.0**（带 `opts.filter` 的那版），老版本会静默忽略第三个参数 |

> 站点概要与 `sites` 里的字段：`key / base_url / stage_count / breaker_paused / breaker_paused_at`。
> `base_url` 与 `stage_count` 是**启动时按声明抄的**（改声明要重启；直接改库不生效）。
> `breaker_paused*` 这一对**只由后台手动暂停/恢复写**（熔断触发不写它 —— 它只往引擎的暂停信号里写）：
> 所以列名叫 `breaker_*` 但含义是"**手动**暂停"，要看熔断触发请读横幅（`breakers[]` 的 `source`）。
> 「每站每阶段的池子统计」是**实时**数字，不落库 —— 它就在 `stages` 快照里：**键是 `site:stage`**
> （阶段名只在本站内唯一，两个站的 `catalog` 是两条），条目里另带裸阶段名 `stage` 与 `site` ——
> 站点 Tab 按 `site` 分组，面板标题用 `stage`。

```jsonc
"sites": {
  "huangguo": {
    "key": "huangguo", "base_url": "https://example.com/", "stage_count": 3,
    "breaker_paused": false, "breaker_paused_at": "0001-01-01T00:00:00Z"
  }
}
```

## 7. 日志导出

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/logs` | 列出日志目录文件 |
| GET | `/api/logs/download` | 下载日志；`?file=name` 下载单个，缺省打包全部为 zip |

## 8. 业务自定义数据展示

fetcher 里调 `engine.RecordMetric("key", value)`，监控页「自定义数据」模块实时展示（配合 `engine.GetMetrics()` 读快照）。

`GetMetrics()` 除了业务自定义数据，还会附带**框架级队列治理计数**（同样展示在「自定义数据」模块）：

| 键 | 含义 |
| --- | --- |
| `queue_spilled` | 累计溢出任务数（任务没能直接入队、改由溢出表寄放的次数）。三种来由：队列达高水位 · 阶段/站点被暂停 · **引擎已停机**（`Engine.Stop` 之后不再建池，晚到的投递一律寄放）。后两种不是容量问题 —— 看这一栏时别当成队列压力 |
| `queue_spill_backlog` | 当前待回灌的溢出任务数 —— 两种来由：队列持续满，**或有站点被暂停**（暂停期间不接新活，任务都寄放在这里，恢复时立刻灌回） |
| `recover_total` | 累计启动恢复的任务数（`recover_queue`，只在启动跑一次） |
| `error_retry_total` | 累计失败重投任务数（error_queue） |
| `repeat_repoll_total` | 累计周期轮询重投任务数（repeat_queue） |

> `error_retry_total` / `repeat_repoll_total` 同时也是「队列治理」模块「处理量」的累计数，
> 同一份引擎内存计数，不会重复统计；`recover_total` 只在启动恢复时累加，面板上没有对应项。

## 9. 暂停与熔断

**暂停只有一处状态**：引擎的暂停信号（`engine/pause.go`），按 scope 记（整站或某站的某阶段）、
带原因与来源。两个地方读它、两层一起生效：

| 层 | 位置 | 语义 |
| --- | --- | --- |
| **取任务点** | 每个工作池绑的闸门 | **冻住**：worker 停在取任务之前，队列里已经躺着的那批也不动，在途任务跑完为止 |
| **入队点** | `submitToStage` | **收口**：不再接新活，任务寄放到溢出表（`queue_spill_backlog` 会涨），「恢复」时立刻灌回 |

**两层各有一个粒度**（同一张信号表，只是 scope 不同）：

- **站点那一层**（`scope`）= **管这一站的全部阶段** —— 包括后台列表里根本看不到的那些
  （`detail` / `episode` 这种由上一级投出来的）。**框架自带的界面只做到这一层**（卡片头上那个
  「暂停整站」）：它是唯一不需要站点知识的粒度（"这一站先别抓"）。
- **阶段那一层**（`scope` + `stage`）= 只掐那一个阶段，别的照跑（"catalog 先别抓、让 detail 跑完"）。
  这是**接口**，框架不替业务决定界面长什么样 —— 见 §10 的"阶段级控制是接口，不是框架的界面"。

**暂停不跨重启**：进程重启即恢复运行（起进程本身就是一次人工介入）。要长期停就改声明、
或在部署层面停进程 —— 别指望点一下「暂停」能撑过一次重启。

**来源只有两种**（payload 里的 `source`）：`manual`（后台/业务手动）与 `breaker`（熔断自动触发，
见下）。**熔断只是"自动往这张表里写一条的人"** —— 它自己数窗口内的终态失败、比阈值、发
`AlertCritical`，停与不停、现场怎么写、横幅怎么画都归暂停信号。

熔断触发时（或业务自己调了 `engine.PauseCrawling` / 后台点了「暂停」），**整页顶部**出一条红色横幅 ——
它在所有模块之上，切到哪个页都看得到。横幅状态跟着 `/api/monitor` 每轮刷新一起来
（不额外发请求），所以别的标签页里触发的暂停，这边几秒内也能看到。

**每个站点一条独立的暂停**（阈值按站点配：`SiteSpec.Breaker`，见 FETCHER_WRITING_GUIDE.md 7.2），
横幅**逐站一行**：每行写清是哪一站（站点是必填维度，没有空键那一档）、哪个阶段、窗口内多少次终态失败、
暂停于何时，并各带一个「恢复」按钮（只放行那一站）；两个以上被闸住时额外给一个「全部恢复」。

**手动暂停与熔断触发在横幅上是两种说法**（看 `source`，不再靠猜）：熔断触发一定带着阶段与
"窗口内 N 次失败，阈值 M"；后台手动按的（那张站点卡片上的「暂停」）显示成
`抓取已暂停（后台手动暂停）` ——
不然会读出"阈值 0 / 0 次失败"这种幻觉（熔断没启用时阈值本来就是 0）。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/breaker` | 各站点的状态：`{"breakers":[...]}`（按站点名字典序） |
| POST | `/api/breaker/resume` | 放行。**不点名 = 全部站点**；点名：`?scope=siteb` 或请求体 `{"scope":"siteb"}`。未知 scope 回 **400**（不静默放行全部） |
| POST | `/api/breaker/pause` | 暂停。**必须点名**（`{"scope":"siteb"}`）；不点名回 **400** —— 放行是往安全方向走，暂停是往危险方向走，一个误点不该把全部站点停掉 |

两个端点都再认一个可选的 **`stage`**（同上：query 或请求体）：给了就只动那一站的**那一个阶段**
（`{"scope":"siteb","stage":"catalog"}`），不给就是整个站点那一层。**给了 `stage` 却没给 `scope` 回 400**
（阶段属于某个站，不能当成"整站"或"全部"）。

`resumed=false` 表示**当前本来就不在暂停态**（比如另一个人先点了），这不是错误 ——
重复点击恢复不该看起来像失败，但也不能回一个含糊的 ok 让调用方以为是自己放行的。
放行了哪些 scope 在 `resumed_scopes` 里。

暂停与放行都是**人在场的干预**（它们停 / 通的是整条抓取线），所以状态真的变了之后会写一条操作日志
（`Table: breaker` / `Action: pause` 或 `resume`，带操作人与 `scope`）—— 前提是开了 `server.operation_log`。
「本来就在暂停态」那种空点不记（没有状态变更），也不报错。

`/api/monitor` 里的 `breakers` 数组（每项就是下面这个对象；`site` 是这个 scope 的站点键）：

```jsonc
"breakers": [
  {
    "enabled": true,
    "site": "huangguo",    // 哪个站（站点是必填维度，没有空键那一档）
    "window": 300000000000, // duration 是纳秒
    "threshold": 50,
    "in_window": 12,        // 实时值：当前窗口内的终态失败数
    "paused": true,
    "paused_at": "...",
    "resumed_at": "...",
    "reason": "窗口内终态失败数达到阈值",
    "source": "breaker",    // 这条暂停是谁下的：manual（后台手动）/ breaker（熔断自动触发）
    "stage": "catalog",   // 触发的阶段（裸名）；哪个站看上面的 "site"
    "failures": 50
  }
]
```

另一处能看出站点的是**告警**：任务终态失败与熔断触发都走同一套 `Notifier`，事件里带
`site`（`papa.TaskError.Site`）—— webhook 那头可以据此路由（@对应的负责人）。

## 10. 阶段入口（手动点火）

仪表盘上的**阶段入口**面板：**一站一张卡片**，卡头是站点名与「暂停 / 恢复」（复用上面那条暂停信号），
卡体是该站的入口阶段 —— 每个阶段一行，写明它的入口模式与上次点火时间。

什么阶段会出现、能不能点，都由声明里的 `StageSpec.Entry` 决定（见 [FETCHER_WRITING_GUIDE.md](./FETCHER_WRITING_GUIDE.md) §7）：

| `Entry` | 启动时投？ | 卡片上 |
| --- | --- | --- |
| `papa.EntryAuto` | 投 | 列出来，标 `auto`，**不给按钮**（已自动投过） |
| `papa.EntryManual` | 不投 | 列出来，标 `manual`，带「点火」按钮 |
| 不写（`EntryOff`） | 不投 | **不列**（这个阶段没有入口这回事） |

它解决的是"入口任务在进程启动那一刻就全投出去了、多站时没法控制先后"：
把想晚点跑的站声明成 `EntryManual`，等你在后台点。**点火是幂等的** —— 入口任务自带幂等键，
已经有终态行（成功或失败）的入口会被 `SubmitTask` 静默跳过，所以按钮的语义是
"把还没有终态行的入口投一遍"，不会重跑已经完成的那些。

> **注意启动那一下拦不住**：`EntryAuto` 在 `ApplyRegisterStage`（HTTP 服务起来之前）就把入口投了，
> 所以"暂停按钮"管的是**之后**的抓取。要让某一站启动时不投，得声明成 `EntryManual`。
>
> 站点被暂停时**点火是允许的**：任务照常入库，但**寄放在溢出表**里（不接新活、也不占 worker），
> 点「恢复」时立刻灌回 —— 这正是"先把入口排好、再放开这一站"的用法。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/api/entries/trigger` | 点火。请求体 `{"site":"yeguodj","stage":"catalog"}`（也认 `?site=&stage=`，给脚本用）。成功回 `{"status":"ok","site":…,"stage":…}`；**点不动的原因一律 409 + 引擎那句话**：没入口 / 它是 auto / 引擎还没就绪 / 已有一次在跑 |
| GET | `/api/monitor` 的 `entries` | 各站点入口阶段的快照（数组，按站点 → 阶段排序，见下） |

```jsonc
"entries": [
  {
    "site": "yeguodj",
    "stage": "catalog",
    "mode": "manual",              // 入口模式：auto（启动已投过）/ manual（等后台点火）
    "last_triggered_at": "…",      // 上次**手动点火**的时刻；零值 = 从未（启动自动投的那次不算）
    "paused": false,               // 这个阶段现在停不停 —— **含继承**：整站停着时每个阶段都算停
    "pause_source": ""             // 暂停是谁下的：manual / breaker；没停时为空
  }
]
```

这张卡只列**有入口任务**的阶段，而且只**显示**状态（每行那个`已暂停`标记来自 `entries[].paused`），
暂停/恢复是**站点那一层**的（卡头那个按钮）。没有入口行的阶段（`detail` / `episode` 这种由上一级投出来的）
不在卡上，但它们**同样受整站那一层管** —— 「暂停整站」停的就是这一站的全部阶段。

### 阶段级控制是接口，不是框架的界面

"只掐某一个阶段"（比如 `catalog` 先别抓、`detail` 照跑）框架**给的是接口，不做界面** ——
要不要在后台开到这个粒度、按什么层级呈现，由业务自己接：

| 接口 | 形态 |
| --- | --- |
| HTTP | `/api/breaker/pause` 与 `/api/breaker/resume` 都认一个可选 `stage`：`{"scope":"yeguodj","stage":"catalog"}`（只动那一个阶段；**给了 `stage` 没给 `scope` 回 400**） |
| 引擎 | `Engine.PauseStage(site, stage, reason)` / `Engine.ResumeStage(site, stage)`；读状态用 `Engine.StageEntries()` 里的 `paused` / `pause_source` |
| 前端 | 静态文件里的 `pauseCrawling(site, stage)` / `resumeCrawling(site, stage)` 也是这个签名，注进来的页可以直接调 |

接法就是本仓现成那两条扩展点：`app.UsePage` 注入一个页 + `app.UseRouter` 加一条路由（见 §4/§5），
或者干脆只用 HTTP 接口写个脚本 —— 框架不替你选。

参数走**请求体**而不是 query string 是有意的：站点键与阶段名除了不能带 `:` 什么都允许
（空格、`&`、中文……），塞进 `?site=` 会被 URL 编码坑。

点火会**投一批任务**（入口任务还常带轮询周期），所以成功与失败都会写一条操作日志
（`Table: stage_entry` / `Action: trigger`，带站点、阶段、模式、操作人与 IP）——
前提是开了 `server.operation_log`；接口没配置（404）不记（那不是一次触发）。
