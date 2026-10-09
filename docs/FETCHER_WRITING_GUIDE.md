# Papa Fetcher 书写手册（路径 B：AI 直接书写 Go fetcher）

> 面向人群：要把一个新目标站点接入 Papa，让 AI 帮你写抓取逻辑的你。
>
> 这份手册回答一件事：**给定一个目标网站，如何把它变成 Papa 里能跑起来的 fetcher**。
> 不涉及声明式策略（那类东西尚未实现）。
>
> 抓取结果如何落库、怎么生成 model，见 [DATA_LANDING_GUIDE.md](./DATA_LANDING_GUIDE.md)。

---

## 0. 一句话工作流

```text
papa new <project-name>    # 用脚手架生成新项目骨架（main.go / fetcher / models / monitor / config / docker / docs / Makefile / logs）
  -> 你投喂目标站点，AI 在生成的 fetcher/fetch_catalog.go 里填抓取逻辑（只 import github.com/ydtg1993/papa/v2）
  -> 在 config.yaml 加 stage、在 main.go 注册 fetcher，并在注册回调里提交起始 URL
  -> 任务被 worker 池调度执行，结果通过结果 API 写入 crawler_tasks 表
```

整个过程 AI 不修改框架代码，只在你自己的项目里新增/修改 fetcher，并给 `config.yaml` / `main.go` 加几行。

---

## 1. 核心契约（AI 必须遵守的边界）

### 1.1 Fetcher 接口

框架对外是一个门面包 `github.com/ydtg1993/papa/v2`，fetcher 只需 import 它：

```go
type Fetcher interface {
    GetStage() string
    FetchHandler(ctx context.Context, task *Task, engine *Engine) error
}
```

- `GetStage()` 返回阶段名，它在**整个程序里必须唯一**（多站就带站点前缀，如 `hgd_catalog`）。重名、缺 `WorkerCount`/`QueueSize`、`Delay` 写错都会在 `RegisterSites` 时 panic 并点名 —— 阶段的存在性与参数都在 `configs/sites/<站名>.go` 的声明里（见 7.2）。
- **可选**：fetcher 还可以实现三个接口，都是"实现一个方法就自动生效"，不实现也不影响运行：
  ```go
  // 声明本阶段要用下载器 → 框架在启动时校验接线（没接直接 panic，而不是等第一条任务跑到那一步）
  func (f *FetchVideo) NeedsFiledown() bool { return true } // 会调 engine.GetFiledown()
  func (f *FetchVideo) NeedsM3U8() bool     { return true } // 会调 engine.GetM3U8()

  // 入口任务："注册即投一批起始任务" → 框架在启动时自动调（那时所有阶段的池子都已建好）
  func (f *FetchCatalog) SubmitEntries(engine *papa.Engine) { /* engine.SubmitTask(...) */ }
  ```
- `FetchHandler` 返回 `nil` → 引擎把任务标记为 `success`；返回普通 `error` → 引擎按该 stage 的 `retry.max_attempts` / `retry.backoff` 自动重试，最终失败标记为 `failed`。
- 返回 `papa.WrapNoRetry(err)` 或 `papa.WrapNoRetryKind(kind, err)` → 引擎**不重试**，直接把任务标 `failed` 并触发告警，适合「结构错误 / 404 / 访问受限」这类重试无意义的失败。
- 用 `engine.FetchHTML` 抓静态页时，**判失败原因别看错误文本**：非 2xx 是 `*htmlfetch.StatusError`（`htmlfetch.StatusCode(err)` 直接取码），响应体超 `html.max_body_size` 是 `*htmlfetch.BodyTooLargeError`。文本匹配会误伤 —— 把 `max_body_size` 配成 `4040000` 时，"html response exceeds 4040000 bytes" 里就带着 `404`，一个该重试的错误会被判成"不可重试的 not_found"。
  ```go
  if code, ok := htmlfetch.StatusCode(err); ok && (code == 404 || code == 410) {
      return papa.WrapNoRetryKind("not-found", err)
  }
  ```
- **不要**在 fetcher 里自己调 `task.UpdateStatus`，状态由引擎自动维护；你只负责提取结果并写库。
- **日志由框架兜底**：`FetchHandler` 返回的 error 会由引擎结构化记入日志并触发告警（`stage/task_id/url/retry/kind`），你**不需要**再自己拼日志。同样，`engine.SubmitTask` / `SubmitTasks` 的提交类错误（非法 stage、入库失败、入队失败）框架也会自动记录——你只需在返回值上做控制流判断（要不要继续、要不要 abort），不用再记一遍。

### 1.2 Task 结构

```go
type Task struct {
    ID             int               // 数据库记录 ID，写结果时用它定位
    PID            int               // 父任务 ID，派发子任务时用它关联
    URL            string            // 当前任务要处理的 URL
    Retry          int
    Stage          string
    Repeatable     bool
    Meta           map[string]string // 业务键（如 series_id/episode_id），与 URL 解耦
    IdempotencyKey string            // 自定义幂等键，空则回退 stage|url
    NotBefore      time.Time         // 延迟投递：最早可执行时间
    Delay          time.Duration     // 延迟投递：相对现在的延迟
    Urgent         bool              // 加急：投到所属阶段的快车道，插到排队任务前面
    Trace          *Trace            // 本次执行的步骤记录器，由引擎挂上；关闭时是 nil（调用安全）
}
```

- `Meta` 承载业务键（`series_id`/`episode_id` 等），**不要**再把它们拼进 URL 或用 `hg2:series:123` 之类的 scheme；handler 里直接读 `task.Meta["series_id"]`。
  **它会随行落库**（`crawler_tasks.meta`，json 列）：恢复队列（进程重启）、轮询队列、错误队列重投、后台「重投 / 加急」这几条「从行重建任务」的路都会把它读回来，handler 不必再自己按 URL 回查业务表。
  两边不一致时以**这一列**为准 —— 提交时带了非空 `Meta` 就刷新它，带空 `Meta` 的提交不动它（重投路径自己就是照着库里那份读的）。
  它与 `IdempotencyKey` 是两件事，别互相替代：后者只保证「同一个任务不重复投递」，前者回答「这条任务是哪条业务行」。
- `IdempotencyKey` 自定义去重键（如「标准化分类 URL + 页码」），空值回退到默认的 `stage|url`。
- `NotBefore` / `Delay` 实现延迟投递：任务到点才入队，不空占 worker（反爬要随机间隔时设 `Delay` 即可，别在 handler 里 `time.Sleep`）。
- `Urgent` 加急：该任务投到所属阶段的**快车道**，插到常规队列前面。适合"怀疑某条有问题、想单独跑一遍看着它跑"的探测任务（`&papa.Task{URL: u, Stage: "detail", Urgent: true}`）。注意它只省**排队**时间 —— 该阶段 worker 全在忙的时候，插队也快不了；worker 认领后 `urgent` 列自动归零，是一次性的。
- `Trace` 是**本次执行的步骤记录器**，由引擎在调用 `FetchHandler` 前挂上（见 1.3）。你只管调它的方法，不用判空。

### 1.3 步骤追踪（`task.Trace`）

想知道「这条任务跑到哪一步、哪一步挂了、那一步采到了什么」时，在 handler 里按步骤上报：

```go
func (f *FetchDetail) FetchHandler(ctx context.Context, task *papa.Task, engine *papa.Engine) error {
	doc, finalURL, err := engine.FetchRendered(ctx, task.URL, ".detail-box")
	if err != nil {
		task.Trace.Fail("渲染详情页", err, map[string]string{"url": task.URL})
		return err
	}
	task.Trace.Step("渲染详情页", map[string]string{"final_url": finalURL})

	detail, err := parseDetail(doc)
	if err != nil {
		task.Trace.Fail("解析字段", err, doc.Text()) // 挂掉的现场数据在这里带上
		return err
	}
	task.Trace.Step("解析字段", detail)

	return engine.SaveResult(task.ID, detail.Title, detail)
}
```

- **三档，选错档会让后台误报**：
  - `Step(name, data)` —— 这一步做完了，耗时 = 距上一个 `Step`（首步距本次尝试开始）的间隔；
  - `Fail(name, err, data)` —— 这一步失败了，带上错误分类与现场数据；
  - `Warn(name, err, data)` —— **非致命**：出了点事，但不该把任务判失败。典型是附带产物 —— 封面没下下来、附件缺了、某个可选字段没解析出来，而任务本身是成功的（用 `Fail` 会把"任务失败了"的信号发出去）。它**不影响任务终态**，只写进追踪，后台用橙色和红色的失败步骤分开显示。
- **`data` 只在失败的尝试里落库**：成功的尝试（绝大多数）一条 `data` 都不写，写入量按失败率走。传 `nil` 也完全可以，只留步骤骨架。
  **例外**：`Warn` 步骤的 `data` 在成功的尝试里也保留 —— 它记的正是"任务成功了、但这一步降级了"，而下一次成功的尝试里它照样只是 warn，剥掉就等于把这条唯一的线索永远丢掉（warn 按定义是异常，量不成问题）。
- **它与日志的分工**：1.1 那条「日志由框架兜底、不要再自己记」的规则**不变** —— trace 是可查询的结构化步骤，不是日志的替代品。返回 `error` 该返回还是返回，`task.Trace` 只是额外告诉框架「走到哪一步了」。
- **默认关闭**：`crawler.trace.enabled` 打开才写库（见 [CORE_CONFIG.md](./CORE_CONFIG.md)）。关闭时 `task.Trace` 为 `nil`，上面所有调用都是安全的 no-op，**不需要判空**。记录保留期由 `crawler.trace.retention` 控制（默认 7 天，后台按批清理）。
- 每次尝试（`FetchHandler` 的一次调用）单独成组，后台任务表的「追踪」动作按「第 N 次尝试」分段展示；handler panic 时，panic 之前已上报的步骤同样会落库。
- **框架自己也会写一步**：任务被加急时（`Urgent: true` 或后台「加急」动作），第一次尝试的第一步是「加急执行」。它不需要你做什么，但你的步骤名别撞车。

### 1.4 Engine 暴露的能力

fetcher 里通过 `engine` 参数能拿到的东西：

| 方法 | 用途 |
| --- | --- |
| `engine.GetBrowserPool().Get(ctx)` / `Put(bw)` | 取 / 还一个浏览器实例（Rod 封装） |
| `engine.FetchRendered(ctx, url, waitSelector)` | 借浏览器渲染页面并等待选择器，返回 `*goquery.Document` + 最终 URL（比手写 Get/Put + NewPage 省事） |
| `engine.GetHTMLClient()` / `engine.FetchHTML(ctx, url)` | 静态 HTML 抓取（goquery），不启动浏览器，适合服务端渲染页 |
| `engine.GetM3U8()` | m3u8 下载器，需在 main.go 先 `SetM3U8` |
| `engine.GetFiledown()` | 文件下载器，需先 `SetFiledown` |
| `engine.GetDB()` | gorm 实例，用于写结果 / 查任务 |
| `engine.Upsert(record, conflictCols, updateCols)` | 冲突更新并回填主键（替代手写 `clause.OnConflict` + `if ID==0` 查回） |
| `engine.GetConfig()` | 全局配置 |
| `engine.SubmitTask(&papa.Task{...})` | 派发子任务（列表页 → 详情页） |
| `engine.SubmitTasks([]*papa.Task{...})` | 批量派发子任务（一次多行 INSERT，减少 DB 往返；撞唯一索引时自动降级为逐条并跳过冲突行，不会让整批失败） |
| `engine.GetStageStats()` | 各阶段的统计快照（监控页读的就是它） |
| `engine.RecordMetric(key, value)` | 写一条业务自定义监控数据，监控页的「自定义数据」模块展示 |
| `engine.AddNotifier(notifier)` | 注册失败告警通知器 |
| `engine.GetProxy()` | 代理管理器 |

### 1.5 写结果到数据库

任务表由框架内置（`crawler_tasks`），结果字段是 `title` 和 `content`（JSON）。fetcher 不直接碰表，而是通过引擎的结果 API 落库：

```go
import (
    "github.com/ydtg1993/papa/v2"
    "yourproject/models" // 你的项目 models 包，papa new 已生成
)

content := models.DetailContent{ Title: "xxx", Cover: "https://..." } // 你自己的结构，定义在 models 包里
err := engine.SaveResult(task.ID, content.Title, content)
```

`content` 可以是任何可 JSON 序列化的结构，框架自动 `json.Marshal` 后写进 `content` 列。业务内容结构（如 `DetailContent`）不在框架里，而是 `papa new` 生成在你自己的 `models` 包里，按需增删字段即可。

结果 API 一览：

| 方法 | 用途 |
| --- | --- |
| `engine.SaveResult(task.ID, title, content)` | 写 `title` + `content`（最常见） |
| `engine.SaveContent(task.ID, content)` | 只写 `content`，保留已有 `title`（回写已有结果用） |
| `engine.GetResult(task.ID, &out)` | 读回 `content` 并反序列化到 `out` |

### 1.6 派发子任务（阶段串联）

列表页抓到详情 URL 后，把详情页作为**子任务**提交到下一个 stage：

```go
err := engine.SubmitTask(&papa.Task{
    PID:   task.ID,       // 关联父任务
    URL:   detailURL,
    Stage: "detail",      // 必须对应 config.yaml 里已配置的 stage
    Meta:  map[string]string{"series_id": "119002"}, // 携带业务键，handler 里读 task.Meta
})
```

去重键默认是 `stage|url`，同一个 URL 重复提交会被引擎识别为**已存在**：单任务 `SubmitTask` 返回 `nil`（视为成功，无需 `_ = err` 忽略），批量 `SubmitTasks` 直接跳过该条——天然防重。需要别的幂等键时设 `IdempotencyKey`。

**延迟投递**：episode 这类要反爬随机间隔的任务，设 `Delay`（或 `NotBefore`），引擎到点才入队，不空占 worker：

```go
engine.SubmitTask(&papa.Task{
    URL:   episodeURL,
    Stage: "episode",
    Delay: time.Duration(10+rand.Intn(21)) * time.Second, // 10–30s 随机
})
```

**批量派发**：一次要投 N 个 episode 时用 `SubmitTasks`（事务化入库，比循环 N 次 `SubmitTask` 更快）：

```go
var tasks []*papa.Task
for _, ep := range episodes {
    tasks = append(tasks, &papa.Task{PID: task.ID, URL: ep.URL, Stage: "episode"})
}
engine.SubmitTasks(tasks)
```

---

## 2. 投喂清单（你写 fetcher 前要给 AI 的信息）

AI 不是神仙，写 fetcher 前请提供以下信息，越具体写得越准：

1. **起始 URL** 和登录态：是否需要 Cookie / 登录？Cookie 从哪来？
2. **页面加载方式**：
   - 服务端渲染（直接抓 HTML 即可）还是 JS 动态渲染（必须用浏览器）？
   - 列表是「点下一页」还是「滚动加载」还是「点击下拉/分类触发」？
3. **数据位置**：列表项的 CSS 选择器、详情链接在哪个标签的哪个属性（`href`）、标题/封面等字段的选择器。
4. **要提取哪些字段**，最终想存成什么结构。
5. **是否需要视频/文件**：是否要抓 m3u8、是否要下载图片/文件，referer / user-agent 是否有特殊要求。
6. **阶段划分**：一个 stage 够不够，还是需要「列表 → 详情 → 视频」多阶段流水。

> 建议你直接把页面「另存为 HTML」或者贴出关键 DOM 片段，AI 写选择器会快很多。

---

## 3. 两种抓取策略：HTMLFetch 与 Rod 浏览器

先根据响应中的数据来源选策略，而不是默认启动浏览器：

| 策略 | 入口 | 适用页面 | 节点获取方式 | 校验重点 | 不适用场景 |
| --- | --- | --- | --- | --- | --- |
| **HTMLFetch（优先）** | `engine.FetchHTML(ctx, task.URL)` | 服务端渲染；页面源码已包含列表、详情字段和链接 | `page.Document.Find/ Text/ Attr`，CSS 选择器由 goquery 执行 | HTTP 非 2xx 已自动返回错误；检查目标选择器匹配数、必填文本、链接属性与 URL 解析 | 数据仅在 JS 执行、滚动或点击接口后出现；需监听网络请求 |
| **Rod 浏览器** | `engine.GetBrowserPool().Get(ctx)` | JS 渲染、登录交互、下拉/滚动懒加载、点击播放并截获 m3u8 | `page.MustElement(s)`、`Attribute`、`MustText`，必要时执行 JS / 监听事件 | 等待页面或目标节点完成渲染；检查节点存在和属性；设置导航超时 | 静态页面；此时浏览器成本高且占用浏览器池 |

HTMLFetch 不启动 Chromium，受 `html.timeout`、`html.max_body_size` 和 `html.headers` 控制；配置文件须保持 `html.enable: true`。Rod 需要同时配置 `browser.enable: true`，并保证 `browser.pool_size`（浏览器并发上限，按需创建）不小于并发使用浏览器的 worker 数量。

### 3.1 HTMLFetch：静态目录页的完整方式

`engine.FetchHTML` 请求并解析 HTML，非 2xx、超时、响应体超过限制和 HTML 解析异常都会返回 `error`，应直接返回给引擎触发重试。成功后从 `page.Document` 获取节点：

```go
page, err := engine.FetchHTML(ctx, task.URL)
if err != nil {
    return fmt.Errorf("fetch catalog html: %w", err)
}

entries := page.Document.Find(`h3 a[href^="/detail/"]`)
if entries.Length() == 0 {
    return fmt.Errorf("catalog selector matched no entries at %s", page.URL)
}

entries.EachWithBreak(func(index int, entry *goquery.Selection) bool {
    title := strings.TrimSpace(entry.Text())
    href, ok := entry.Attr("href")
    if !ok || title == "" || strings.TrimSpace(href) == "" {
        entryErr = fmt.Errorf("catalog entry %d is missing a title or href", index)
        return false
    }

    detailURL, err := page.URL.Parse(href)
    if err != nil {
        entryErr = fmt.Errorf("resolve catalog entry %d URL: %w", index, err)
        return false
    }
    // 保存或派发 detailURL.String()
    return true
})
```

`Document` 的常用 API：

| 需求 | API | 用法 / 返回值 |
| --- | --- | --- |
| 获取重复节点，需逐个处理属性与文本 | `Find(selector)` | 返回 `*goquery.Selection`；用 `Each` / `EachWithBreak` 遍历 |
| 获取第一个节点文本 | `Text(selector)` | `(string, bool)`；`bool=false` 表示节点不存在 |
| 获取全部节点文本 | `Texts(selector)` | `[]string`，每项已 `TrimSpace` |
| 获取第一个节点属性 | `Attr(selector, attribute)` | `(string, bool)`；不可把空字符串当作有效 URL |
| 获取多个节点属性 | `Attrs(selector, attribute)` | `[]string`；适合已确定只有同类节点时批量取值 |
| 需要原始局部标记调试或解析 | `HTML(selector)` | `(string, bool)` |

**节点选择步骤**：先用浏览器的“查看源代码”或保存的原始 HTML 定位页面中实际存在的元素；优先选择业务语义和链接路径共同限定的 CSS 选择器，例如 `h3 a[href^="/detail/"]`，不要使用易变的层级、序号或样式类。若列表项有稳定容器，应先 `Find(".item")`，再在每项内部 `Find("a")`，避免将侧栏、分页或导航的同名链接误收集。

**最小验证集**：

1. `Find` 的结果数量必须大于零；零个节点常表示页面改版、被反爬页替换或内容改为前端渲染，应返回 error 重试并检查响应 HTML。
   **被反爬拦下**有现成的判据（不用每个项目自己写一份）：
   ```go
   page, err := engine.FetchHTML(ctx, task.URL)
   if err != nil { return err }
   // 不可重试，分类 access_restricted；本站词表（SiteSpec.RestrictedKeywords）由框架从 task.Site 取
   if err := engine.RestrictedError(task, page, "catalog"); err != nil { return err }
   ```
   一步就够，不用自己 `engine.Site(task.Site)` 再传 `extra` —— 那条路**漏了不报错**，站点声明里写了
   词表、调用点忘了传，判定只是静默地不生效。stage 传空串则用 `task.Stage`；同一个任务里抓了第二张
   页面（如详情页里的播放页）就传个自己的名字。要自己判、或词表不来自站点声明时，仍可直接用
   `htmlfetch.RestrictedReason(page, extra...)` + `papa.RestrictedPageError`。

   默认词表扫**可见正文 + 标题**（captcha / verify you are human / access denied / 访问受限 / 验证码 /
   Cloudflare 的 checking your browser · just a moment / Google 的 unusual traffic），业务可用
   `SiteSpec.RestrictedKeywords` 追加本站文案。它**不扫整段 HTML** —— 内联脚本与 style 里的
   `captcha` 字面量很常见，扫原始 HTML 会把好页面判成受限页（`RestrictedReason` 内部先摘掉 script/style）。
   词表刻意**宁少勿多**：命中之后通常返回不可重试错误、任务直接判死，假阳性的代价是任务白死。
   拿不准就先只观察：`task.Trace.Warn("疑似受限页", nil, map[string]string{"marker": reason})`。
2. 每一项的标题和 `href` 都必须非空；出现一个残缺条目就返回 error，避免把不完整目录标记为成功。
3. 相对地址用 `papa.ResolveURL(page.URL.String(), href)` 解析成绝对地址：只收 http/https
   （`javascript:`、`mailto:` 这类"链接"进不了任务队列）、去掉 fragment、解析不出 host 当场报错。
   **不要字符串拼接**当前页面 URL。
4. 域名用 `papa.SameHost(base, u)`（apex 与 `www.` 算同一个 host）与 `papa.IsSubdomainOf(u, base)`
   （按**点边界**比对 —— `evil-example.com` 不是 `example.com` 的子域，自己写 `strings.HasSuffix`
   最容易在这儿翻车）校验；路径前缀、ID 格式、条目数下限也在这里一起把关。
5. 将列表快照写入当前 `catalog` 任务；只有已在 `config.yaml` 和 `main.go` 注册 `detail` stage 时，才为通过校验的链接调用 `engine.SubmitTask`。

> **失败时自动留现场**：开了 `crawler.archive`（见 [CORE_CONFIG.md](./CORE_CONFIG.md)）之后，
> `engine.FetchHTML` 抓到的**每一页**都会在本次尝试失败时原样落到
> `{dir}/{stage}/task-{id}-try-{retry}-{urlhash8}.html`，并在后台「追踪」里多一条 `归档页面` 步骤指向它。
> **handler 一行代码都不用改** —— 排查"选择器为什么没匹配上"时，直接打开那个文件看它到底长什么样。
> 落的是**原始字节**（不是 goquery 再序列化的结果），所以 `<noscript>` 里的延迟渲染回退标签也在。

### 3.2 Rod：动态页面或浏览器交互方式

仅当原始 HTML 不含目标节点，或数据必须经 JavaScript、点击、滚动、登录或网络监听才能得到时使用 Rod。浏览器模式的核心是：导航完成后等待目标交互/渲染，随后对 DOM 做与 HTMLFetch 同等严格的节点和属性校验；不要只因一个页面“看起来像网页”就启动浏览器。

```go
bw, err := engine.GetBrowserPool().Get(ctx)
if err != nil {
    return err
}
defer engine.GetBrowserPool().Put(bw)

page := bw.Browser.MustPage("")
defer page.Close()
if err := page.Context(ctx).Timeout(30*time.Second).Navigate(task.URL); err != nil {
    return err
}
page.MustWaitLoad()

items := page.MustElements(".doc-item")
if len(items) == 0 {
    return fmt.Errorf("catalog selector matched no entries")
}
```

> 若只需「借浏览器 → 导航 → 等某个选择器出现 → 拿 HTML/最终 URL」，直接用高层封装 `engine.FetchRendered`，免去手写借还浏览器：
>
> ```go
> doc, finalURL, err := engine.FetchRendered(ctx, task.URL, ".doc-item")
> if err != nil {
>     return err
> }
> // doc 是 *goquery.Document，finalURL 是导航后的最终地址
> ```

需要下拉、滚动或点击时，先完成操作并等待新节点出现，再提取；m3u8 这类只在播放后产生的资源，必须先通过 `EachEvent` 挂好网络监听，再点击播放。Rod 的 `Must*` 方法遇到节点缺失会 panic，因此生产 fetcher 更适合使用返回 `error` 的 API 或预先检查元素存在性，保证错误交给引擎重试。

---

## 4. 场景一：JS 下拉加载 → 抓列表路由 + 详情信息

> 脚手架默认只生成单阶段 `catalog`。本节演示怎么扩成「列表 → 详情」两阶段 —— 写第二个 fetcher，在 `configs/sites/<站名>.go` 里加一段 `StageSpec` 即可（main.go 不用动）。

典型例子：目录页要点「展开分类」下拉、滚动到底部触发懒加载，才能拿到所有文档的标题和链接。

### 3.1 在站点声明里加两个阶段

```go
// configs/sites/mysite.go
Stages: []papa.StageSpec{
    {
        Fetcher:     &fetcher.FetchCatalog{}, // 列表页阶段
        WorkerCount: 1,
        QueueSize:   20,
        Delay:       "5m",
        Retry:       papa.RetrySpec{MaxAttempts: 3, Backoff: "30s"},
        AutoStart:   true,
    },
    {
        Fetcher:     &fetcher.FetchDetail{}, // 详情页阶段
        WorkerCount: 3,
        QueueSize:   500,
        Delay:       "3m",
        Retry:       papa.RetrySpec{MaxAttempts: 3, Backoff: "30s"},
    },
},
```

### 3.2 main.go

不用动 —— 各站点文件在自己的 `init()` 里登记，框架用 `papa.Sites()` 收集（见 7.2）。

### 3.3 catalog fetcher（列表页：点下拉 + 滚动 + 派发）

```go
package fetcher

import (
    "context"
    "strings"
    "time"

    "github.com/go-rod/rod"
    "github.com/ydtg1993/papa/v2"
)

type FetchCatalog struct{}

func (f *FetchCatalog) GetStage() string { return "catalog" }

func (f *FetchCatalog) FetchHandler(ctx context.Context, task *papa.Task, engine *papa.Engine) error {
    bw, err := engine.GetBrowserPool().Get(ctx)
    if err != nil {
        return err
    }
    defer engine.GetBrowserPool().Put(bw)

    page := bw.Browser.MustPage("")
    defer page.Close()

    if err := page.Context(ctx).Timeout(30*time.Second).Navigate(task.URL); err != nil {
        return err
    }
    page.MustWaitLoad()

    // 1. 点击「展开分类」下拉，加载全部列表项
    if el, err := page.Element(".dropdown-trigger"); err == nil {
        el.MustClick()
        page.MustWaitLoad()
    }

    // 2. 滚动到底部触发懒加载，直到没有新内容（最多 N 次）
    for i := 0; i < 10; i++ {
        page.MustEval(`() => window.scrollTo(0, document.body.scrollHeight)`)
        time.Sleep(500 * time.Millisecond)
    }

    // 3. 遍历列表项，提取标题 + 详情链接
    items := page.MustElements(".doc-item")
    for _, item := range items {
        title := item.MustElement(".doc-title").MustText()
        href, err := item.MustElement("a").Attribute("href")
        if err != nil || href == nil {
            continue
        }
        detailURL := *href
        // 相对路径补全为绝对 URL
        if strings.HasPrefix(detailURL, "/") {
            info, _ := page.Info()
            detailURL = strings.TrimRight(info.URL, "/") + detailURL
        }

        // 4. 派发详情子任务
        if err := engine.SubmitTask(&papa.Task{
            PID:   task.ID,
            URL:   detailURL,
            Stage: "detail",
        }); err != nil {
            // 去重命中不会走到这里 —— SubmitTask 返回 nil（视为成功）；
            // 真返回 error 说明是别的提交问题，别吞掉
            return err
        }
        _ = title
    }
    return nil
}
```

> `rod.Page` 的常用操作：`MustElements(sel)` 取多个、`MustElement(sel)` 取单个、`MustClick()`、`MustText()`、`Attribute(name)`、`MustEval(js)`、`MustWaitLoad()`。完整 API 见 go-rod 文档。

### 3.4 detail fetcher（详情页：提取字段 + 写库）

```go
package fetcher

import (
    "context"
    "time"

    "github.com/ydtg1993/papa/v2"
    "yourproject/models"
)

type FetchDetail struct{}

func (f *FetchDetail) GetStage() string { return "detail" }

func (f *FetchDetail) FetchHandler(ctx context.Context, task *papa.Task, engine *papa.Engine) error {
    bw, err := engine.GetBrowserPool().Get(ctx)
    if err != nil {
        return err
    }
    defer engine.GetBrowserPool().Put(bw)

    page := bw.Browser.MustPage("")
    defer page.Close()

    if err := page.Context(ctx).Timeout(30*time.Second).Navigate(task.URL); err != nil {
        return err
    }
    page.MustWaitLoad()

    title := page.MustElement("h1").MustText()
    coverURL, _ := page.MustElement(".cover img").Attribute("src")

    content := models.DetailContent{
        Title: title,
        Cover: deref(coverURL),
        // ... 其他字段按需扩展
    }
    return engine.SaveResult(task.ID, title, content)
}

func deref(s *string) string {
    if s == nil {
        return ""
    }
    return *s
}
```

---

## 5. 场景二：详情页点击播放 → 捕获 m3u8 → 下载

典型例子：详情页要「点播放按钮」才发起视频请求，m3u8 地址不在 DOM 里，只能从网络请求里截获。

### 4.1 关键点：先挂监听，再点播放

m3u8 地址是点播放后才产生的网络请求，所以**必须在导航后、点击前**用 Rod 的 `EachEvent` 监听网络请求：

```go
package fetcher

import (
    "context"
    "strings"
    "sync"
    "time"

    "github.com/go-rod/rod/lib/proto"
    "github.com/ydtg1993/papa/v2"
    "github.com/ydtg1993/papa/v2/pkg/middleware/m3u8"
    "yourproject/models"
)

type FetchVideo struct{}

func (f *FetchVideo) GetStage() string { return "video" }

func (f *FetchVideo) FetchHandler(ctx context.Context, task *papa.Task, engine *papa.Engine) error {
    bw, err := engine.GetBrowserPool().Get(ctx)
    if err != nil {
        return err
    }
    defer engine.GetBrowserPool().Put(bw)

    page := bw.Browser.MustPage("")
    defer page.Close()

    if err := page.Context(ctx).Timeout(30*time.Second).Navigate(task.URL); err != nil {
        return err
    }
    page.MustWaitLoad()

    // 1. 监听网络请求，捕获 m3u8 地址（必须在点击播放前挂上）
    var mu sync.Mutex
    var m3u8URL string
    wait := page.EachEvent(func(e *proto.NetworkRequestWillBeSent) {
        if strings.Contains(e.Request.URL, ".m3u8") {
            mu.Lock()
            if m3u8URL == "" {
                m3u8URL = e.Request.URL
            }
            mu.Unlock()
        }
    })()

    // 2. 点击播放按钮，触发视频请求
    page.MustElement(".play-btn").MustClick()
    page.MustWaitLoad()
    time.Sleep(2 * time.Second) // 等 m3u8 请求发出
    wait()

    mu.Lock()
    url := m3u8URL
    mu.Unlock()
    if url == "" {
        return nil // 或返回 error 触发重试
    }

    // 3. 交给 m3u8 下载器（main.go 里要先 SetM3U8）
    dl := engine.GetM3U8()
    if dl == nil {
        return nil
    }
    // OptionsFromRequest：把这次抓取上下文里的站点级/逐请求头与显式代理带进下载选项。
    // 下载器**不读 ctx**（隐式继承会让人不知道请求上到底带了什么）—— 想用就显式这一步，
    // 不想用就自己造一份 &m3u8.DownloadOptions{Referer: task.URL}（多数 m3u8 站点校验 referer）。
    res := dl.Download(ctx, url, task.Stage, "video.ts", m3u8.OptionsFromRequest(ctx, task.URL))
    if res.Error != nil {
        return res.Error
    }

    // 4. 写库（记录 m3u8 地址和本地输出；VideoContent 是你在 models 包里自定义的结构）
    // res.OutputFile 是**相对** dl.OutputDir() 的路径，要落一个能直接指到文件的本地路径就拼起来。
    content := models.VideoContent{ Dir: filepath.Join(dl.OutputDir(), res.OutputFile), Source: url }
    return engine.SaveContent(task.ID, content)
}
```

### 4.2 下载器接线（main.go）

```go
import "github.com/ydtg1993/papa/v2/pkg/middleware/m3u8"

cfg := m3u8.DefaultConfig()
cfg.OutputDir = "./downloads/video"      // 输出目录
cfg.AutoMerge = false                    // 不转码，直接拼 TS（要 mp4 则保持 true 并装 ffmpeg）
app.Engine.SetM3U8(m3u8.NewDownloader(cfg))
```

m3u8 下载器能力：并发下载片段、AES-128 解密、断点续传、限速、合并（`concatTSFiles` 或 ffmpeg 转 mp4）。`Download` 是同步阻塞的，返回 `DownloadResult{OutputFile, Segments, Size, Error}`。

### 4.3 静态页 / 普通文件用 filedown

不需要浏览器、直接下载文件（图片、附件等）时用 `filedown`：

```go
import "github.com/ydtg1993/papa/v2/pkg/middleware/filedown"

dl := engine.GetFiledown()
// OptionsFromRequest：站点级 + 逐请求头、显式代理一起带上（同 m3u8，下载器自己不读 ctx）
opts := filedown.OptionsFromRequest(ctx, task.URL)
opts.Direct = true // 封面/附件这类几十~几百 KB 的文件：一次 GET 落盘，省掉 HEAD 探测与分片那套机器
res := dl.Download(ctx, fileURL, "images", "cover.jpg", opts)
if res.Error != nil {
    return res.Error
}

// 本地路径 = 下载器的输出根 + 相对路径。**别自己再写一个常量去对齐 main.go 里的 OutputDir**：
// 两处一旦漂了不报错，只表现为"文件下下来了，但库里记的路径指空"。
localPath := filepath.Join(dl.OutputDir(), res.OutputFile)
```

头里的空值表示删掉这个头（与抓取路径同一套语义）。

注意 `OptionsFromRequest` **只带 ctx 上的两层**：站点级（`SiteSpec.Headers`）与逐请求（`papa.WithHeaders`）。
全局的 `html.headers` / `browser.headers` 不在里面（那是抓取客户端的默认层）—— 要让下载跟抓取用同一套头
（UA 尤其，图片/视频站常查），**把该键写进 `SiteSpec.Headers`**，两条路就都继承到了。

`DownloadResult` 还带 `ContentType`（响应声明的类型）：站点用图片代理按 `Accept` 协商输出格式时
（源图 webp、只声明 `image/*` 就回 jpeg），拿它或按文件头复核一下再决定落盘后缀，别让"叫 `.webp` 的 jpeg"进库。

---

## 6. 最小可跑骨架（脚手架已生成）

`papa new <name>` 会生成好 main.go / fetcher/fetch_catalog.go / models（建表清单 models.go）/ monitor（后台分层：表格页、控制器、视图、路由与中间件）/ **configs/sites/（一站一个文件 + 注册表）** / configs/config.yaml / docker / docs / Makefile / logs，你只需把 fetcher 里的 TODO 换成真实逻辑。

其中 main.go 的阶段注册只有一行（声明在 `configs/sites/<站名>.go`）：

```go
app.RegisterSites(papa.Sites()...)
```

生成后的 fetcher 长这样（`SubmitEntries` 是入口任务，见 7.1）：

```go
package fetcher

import (
	"context"
	"time"

	"github.com/ydtg1993/papa/v2"
)

// FetchCatalog 阶段一：抓取目录/列表页。
// 职责：打开目标页 → 提取标题 + 详情链接 → 派发 detail 子任务。
type FetchCatalog struct{}

func (f *FetchCatalog) GetStage() string { return "catalog" } // 阶段名，全程序唯一（多站带站点前缀）

func (f *FetchCatalog) FetchHandler(ctx context.Context, task *papa.Task, engine *papa.Engine) error {
	bw, err := engine.GetBrowserPool().Get(ctx)
	if err != nil {
		// 步骤追踪（crawler.trace.enabled 开启时生效）：失败时带上现场数据，后台「追踪」里能直接看到。
		// task.Trace 关闭时是 nil，这些调用都是安全的 no-op —— 不需要判空。
		task.Trace.Fail("取浏览器实例", err, nil)
		return err
	}
	defer engine.GetBrowserPool().Put(bw)

	page := bw.Browser.MustPage("")
	defer page.Close()

	if err := page.Context(ctx).Timeout(30*time.Second).Navigate(task.URL); err != nil {
		task.Trace.Fail("打开列表页", err, map[string]string{"url": task.URL})
		return err
	}
	page.MustWaitLoad()
	// Step 表示「这一步已经做完了」，耗时按距上一步的间隔算
	task.Trace.Step("打开列表页", map[string]string{"url": task.URL})

	// TODO: 遍历列表项，提取标题和详情链接，然后派发子任务：
	// engine.SubmitTask(&papa.Task{PID: task.ID, URL: detailURL, Stage: "detail"})

	return nil
}
```

> `task.Trace.*` 是**框架的步骤上报接口**（见 1.3），关掉追踪时是 no-op —— 写新 fetcher 时照抄即可。

配套两处改动：
1. `configs/sites/<站名>.go` 里加一段 `StageSpec`（worker_count / queue_size / delay / retry / 要不要 AutoStart）。
2. 若用下载器，`main.go` 里先 `SetM3U8` / `SetFiledown`（要在 `RegisterSites` 之前 —— 池子在那时就把实例绑好了）。

---

## 7. 如何驱动任务（提交 + 监控）

Papa 没有 MCP 了，任务驱动靠四处：

1. **初始任务**：让 fetcher 自己实现 `SubmitEntries(*papa.Engine)`（见 1.1 的可选接口），框架在启动时自动调它：

```go
// fetcher/fetch_catalog.go
func (f *FetchCatalog) SubmitEntries(engine *papa.Engine) {
    if err := engine.SubmitTask(&papa.Task{
        URL:        "https://example.com/classify?type=rexue", // 起始 URL 写在这儿
        Stage:      "catalog",
        Repeatable: true,
    }); err != nil {
        engine.GetLoggerSet().Engine.Errorf("submit initial task: %s", err.Error())
    }
}
```

> 它跑在**所有阶段的池子都建好之后**，所以可以往任意阶段投递（不限于自己这个阶段）：
> 比如把多个站点的入口分别交给各自的阶段。调用顺序按阶段名字典序，启动期提交顺序是稳定的。
>
> **要不要投、由声明说了算**：`StageSpec.AutoStart`（`true` = 启动时投）。排查时只想跑某个阶段
>（不想让入口阶段又跑一遍）就把它设成 `false` —— 它只管启动时投不投入口，阶段本身照常跑。
> 实现了入口却没开 AutoStart 会打一条 Info（"是关的、不是漏的"）。
>
> **站点与阶段声明在一个文件里**：`configs/sites/<站名>.go`（并发/队列/间隔/重试/入口开关/
> 站点归属/熔断阈值都在那一处，`configs/sites/sites.go` 只做汇总），`main.go` 只要一行 `app.RegisterSites(papa.Sites()...)`。
> 加阶段 = 写 fetcher + 在 `Sites()` 里加一段。阶段名取自 `GetStage()`，重名**启动就报错**；
> 多站时阶段名要能区分（约定带站点前缀，如 `hgd_catalog` / `siteb_catalog`）。

### 7.2 多站：一站一个文件（框架自己收集）

一站一个文件放进 `configs/sites/`（同一个包），**每个文件在自己的 `init()` 里登记** ——
框架把它们收在 `papa.Sites()` 里，**项目里没有汇总清单要维护**：

```
configs/
  config.yaml
  whitelist
  sites/
    huangguo.go             // 一站一份：Key/BaseURL/Breaker/Stages + func init() { papa.RegisterSite(huangguo()) }
    huangguo2.go            // 加一个站 = 加一个文件（改 Key/BaseURL/阶段参数）
```

```go
// main.go
import (
    _ "yourmod/configs/sites"   // 匿名导入：让各站点文件的 init 跑起来（它们自己登记）
)
app.RegisterSites(papa.Sites()...)
```

`configs/` 根目录仍只放数据（config.yaml / whitelist），Go 包单独一层。
**框架不需要"站点包"这个概念** —— `SiteSpec` 就是普通值类型；为什么必须"文件自己登记"：
Go 没有"枚举一个包里有哪些函数/类型"的能力（反射只能从值倒推名字），所以"框架自动发现"只有
代码生成或代码自登记两条路，这里选后者（零构建步骤、可 grep，也是 `database/sql` 驱动那套标准做法）。
登记顺序 = **文件名字典序**（Go 规范：同包 init 按文件名执行），稳定可复现；站点键/阶段名重复都会在启动时报错。

main.go 仍然一行：`app.RegisterSites(papa.Sites()...)`。**框架不需要"站点包"这个概念** —— `SiteSpec` 就是普通值类型，怎么组织是你项目的事。

三条要注意的：

- **阶段名跨站仍要唯一**（如 `hgd_catalog` / `siteb_catalog`）—— 重名会在**启动时 panic** 并提示；前缀不是强制的，能区分就行。
- **别把 Go 包放进 `configs/`**：那是数据目录（config.yaml / whitelist）。声明与 fetcher 放 `sites/`（或 `targets/`）。
- **fetcher 持有自己那一站的站点值**（`type Catalog struct{ site *Site }`），handler 里就没有站点字面量了；`GetStage()` 用同一份键拼阶段名，一处改处处跟着。

已按站切开的东西：每站每阶段一个池子（并发/间隔/重试各自独立）、**每站一把熔断闸门**（后台横幅逐站一行、可逐站放行）、任务表 `site` 列（引擎按目标阶段自动填，跨站派发自然落到目标站）、后台任务表的「站点」列与筛选、告警事件带 `site`、`engine.Site(task.Site)` 取站点声明。

**请求头按站切开了**：`SiteSpec.Headers` —— 这个站的任务抓任何页面都自动带上（静态抓取与浏览器渲染都认），
同键覆盖 `html.headers` / `browser.headers`，空值表示删掉那个头；handler 里对**单次请求**还能
`engine.FetchHTML(papa.WithHeaders(ctx, map[string]string{...}), url)` 再覆盖（叠加，只覆盖给到的键）。
优先级：框架默认 < 全局 headers 配置 < 站点 headers < 逐请求。
站点还有一项 `RestrictedKeywords`：本站自己的"受限页"文案，追加到上面那个默认词表之后。

**代理：出口由 fetcher 随用随取**，框架不替业务决定 —— 与 `GetFiledown()` / `GetM3U8()` 那套"按需取"一致：

```go
// 取一个（配了 proxy.api_url 就是那个池子；没配/池子空 → 返回空串）
if addr := engine.NextProxy(); addr != "" {
    page, err = engine.FetchHTML(papa.WithProxyURL(ctx, addr), url)   // 静态：显式出口
}
res := engine.GetFiledown().Download(ctx, coverURL, dir, name, &filedown.DownloadOptions{
    Proxy: engine.NextProxy(),   // 下载器：逐次下载指定出口（同一地址复用客户端）
})
```

`core.WithProxyURL` 比 `htmlfetch.WithProxy(ctx, use)`（走池/直连）更具体，优先于它。
**浏览器路径不支持逐请求代理地址** —— 代理是浏览器实例级设置（启动参数），Chrome 不提供逐请求改路由，
所以传了会**明确报错**（而不是静默直连、让人以为走了代理）；要按站换出口就用池上那套（proxied / direct 两类实例）
或一站一进程。

2. **阶段间串联**：fetcher 里用 `engine.SubmitTask(&papa.Task{PID: task.ID, URL: ..., Stage: "detail"})` 派发子任务（见 1.5）。

3. **定时重抓 / 恢复失败**：
   - 定时轮询：业务用 `app.RegisterCronJob("repeat_daily", "0 0 8 * * *", func(){ app.Engine.RepollRepeatableTasks() })` 注册，每日重跑 `repeatable: true` 的轮询任务；
   - 中断恢复：`recover_queue` 配置开启后，**启动时**把「未到终态」（pending/processing）的任务全部重新入队 —— 进程刚起，这些就是上次中断留下的孤儿，不用按时间猜。详见 [RECOVER_QUEUE.md](./RECOVER_QUEUE.md) 与 [SCHEDULER.md](./SCHEDULER.md)。

4. **失败任务再处理**：`error_queue` 配置开启后，`failed` 任务会被自动（`interval` 轮询）或手动（后台「队列治理」模块里该队列的「立即执行」按钮 / `POST /api/errorqueue/process`）重新投递，带 `max_retry` 再处理代数上限。详见 [ERROR_QUEUE.md](./ERROR_QUEUE.md)。

**看状态**：启动后打开监控页 `http://localhost:9090/monitor`（`server.enabled: true`），OA 后台布局：Dashboard 看机器 CPU/内存/磁盘、业务目录（downloads/logs）占用与任务队列概览，另有「任务队列」「自定义数据」模块。登录用**访问令牌**（`papa token add --operator <名字>` 创建，库里只存哈希）；可用 `server.whitelist` 限制来源 IP。fetcher 里可调 `engine.RecordMetric("key", value)` 写入自定义展示数据。

---

## 8. 常见坑

1. **阶段名重复** → `RegisterSites` 直接 panic 并点出是哪两个站声明了同一个名字。多站一律带站点前缀（`hgd_catalog` / `siteb_catalog`），别靠"能区分"碰运气。
2. **浏览器池耗尽**：`browser.pool_size`（浏览器并发上限）要 ≥ 各 stage `worker_count` 之和，否则 worker 会阻塞在 `pool.Get`。
3. **m3u8 需要 referer/cookie**：多数 m3u8 站点校验 referer，用 `m3u8.OptionsFromRequest(ctx, task.URL)` 一步带上（站点头/逐请求头/代理一起过来），或自己写 `m3u8.DownloadOptions{Referer: ...}`。
3b. **下载器不继承抓取的头**：`SiteSpec.Headers` / `papa.WithHeaders` 只作用于**抓取**（静态 + 浏览器），下载（filedown / m3u8）要自己带 —— 用 `OptionsFromRequest(ctx, referer)`，它不会自动发生。
4. **懒加载**：滚动加载别只滚一次，循环滚到底 + 等待，直到没有新元素。
5. **相对链接**：`href`/`src` 可能是相对路径，用 `page.Info().URL` 拼成绝对 URL 再提交任务。
6. **重试语义**：fetcher 返回普通 error 会触发重试；对「确实失败、重试无意义」的（页面 404、缺字段、验证码拦截等），返回 `papa.WrapNoRetryKind("structure", err)`（或 `WrapNoRetry(err)`），引擎不重试、直接标 failed 并告警，别再「返回 nil 假装成功」。
7. **写库用 task.ID**：子任务派发后，每个 fetcher 只写自己这个 `task.ID` 的记录。
8. **ffmpeg**：`AutoMerge: true` 转 mp4 需要系统装 ffmpeg；只想拼 TS 就 `AutoMerge: false`。
9. **延迟投递**：反爬随机间隔用 `task.Delay`（或 `NotBefore`）在派发时设置，别在 handler 里 `time.Sleep` 空等，那会浪费 worker 并发位。
10. **别重复记日志**：`FetchHandler` 的 error 和 `SubmitTask` 的提交错误框架都会自动记，fetcher 里**不必再** `logger.Errorf` 记一遍，否则会刷双份。你只需判断 error 要不要改变控制流（继续/中止/告警）。

---

## 9. API 速查

**go-rod（页面操作）**
- `bw.Browser.MustPage("")` 建空白页；`page.Close()` 关页。
- `page.Context(ctx).Timeout(d).Navigate(url)` 带取消+超时导航；`page.MustWaitLoad()`。
- `page.MustElement(sel)` / `page.MustElements(sel)`；`el.MustClick()` / `el.MustText()` / `el.Attribute(name)`。
- `page.MustEval(js)` 执行 JS（滚动、取全局变量等）。
- `page.EachEvent(func(e *proto.NetworkRequestWillBeSent){...})()` 监听网络请求（挂上后返回 wait 函数）。

**静态 HTML（goquery）**
- `engine.FetchHTML(ctx, url)` → `*htmlfetch.Page`；`page.Document.Text/Texts/Attr/Attrs/HTML(selector)`。
- `page.Selection()`（= `page.Document.Selection()`）→ `*goquery.Selection`：要写复杂解析
  （`Find(...).Each`、Children/Parent/Siblings…）就拿这个根节点。解析函数统一收 `*goquery.Selection`，
  线上传 `page.Selection()`、测试传 `goquery.NewDocumentFromReader(...).Selection` —— 同一个类型，
  不用为"框架的文档 vs 测试的文档"再包一层接口。
- 链接与域名：`papa.ResolveURL(base, ref)` / `papa.SameHost(a, b)` / `papa.IsSubdomainOf(child, parent)`。
- 受限页：`engine.RestrictedError(task, page, stage)`（站点词表自动带上，不可重试）。

**下载器**
- m3u8：`NewDownloader(cfg)` / `Download(ctx, url, outDir, outFile, opts...) → DownloadResult`。
- filedown：`NewDownloader(cfg)` / `Download(ctx, url, outDir, fileName, opts...) → DownloadResult`。
- 两个下载器都有 `Downloader.OutputDir()`：`DownloadResult.OutputFile` 是**相对**它的路径，落库的本地路径靠它拼。
- 两个下载器都有 `OptionsFromRequest(ctx, referer)`：把抓取上下文里的站点级/逐请求头与显式代理转成下载选项
  （下载器不读 ctx，用不用由你显式决定）；`filedown.DownloadOptions.Direct` 走单次 GET（小文件）。

**数据库 / 结果落地**
- `engine.SaveResult(task.ID, title, content)` 写 `title` + `content`。
- `engine.SaveContent(task.ID, content)` 只写 `content`（保留 `title`）。
- `engine.GetResult(task.ID, &out)` 读回 `content` 反序列化到 `out`。
- `engine.Upsert(record, conflictCols, updateCols)` 冲突更新并回填主键（独立表用）。
- `engine.GetDB()` 拿 gorm 实例（建独立表、复杂查询时用）。

**任务派发 / 错误分类 / 告警**
- `engine.SubmitTask(&papa.Task{...})` 派发单个子任务；`engine.SubmitTasks([]*papa.Task{...})` 批量派发。
- `papa.WrapNoRetry(err)` / `papa.WrapNoRetryKind(kind, err)` 标记不可重试错误。
- `papa.Retryable(err)` / `papa.ErrorKind(err)` 判断错误是否可重试 / 取其分类。
- `engine.AddNotifier(notify.NewWebhook(url))` 注册失败告警；`notify` 在 `pkg/notify`。熔断触发时也会经这条通道发一条 `papa.AlertCritical`（级别比 `papa.AlertError` 高一级，可单独路由）。
- `engine.PauseCrawling(reason)` / `engine.ResumeCrawling()` / `engine.BreakerStatus()`：手动闸住/放行抓取、读熔断状态。配 `crawler.breaker` 可由框架自动触发（见 [CORE_CONFIG.md](./CORE_CONFIG.md)）。
- `engine.FetchRendered(ctx, url, waitSelector)` 借浏览器渲染并返回 `(*goquery.Document, finalURL, error)`。
- `engine.ProcessErrorQueue()` 立即把失败任务重新投递（返回处理条数）；配 `error_queue` 可自动轮询或 OA 手动触发。
