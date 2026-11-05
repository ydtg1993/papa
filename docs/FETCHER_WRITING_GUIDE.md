# Papa Fetcher 书写手册（路径 B：AI 直接书写 Go fetcher）

> 面向人群：要把一个新目标站点接入 Papa，让 AI 帮你写抓取逻辑的你。
>
> 这份手册回答一件事：**给定一个目标网站，如何把它变成 Papa 里能跑起来的 fetcher**。
> 本文聚焦 Go Fetcher；声明式 HTML/JSON 规则、离线样本验收与内存版本切换见 [CRAWL_RULES.md](./CRAWL_RULES.md)，通用联网 RuleFetcher 尚未提供。
>
> 抓取结果如何落库、怎么生成 model，见 [DATA_LANDING_GUIDE.md](./DATA_LANDING_GUIDE.md)。

---

## 0. 一句话工作流

```text
papa new <project-name>    # 用脚手架生成新项目骨架（main.go / fetcher / models / monitor / config / docker / docs / Makefile / logs）
  -> 你投喂目标站点，AI 在生成的 fetcher/fetch_catalog.go 里填抓取逻辑（只 import github.com/ydtg1993/papa/v3）
  -> 在 sites/<站名>.go 里声明阶段（并发/队列/间隔/重试）与该阶段的 fetcher，起始 URL 由 fetcher 的 SubmitEntries 提交
  -> 任务被 worker 池调度执行，结果通过 Upsert 写入业务表，crawler_tasks 只维护调度状态
```

整个过程 AI 不修改框架代码，只在你自己的项目里新增/修改 fetcher 与**站点声明**（`sites/<站名>.go`：阶段参数 + 入口开关都在那儿），`config.yaml` 只放全局配置（如 `db.dsn`、`browser.enable`）。

---

## 1. 核心契约（AI 必须遵守的边界）

### 1.1 Fetcher 接口

框架对外是一个门面包 `github.com/ydtg1993/papa/v3`，fetcher 只需 import 它：

```go
type Fetcher interface {
    GetStage() string
    FetchHandler(ctx context.Context, task *Task, engine *Engine) error
}
```

- `GetStage()` 返回阶段名，它在**本站内必须唯一**（两个站可以各有自己的 `catalog`：阶段键是 `site:stage`，各跑各的池子与统计）。同一个站里重名、缺 `WorkerCount`/`QueueSize`、`Delay` 写错都会在 `RegisterSites` 时 panic 并点名 —— 阶段的存在性与参数都在 `sites/<站名>.go` 的声明里（见 7.2）。
- **可选**：fetcher 还可以实现两个接口，都是"实现一个方法就自动生效"，不实现也不影响运行：
  ```go
  // 声明本阶段要借浏览器 → 框架在启动时校验接线（没开 config.yaml 的 browser.enable 就直接 panic，
  // 而不是等第一条任务跑到那一步）
  func (f *FetchVideo) NeedsBrowser() bool { return true } // 会借浏览器（engine.CaptureRequest / FetchRenderedPage / GetBrowserPool）

  // 入口任务："注册即投一批起始任务" → 框架在启动时自动调（那时所有阶段的池子都已建好）
  // site = 本站声明快照（SiteSpec → core.Site）：BaseURL / Headers 都在里面
  func (f *FetchCatalog) SubmitEntries(engine *papa.Engine, site papa.Site) { /* engine.SubmitTask(...) */ }
  ```
  曾经还有 `NeedsFiledown()` / `NeedsM3U8()` 两个"声明要用下载器"的接口，已随下载器全局配置面一起删除 ——
  框架一定按 `DefaultConfig()` 备好下载器基座，"声明了却没接线"在结构上不可能发生，那两条校验成了永不触发的死代码。
  老 fetcher 上若还留着这两个方法，它们只是普通方法、不会再被调用（删不删都行）。
- `FetchHandler` 返回 `nil` → 引擎把任务标记为 `success`；返回普通 `error` → 引擎按该阶段声明里的 `Retry.MaxAttempts` / `Retry.Backoff`（见 7.2）自动重试，最终失败标记为 `failed`。
- 返回 `papa.WrapNoRetry(err)` 或 `papa.WrapNoRetryKind(kind, err)` → 引擎**不重试**，直接把任务标 `failed` 并触发告警，适合「结构错误 / 404 / 访问受限」这类重试无意义的失败。
- 用 `engine.FetchHTML` 抓静态页时，**判失败原因别看错误文本**：非 2xx 是 `*htmlfetch.StatusError`（`htmlfetch.StatusCode(err)` 直接取码），响应体超 `html.max_body_size` 是 `*htmlfetch.BodyTooLargeError`。文本匹配会误伤 —— 把 `max_body_size` 配成 `4040000` 时，"html response exceeds 4040000 bytes" 里就带着 `404`，一个该重试的错误会被判成"不可重试的 not_found"。
  ```go
  if code, ok := htmlfetch.StatusCode(err); ok && (code == 404 || code == 410) {
      return papa.WrapNoRetryKind("not-found", err)
  }
  ```
- **不要**在 fetcher 里自己调 `task.UpdateStatus`，状态由引擎自动维护；你只负责提取结果并写库。
- **认领与终态必须确认落库**：认领数据库错误时不执行 Fetcher；状态操作以 100ms 起步、最多 2s 间隔退避重试，等待可取消并占用当前 worker 形成背压。业务完成后只重试终态写入，不重跑业务；确认终态前不发送完成事件。取消或认领回执不明确的任务计入关停未完成统计，需依靠后续恢复处理。这不提供跨进程租约或业务 exactly-once，进程崩溃后的恢复仍要求业务写入幂等。
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
    IdempotencyKey string            // 自定义幂等键，规范格式 site:stage:<标识>；空则回退 site:stage:url
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
- `IdempotencyKey` 自定义去重键，**规范格式 `site:stage:<标识>`**（如 `shop:catalog:https://shop.example/books`、`shop:detail:series:5`），空值回退成同样形状的 `site:stage:url`。三段缺一不可：少了 `site`/`stage`，两个站的同名阶段会撞成同一条任务（B 站被判"已存在"、静默不跑）。
  唯一性由**数据库**兜底：唯一索引建在 `idempotency_hash`（键原文的 sha256）上，同一个自定义键在库里只会有一行，并发也插不进第二行；没自定义键的行该列落 NULL（不是空串）。键原文上限 1000 字符，第三段用规范化后的短 URL。
- `NotBefore` / `Delay` 实现延迟投递：任务到点才入队，不空占 worker（反爬要随机间隔时设 `Delay` 即可，别在 handler 里 `time.Sleep`）。
- `Repeat` 只读提交轮次计数：从 `crawler_tasks.repeat` 还原，可轮询任务每轮成功写回 pending 时递增一次；同轮因队满或池回收反复回灌不再递增。不是成功抓取次数，也不是仅由 repeat_queue 累加。禁止用赋值方式重置数据库计数。
- `StageSpec.RepeatLimit` 可对某站某阶段设置轮询上限，例如 detail 设 30、catalog 保持 0（不限）。`repeat >= 30` 后仅 repeat_queue 停止重投，恢复/错误队列和后台单条重投仍可用。详见 [REPEAT_QUEUE.md](./REPEAT_QUEUE.md)。
- `Repeatable` 周期轮询：`true` = 这条任务跑完后**留在库里**，由 `repeat_queue` 在**到点**时再捞起来重跑；`false`（默认）= 跑完就不再重投。它管的是**这条任务**，不是阶段 —— 同一个阶段里分类页可以是轮询的、详情页是一次性的（该给谁 `true` 由派发它的那处代码决定）。配套的 `RepeatInterval`（如 `10 * time.Minute`）就是它的周期；不写（0）= 用全局 `repeat_queue.interval`。两者都只在**首次落库**时播种，之后用后台的「开轮询」/「停轮询」/「设轮询周期」或 `engine.SetTaskRepeatable(id, on)` / `engine.SetTaskRepeatInterval(id, was, seconds)` 改（见 [REPEAT_QUEUE.md](./REPEAT_QUEUE.md) 第 3 节）。
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

	// 结果落业务自己的表（见 §1.5）：框架不替你决定表结构
	return engine.Upsert(&models.Series{SourceKey: detail.Key, Title: detail.Title},
		[]string{"source_key"}, []string{"title"})
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
- **框架写的失败现场步骤**：用 `engine.CaptureRequest` 时失败会写「捕获网络请求」（data 里有候选请求样本）；用 `FetchRenderedPageWith` 等等待条件失败会写「等待渲染」（卡住的 selector / js / request-idle）。两者后面都会跟「归档页面」（若开了归档），抽屉里可直接下载那一页。

### 1.4 Engine 暴露的能力

fetcher 里通过 `engine` 参数能拿到的东西：

| 方法 | 用途 |
| --- | --- |
| `engine.GetBrowserPool().WithBrowser(ctx, fn)` / `WithDirectBrowser` | 借实例执行回调，成功 / 报错 / panic 都保证归还（**推荐**；底层 `Get(ctx)` / `Put(bw)` 仍可用） |
| `bw.WithPage(ctx, fn)` / `bw.WithNavigatedPage(ctx, url, opts, fn)` | 页面生命周期托管：返回后保证关页 |
| `engine.CaptureRequest(ctx, url, browser.CaptureOptions{...})` | 导航前挂监听、截第一条匹配的网络请求；失败现场自动进归档 + trace（见 §5） |
| `engine.FetchRendered(ctx, url, waitSelector)` | 借浏览器渲染页面并等待选择器，返回 `*goquery.Document` + 最终 URL（比手写 Get/Put + NewPage 省事） |
| `engine.FetchRenderedPage(ctx, url, waitSelector)` | 同上，但返回 `*htmlfetch.Page`（与 `FetchHTML` 同一种）—— 走浏览器的阶段要接着做受限页判定、或按 `page.Selection()` 解析时用它 |
| `engine.FetchRenderedPageWith(ctx, url, browser.PageOptions{...})` | 同上，另支持 `WaitSelector` / `WaitJS` / `WaitRequestIdle` / `Block`（按页拦资源） |
| `engine.FetchHTTP(ctx, papa.HTTPRequest{...})` | 通用 HTTP/API、限长响应与 JSON 解码，见 §3.3 |
| `engine.CaptureResponse(ctx, url, browser.ResponseOptions{...})` | 捕获浏览器 API 响应，见 §3.4 |
| `engine.GetHTMLClient()` / `engine.FetchHTML(ctx, url)` | 静态 HTML 抓取（goquery），不启动浏览器，适合服务端渲染页 |
| `engine.M3U8ForTask(task)` / `M3U8ForStage(stage)` | m3u8 下载器：**阶段 > 站点 > 基座**，一行拿到"这一阶段该用的那个"（见 §5.4） |
| `engine.GetM3U8()` / `GetFiledown()` | 基座那份（框架按 `DefaultConfig` 备好）—— 按站点/阶段配过的那份要用上面的 `For*` 取 |
| `engine.GetDB()` | gorm 实例，用于写结果 / 查任务 |
| `engine.Upsert(record, conflictCols, updateCols)` | 冲突更新并回填主键（替代手写 `clause.OnConflict` + `if ID==0` 查回） |
| `engine.GetConfig()` | 全局配置 |
| `engine.SubmitTask(&papa.Task{...})` | 派发子任务（列表页 → 详情页） |
| `engine.SubmitTasks([]*papa.Task{...})` | 批量派发子任务（一次多行 INSERT，减少 DB 往返；撞唯一索引时自动降级为逐条并跳过冲突行，不会让整批失败） |
| `engine.GetStageStats()` | 各阶段的统计快照（监控页读的就是它） |
| `engine.AddNotifier(notifier)` | 注册失败告警通知器 |
| `engine.GetProxy()` | 代理管理器 |

### 1.5 写结果到数据库

**结果表由业务自己建、自己写** —— 框架只管任务状态（`crawler_tasks`：URL / 阶段 / 状态 / 重试 / 轮询 / 业务键），不替你决定数据长什么样。

**为什么不塞进任务表**：一类数据一张表才谈得上约束与查询（唯一键、外键、按业务维度建索引、后台单独开一张表格页）；塞进一个 JSON 列之后这些全靠应用层兜，而且"任务表"会同时承担调度状态与业务数据两种生命周期 —— 任务行被清理或重投时，业务数据跟着遭殃。

写法就是项目里 `models/` 包那一套（`papa new` 已生成骨架）：

```go
// models/ 里定义业务表（建表走 papa migrate，模型登记在 models.Models()）
type Series struct {
    ID        uint64 `gorm:"primaryKey"`
    SourceKey string `gorm:"type:varchar(191);uniqueIndex;comment:站点内稳定标识"`
    Title     string
    CoverURL  string
}

// fetcher 里写：engine.Upsert 冲突更新并回填主键（不用自己拼 ON DUPLICATE KEY）
record := &models.Series{SourceKey: key, Title: detail.Title, CoverURL: detail.CoverURL}
if err := engine.Upsert(record, []string{"source_key"}, []string{"title", "cover_url"}); err != nil {
    return err
}
```

`engine.Upsert` 与 `engine.GetDB()` 是这一层仅有的两个入口（见 §9 速查）。完整的落地策略（怎么选表、怎么定唯一键、回写与并发怎么写）见 [DATA_LANDING_GUIDE.md](./DATA_LANDING_GUIDE.md)。

### 1.6 派发子任务（阶段串联）

列表页抓到详情 URL 后，把详情页作为**子任务**提交到下一个 stage：

```go
err := engine.SubmitTask(&papa.Task{
    PID:   task.ID,       // 关联父任务
    URL:   detailURL,
    Stage: "detail",      // 必须是已声明的阶段（sites/<站名>.go 的 Stages 里，见 7.2）
    Site:  task.Site,     // 必填，见下
    Meta:  map[string]string{"series_id": "119002"}, // 携带业务键，handler 里读 task.Meta
})
```

**`Site` 必填**：任务按「站点 + 阶段」找池子，漏了它 `SubmitTask` 会拒收（日志 `task stage or url or site is empty`），任务一条都进不了池。子任务继承父任务的 `task.Site`，入口任务（`SubmitEntries`）用 `site.Key`。

去重键默认是 `site:stage:url`，同一个站点的同一个 URL 重复提交会被引擎识别为**已存在**：单任务 `SubmitTask` 返回 `nil`（视为成功，无需 `_ = err` 忽略），批量 `SubmitTasks` 直接跳过该条——天然防重。需要别的幂等键时设 `IdempotencyKey`。

**延迟投递**：episode 这类要反爬随机间隔的任务，设 `Delay`（或 `NotBefore`），引擎到点才入队，不空占 worker：

```go
engine.SubmitTask(&papa.Task{
    URL:   episodeURL,
    Stage: "episode",
    Site:  task.Site, // 必填（同 §1.6）
    Delay: time.Duration(10+rand.Intn(21)) * time.Second, // 10–30s 随机
})
```

**批量派发**：一次要投 N 个 episode 时用 `SubmitTasks`（批量入库，比循环 N 次 `SubmitTask` 更快）：

同一批先按 `Task.Unique()` 去重，保留首次出现的任务参数（包括 Meta、轮询和延迟设置），重复项回填相同 ID 与 Repeat。每个保留项只入队一次；队满则只寄放一份，延迟任务只登记一次。数据库唯一索引及冲突后查库仍用于并发兜底；本批新建任务负责投递，外部冲突记录沿用原去重规则，不因批内重复重置已有任务状态。批量插入失败可能逐条回退，不应把整个调用视为全有或全无的事务。

逐条插入时按“能成功多少就处理多少”：某条实际写入失败后继续后续行，成功行照常投递，最后汇总返回所有失败信息。返回非 nil 不代表整批都没成功；失败行不写入去重缓存，修正原因后可再次提交，成功的普通任务重试仍会被去重。落库后读取状态发生瞬态错误时，任务保留在溢出列表或延迟堆，稍后重读再投，不会拿空状态直接入队。入参和阶段合法性仍在写库前整批校验。

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

> **规则：能用 `engine.FetchHTML` 抓的页面，一律用它。** rod（浏览器）是**例外**，只在
> "目标数据在静态 HTML 里根本不存在"时才上 —— 即必须等 JavaScript 执行、点击、滚动、登录，
> 或者必须监听网络请求之后才产生的那类数据。
>
> 两条最容易搞反的：
>
> - **页面"有 JS"不等于"必须上浏览器"**：Vue / React / Nuxt 这类前端框架的页面，只要站点做了
>   服务端渲染（首屏 HTML 里就有目标节点），照样用 HTMLFetch。抓回来的 HTML 里翻得到，就是静态路。
> - **页面上有交互（点击展开、下拉加载、轮播）也不等于要用浏览器**：只要目标字段在 HTML 里已经存在
>   （哪怕藏在 `data-*` 属性或 `<noscript>` 回退标记里），照样用 HTMLFetch。
>
> 判据只有一条，可执行：**把 `FetchHTML` 抓回来的 HTML 翻一遍，目标节点在不在。** 在 → HTMLFetch；
> 不在 → 再看能不能直接从接口拿（不少站点的数据接口就是普通 HTTP，`FetchHTML` 一样能取）；都不行才上 rod。
>
> 为什么这么定：浏览器是重量级资源（真进程 + 内存 + 受 `browser.pool_size` 限制的池位 + 每页几秒），
> 而静态路还能多拿到状态码、Content-Type 与**原始字节**（归档更完整、排查更容易，见 §3.1 末）；
> 站点一多、量一大，"明明能静态却上了浏览器"会先把自己的并发位吃光 —— 那时表现为"整体变慢"，
> 而不是报错，最难查。

| 策略 | 入口 | 适用页面 | 节点获取方式 | 校验重点 | 不适用场景 |
| --- | --- | --- | --- | --- | --- |
| **HTMLFetch（默认，一律先用它）** | `engine.FetchHTML(ctx, task.URL)` | 服务端渲染；页面源码已包含列表、详情字段和链接 | `page.Document.Find/ Text/ Attr`，CSS 选择器由 goquery 执行 | HTTP 非 2xx 已自动返回错误；检查目标选择器匹配数、必填文本、链接属性与 URL 解析 | 数据仅在 JS 执行、滚动或点击接口后出现；需监听网络请求 |
| **Rod 浏览器（例外，静态拿不到才用）** | `engine.GetBrowserPool().WithBrowser(ctx, fn)`（底层 `Get` / `Put` 仍可用） | 数据必须经 JS 执行、登录交互、下拉/滚动懒加载、点击播放并截获 m3u8 才产生的页面 | `page.MustElement(s)`、`Attribute`、`MustText`，必要时执行 JS / 监听事件 | 等待页面或目标节点完成渲染；检查节点存在和属性；设置导航超时 | **静态 HTML 里已经拿得到目标数据的页面** —— 哪怕它有 JS、有交互（浏览器贵且占池位，见上面那条规则） |

HTMLFetch 不启动 Chromium，受 `html.timeout`、`html.max_body_size` 和 `html.headers` 控制；配置文件须保持 `html.enable: true`。Rod 需要同时配置 `browser.enable: true`（此时 `pool_size` 必填且 > 0）；浏览器二进制默认由 Rod 自动下载（`browser.binary: rod`，服务器不用预装），要用系统已装的浏览器写 `binary: system`，启动自检见 `CORE_CONFIG.md` 的 `browser.self_check`。`browser.pool_size` 是**全局**并发上限（浏览器按需创建，非常驻实例数）：调得比并发用浏览器的 worker 数小，多出来的 worker 只会等在池上、不报错；它不需要机械等于 worker 总数。
（这两段说的是**配置层**的开关怎么开；**该不该用浏览器是策略问题**，判据是上面那条规则 —— 不是"把 `browser.enable` 打开就该用它"。）

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

   **走浏览器的阶段**（`NeedsBrowser`）用 `engine.FetchRenderedPage`，别用 `FetchRendered` ——
   它交出的是 `*htmlfetch.Page`，与静态路径同一种东西，上面这条判据照样能用：
   ```go
   page, err := engine.FetchRenderedPage(ctx, task.URL, ".detail-box")
   if err != nil { return err }
   if err := engine.RestrictedError(task, page, ""); err != nil { return err }
   ```
   `FetchRendered` 只给 `*goquery.Document`，收 `*Page` 的判据（与归档）都用不上，于是被反爬拦下时
   只表现为"选择器解析不出东西"。两条方法抓的是同一份 HTML，改一个字符串就行；拿到的 Page 里
   `StatusCode` / `Content-Type` 是零值 —— 浏览器不给这两个值，别当成"HTTP 200"用。

   默认词表扫**可见正文 + 标题**（captcha / verify you are human / access denied / 访问受限 / 验证码 /
   Cloudflare 的 checking your browser · just a moment / Google 的 unusual traffic），业务可用
   `SiteSpec.RestrictedKeywords` 追加本站文案。它**不扫整段 HTML** —— 内联脚本与 style 里的
   `captcha` 字面量很常见，扫原始 HTML 会把好页面判成受限页。`RestrictedReason` 只读遍历原 DOM，排除 script/style；template、hidden、未打开的 dialog 和关闭 details 仅按 HTML namespace 的固有语义处理，关闭 details 只取首个 HTML 直接子 summary。SVG 的 hidden 属性不按 HTML 隐藏规则处理，foreignObject 内的 HTML 节点仍遵循 HTML 规则。标题只取 head 直属 title；不计算 CSS 可见性，也不把 aria-hidden/inert 当成视觉隐藏。
   词表刻意**宁少勿多**：命中之后通常返回不可重试错误、任务直接判死，假阳性的代价是任务白死。
   拿不准就先只观察：`task.Trace.Warn("疑似受限页", nil, map[string]string{"marker": reason})`。
2. 每一项的标题和 `href` 都必须非空；出现一个残缺条目就返回 error，避免把不完整目录标记为成功。
3. 相对地址用 `papa.ResolveURL(page.URL.String(), href)` 解析成绝对地址：只收 http/https
   （`javascript:`、`mailto:` 这类"链接"进不了任务队列）、去掉 fragment、解析不出 host 当场报错。
   **不要字符串拼接**当前页面 URL。
4. 域名用 `papa.SameHost(base, u)`（apex 与 `www.` 算同一个 host）与 `papa.IsSubdomainOf(u, base)`
   （按**点边界**比对 —— `evil-example.com` 不是 `example.com` 的子域，自己写 `strings.HasSuffix`
   最容易在这儿翻车）校验；路径前缀、ID 格式、条目数下限也在这里一起把关。
5. 将列表快照写入当前 `catalog` 任务；只有已声明 `detail` 阶段时（`sites/<站名>.go` 的 `Stages`），才为通过校验的链接调用 `engine.SubmitTask`。

> **失败时自动留现场**：开了 `crawler.archive.enabled`（见 [CORE_CONFIG.md](./CORE_CONFIG.md)）之后，
> `engine.FetchHTML` 抓到的**每一页**都会在本次尝试失败时原样落到
> `{dir}/{stage}/task-{id}-try-{retry}-{urlhash8}.html`，并在后台「追踪」里多一条 `归档页面` 步骤指向它。
> **handler 一行代码都不用改** —— 排查"选择器为什么没匹配上"时，直接打开那个文件看它到底长什么样。
> 落的是**原始字节**（不是 goquery 再序列化的结果），所以 `<noscript>` 里的延迟渲染回退标签也在。

### 3.2 Rod：动态页面或浏览器交互方式

仅当原始 HTML 不含目标节点，或数据必须经 JavaScript、点击、滚动、登录或网络监听才能得到时使用 Rod。**页面"有 JS"或"有交互"都不构成上浏览器的理由** —— 静态 HTML 里拿得到就仍用 HTMLFetch（判据见本节开头那条规则）。浏览器模式的核心是：导航完成后等待目标交互/渲染，随后对 DOM 做与 HTMLFetch 同等严格的节点和属性校验；不要只因一个页面“看起来像网页”就启动浏览器。

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

> 上面是底层写法，为了把「导航 → 等待 → 取节点」的时序摊开。新代码更推荐用
> `engine.GetBrowserPool().WithBrowser(ctx, func(bw *browser.Browser) error { ... })`
> 包住整段、再用 `bw.WithPage(ctx, fn)` / `bw.WithNavigatedPage(...)` 管页面 ——
> 成功、报错、panic 都保证归还实例与关页（见 §9）。

> 若只需「借浏览器 → 导航 → 等某个选择器出现 → 拿 HTML/最终 URL」，直接用高层封装 `engine.FetchRendered`，免去手写借还浏览器：
>
> ```go
> doc, finalURL, err := engine.FetchRendered(ctx, task.URL, ".doc-item")
> if err != nil {
>     return err
> }
> // doc 是 *goquery.Document，finalURL 是导航后的最终地址
> ```
>
> 要 `*htmlfetch.Page`（接着做受限页判定、或交给按 `page.Selection()` 写的解析函数）就用
> `engine.FetchRenderedPage`：参数完全一样，交出的是 `page.Selection()` 能直接解析、也能喂给
> `engine.RestrictedError` 的页面对象。
>
> 要等 JS 表达式 / 网络空闲，或按页拦图片、统计脚本，用
> `engine.FetchRenderedPageWith(ctx, url, browser.PageOptions{WaitSelector: ..., WaitJS: ...,
> WaitRequestIdle: ..., Block: ...})`：同样返回 `*htmlfetch.Page`，等待失败时页面 HTML 会进归档、
> trace 里多一条「等待渲染」。

需要下拉、滚动或点击时，先完成操作并等待新节点出现，再提取；m3u8 这类只在播放后产生的资源，必须先通过 `EachEvent` 挂好网络监听，再点击播放。Rod 的 `Must*` 方法遇到节点缺失会 panic，因此生产 fetcher 更适合使用返回 `error` 的 API 或预先检查元素存在性，保证错误交给引擎重试。

---

### 3.3 HTTP/API、JSON 与独立会话

采集 JSON API、POST 表单、GraphQL 或原始响应，使用 `engine.FetchHTTP`。它复用 `html.enable`、超时、响应体上限、代理及站点/逐请求头，不需要浏览器。

```go
response, err := engine.FetchHTTP(ctx, papa.HTTPRequest{
    URL: "https://example.com/api/search",
    Method: "POST",
    Headers: map[string]string{
        "Content-Type": "application/json",
        "Accept": "application/json",
    },
    Body: []byte(`{"page":2}`),
})
if err != nil { return err }
var result struct {
    Items []struct { ID string `json:"id"` } `json:"items"`
}
if err := response.DecodeJSON(&result); err != nil { return err }
```

- `Method` 留空为 GET。表单用 `url.Values.Encode()` 生成 Body，并设 `Content-Type: application/x-www-form-urlencoded`。
- `Headers` 优先于 context/站点头和客户端默认头，空值删除对应头。URL 查询参数使用 `net/url` 构建。
- `Response` 包含最终 `URL`、`StatusCode`、完整 `Header` 和限长 `Body`。非 2xx 返回响应和 `StatusError`，可在判错后读取接口错误正文；正文超限时 Body 为空，返回 `BodyTooLargeError`。状态错误可与读错误并存，使用 `errors.As` / `htmlfetch.StatusCode` 判断。
- `DecodeJSON` 要求恰好一份 JSON，拒绝尾随垃圾；解到 `any` 时数字是 `json.Number`，避免长整数被转成 float64。它不强制 Content-Type，也不替业务校验字段。
- 本封装不增加重试循环，标准库 transport 的重试规则仍适用；307/308 跟随重定向时可重放 Body。Fetcher 返回错误后，阶段重试仍然生效：有副作用的 POST 要自行设置幂等键或返回不可重试错误，不能把网络超时当作服务器未处理。
- `engine.FetchHTTP` 的自动 trace 只保存方法、状态、字节数，不保存 URL、请求头或响应正文；返回的 error 仍可能包含 URL，业务自行处理敏感信息。此路径不写 HTML 归档。
- 可直接调用 `engine.GetHTMLClient().Do(ctx, request)`，但没有 Engine trace，且需自行确认客户端已启用。原 `FetchHTML` 保持 HTML 解析与归档行为。

**Cookie 会话：** 每个账号创建并复用独立 Session。默认 HTTP 客户端不保存 Cookie。

```go
session := papa.NewHTTPSession() // 在账号生命周期内保存，不能每个请求都重新创建
accountCtx := papa.WithHTTPSession(ctx, session)
response, err := engine.FetchHTTP(accountCtx, papa.HTTPRequest{URL: "https://example.com/account"})
```

Session 自动接收 Set-Cookie，按域名/路径/Secure/有效期发送 Cookie，并使用 public suffix 列表。`WithHTTPSession(ctx, nil)` 禁用继承的会话。Session 只隔离 Jar，不隔离站点配置中的 Authorization/Cookie：不要给共享配置填某个账号的凭据，也不要混用显式 Cookie 与另一个账号的 Jar。Session 不持久化、不自动刷新登录态、不绑定代理；net/http CookieJar 不模拟浏览器 SameSite 上下文。

浏览器登录完成后，在你持有的页面回调中调用 `browser.CopyCookiesToSession(page, session, targetURL)`。只复制适用于该 URL 的 Cookie；保留 host-only 作用域，拒绝分区 Cookie，不复制 LocalStorage。调用方使用 §3.4.1 的隔离入口管理浏览器账号；创建 HTTP Session 本身不会自动隔离浏览器。该操作是一次快照，后续变化不会自动同步。

**页面内嵌 JSON：** `page.Document.JSONBlocks("script[type='application/ld+json']")` 返回按 DOM 顺序排列的 `[]json.RawMessage`，可继续用 `htmlfetch.DecodeJSON` 解码。缺少节点、非法选择器或任何一块非法 JSON 都报错；不会执行 `window.x=...` 等 JavaScript。

### 3.4 捕获浏览器 API 响应

已有 `engine.CaptureRequest` 用于尽早取得媒体请求地址。需要查看 API 响应正文时用独立的 `engine.CaptureResponse`：

```go
response, err := engine.CaptureResponse(ctx, task.URL, browser.ResponseOptions{
    Timeout: 15 * time.Second,
    MaxBodySize: 1 << 20,
    Match: func(r browser.CapturedResponse) bool {
        return strings.Contains(r.Request.URL, "/api/list") &&
            strings.Contains(r.Header.Get("Content-Type"), "application/json")
    },
})
if err != nil { return err }
// 捕获到 4xx/5xx 也可能是有效诊断结果，调用方检查 response.StatusCode。
var payload any
if err := htmlfetch.DecodeJSON(response.Body, &payload); err != nil { return err }
```

监听在导航前建立，匹配第一条响应后等待同一请求完成，再读取 Body；其他请求不会串入。默认超时 30 秒，正文上限 10 MiB；超时/取消会关闭页面。Match 只看到元数据，必须快速返回，不能在回调中调用浏览器方法。请求元数据暂存最多 256 条，超出后 Method 可能为空，URL 和资源类型仍来自响应事件。

这条路径用于有限大小的 API 响应，不用于下载和长连接。CDP 读取完整正文后才能检查解码后大小，上限限制返回值，**不是 Chrome 内存的硬限制**。响应头和正文仅返回调用方，自动 trace 不落原文；需要保存样本时由业务先脱敏。它不会自动回放接口或复制 Cookie。`RequestHeader` 和 `RequestBody` 保存对应 CDP 请求事件的头与正文；`MaxRequestBodySize` 默认 64 KiB，超限仅保留前缀。`RequestBodyIncomplete` 为 true 表示截断、Chrome 未提供正文或请求事件未被保留，不能把这类样本当作完整请求重放。`Page` 使用设备、请求头、Cookie、资源拦截和导航 Timeout；WaitSelector/WaitJS/WaitRequestIdle 不作为捕获完成条件。

### 3.4.1 按账号隔离浏览器上下文

需要登录的抓取使用 `pool.WithIsolatedBrowser`；强制直连使用 `WithDirectIsolatedBrowser`。每次调用创建独立 Chromium Incognito 上下文，回调内多个页面共享该账号的 Cookie 和 LocalStorage，与默认上下文及其他隔离调用分开。

```go
session := papa.NewHTTPSession()
err := engine.GetBrowserPool().WithIsolatedBrowser(ctx, func(account *browser.Browser) error {
    return account.WithNavigatedPage(ctx, loginURL, browser.PageOptions{
        Timeout: 30 * time.Second,
    }, func(page *rod.Page) error {
        // 在这个上下文内完成你的登录流程，再读取适用于 API 地址的 Cookie。
        return browser.CopyCookiesToSession(page, session, apiURL)
    })
})
if err != nil { return err }
// 回调结束后浏览器账号状态已销毁；显式复制出的 HTTP Session 仍可使用。
response, err := engine.FetchHTTP(papa.WithHTTPSession(ctx, session), papa.HTTPRequest{URL: apiURL})
```

- 一个回调占用一个池中浏览器名额；在同一回调内完成该账号的一组操作。再次调用会创建新会话，不能按账号名找回前次状态。
- 新入口不继承池默认 Headers/Cookies，避免共享凭据进入隔离账号；请求头和 Cookie 通过 `PageOptions` 显式传入，设备设置仍继承。池级头热更新也不会覆盖该隔离上下文。
- Cookie、LocalStorage 属于独立浏览器上下文；代理出口仍沿用借到的浏览器进程。隔离上下文不是独立进程，不提供独立代理配置。
- 成功、错误、panic 均先销毁上下文再归还进程；取消时浏览器操作受 ctx 中断，回调退出后仍用独立清理超时销毁上下文。回调必须配合 ctx，框架不能强制终止任意 Go 代码。
- 销毁失败会关闭并淘汰父浏览器进程，普通返回路径合并清理错误；panic 保持原值向上传播。
- 不要把回调参数或页面带出回调，也不要对它调用池的 Put；若回调启动 goroutine，必须等它们退出再返回。
- 隔离上下文本身是临时的；跨进程恢复使用 §3.4.2 的显式加密快照，检查和登录的站点逻辑由业务提供，可用 §3.4.3 的 SessionGuard 协调刷新。旧 `WithBrowser` 保持原有默认上下文行为。

### 3.4.2 加密保存和恢复页面登录态

`browser.ExportPageState(page)` 导出隔离页面当前 URL 适用的 Cookie（含 HttpOnly）以及当前 origin 的 LocalStorage。普通默认上下文被拒绝，分区 Cookie 被拒绝。导出期间应停止该账号的并发导航和存储写入，快照不是跨两种存储的原子事务。

快照包含凭据，不能当作 §3.5 的脱敏样本。使用 `WritePageState(writer, state, key)` / `ReadPageState(reader, key)` 加密保存、验证并读取：密钥必须是调用方管理的 32 字节随机密钥，AES-GCM 每次使用新 nonce，版本头同时参与认证；文件里不包含密钥，错误密钥、篡改和截断都会失败。明文快照和加密输入均有约 8 MiB 上限（加密输入额外允许 envelope 开销）。

```go
// 在隔离账号回调内，登录完成后：
state, err := browser.ExportPageState(page)
if err != nil { return err }
if err := browser.WritePageState(destination, state, key); err != nil { return err }

// 新进程/新隔离上下文内，先打开同源页面，再恢复：
state, err = browser.ReadPageState(source, key)
if err != nil { return err }
if err := browser.RestorePageState(page, state); err != nil { return err }
if err := page.Reload(); err != nil { return err }
if err := page.WaitLoad(); err != nil { return err }
```

恢复会合并快照 Cookie、替换当前 origin 的 LocalStorage；不会清除快照之外的 Cookie，因此建议使用全新的隔离上下文。恢复前检查版本及同源关系，执行脚本时再次检查 origin；调用方仍需避免同时导航。恢复过程中失败可能已有部分写入，应丢弃该隔离上下文。页面必须设置 ctx/Timeout。

此版本先打开页面再写 LocalStorage，站点首轮脚本已经运行；恢复后需要重新加载，且初始打开的地址不能跳转到另一个登录域。如果站点要求首次请求前已有 Cookie 或跨域认证，需要业务单独编排，不能直接套用该例。

文件与密钥管理由应用控制：接口只读写 Reader/Writer，不默认落盘；密钥应独立于状态文件保存，并在重启后仍可取回。Writer 失败可能留下半份文件，由应用以临时文件和原子发布处理。不要记录 PageState、Cookie 值或明文 JSON。

范围只包含当前 URL 可用 Cookie 和当前 origin 的 LocalStorage，不包含其他路径 Cookie、IndexedDB、sessionStorage、Service Worker 或缓存。持久化不会延长服务端登录有效期，快照接口不自动刷新账号登录；恢复后可由 §3.4.3 的 SessionGuard 调用业务检查与登录流程。

### 3.4.3 登录态检查与刷新协调

本节的 `SessionGuard` 是兼容保留的低层 HTTP Cookie 协调器。需要站点＋账号注册、Token/CSRF、业务响应过期处理、加密重启恢复和 Rod 账号运行时，使用 [ACCOUNT_RUNTIME.md](./ACCOUNT_RUNTIME.md) 的 `account.Manager` 与 `Engine.FetchAccountHTTP / FetchAccountHTML / WithAccountBrowser`；新入口显式选择账号，不改变旧入口。

每个账号创建并长期复用一个 `htmlfetch.SessionGuard`，在业务请求前调用 `Ensure(ctx)`。它返回已经复验的独立 HTTP Session；没有当前会话时先登录，有当前会话时先 Check，明确返回 `false, nil` 才重新登录。网络错误必须返回 error，不能误判成登录过期。

```go
client := engine.GetHTMLClient() // html.enable 必须开启
// accountLogin 是业务实现：把此账号登录得到的 Cookie 写入 candidate。
// 可以使用 WithIsolatedBrowser 登录，再 CopyCookiesToSession(page, candidate, apiURL)。
guard, err := htmlfetch.NewSessionGuard(htmlfetch.SessionGuardConfig{
    Check: func(ctx context.Context, session *htmlfetch.Session) (bool, error) {
        response, err := client.Do(htmlfetch.WithSession(ctx, session), htmlfetch.Request{
            URL: accountProbeURL,
        })
        // 此例约定该站账号检查接口用 401 表示未登录；其他站应按自己的业务响应判断。
        if response != nil && response.StatusCode == 401 { return false, nil }
        if err != nil { return false, err }
        var result struct { LoggedIn bool `json:"logged_in"` }
        if err := response.DecodeJSON(&result); err != nil { return false, err }
        return result.LoggedIn, nil
    },
    Login: accountLogin, // func(context.Context, *htmlfetch.Session) error
    Timeout: 30 * time.Second,
    FailureCooldown: 5 * time.Second,
})
if err != nil { return err }
// guard 应在账号初始化时保存，不能在每次 FetchHandler 里重建。
session, err := guard.Ensure(ctx)
if err != nil { return err }
response, err := engine.FetchHTTP(papa.WithHTTPSession(ctx, session), papa.HTTPRequest{URL: apiURL})
```

新登录始终使用全新的 CookieJar，复验成功才发布；失败不会把半登录状态覆盖到原会话，Ensure 返回错误而不返回旧会话。Login 只操作传入的候选会话，Check 只读取会话，回调不得重入同一个 Guard。CookieJar 自身线程安全，但业务回调中的其他共享状态仍需自行同步。

同一 Guard 的并发调用共享一轮检查/登录结果。首调用者执行回调并控制整轮 context，首调用者取消会影响等待者；等待者可以独立取消自己的等待。默认整轮超时 30 秒（零值使用默认，负值拒绝），回调必须配合 ctx；任意阻塞 Go 代码不能被框架强行终止。

普通失败结果默认冷却 5 秒（零值使用默认，负值拒绝），期间直接返回同一失败，防止重复登录风暴；取消和超时不缓存。登录后 Check 仍无效时返回 `ErrSessionRejected`。回调 panic 会释放等待者，等待者拿到 `ErrSessionCallbackPanic`，首调用者继续向上传播原 panic。

这不是周期保活器：不会启动后台登录任务，不跨进程协调，也不自动重发业务请求。Ensure 成功后服务端仍可能随时使令牌失效；业务响应失效时应返回明确错误，按业务幂等规则决定是否再调用 Ensure。尤其不要自动重放付款/提交等有副作用的 POST。登录凭据、验证码、加密快照保存时机和账号级代理绑定由业务负责；默认只管理内存 Cookie 会话。

### 3.5 显式脱敏样本与离线解析

账号浏览器的首次导航前恢复可使用 `Browser.WithStatePage`；它在初次请求前设置 Cookie、在主 frame 首段脚本前恢复 LocalStorage，加载后卸载初始化器。旧 `RestorePageState` 的「先导航再恢复」语义保持不变。统一账号入口会自动使用前者，见 [账号手册](./ACCOUNT_RUNTIME.md)。

`htmlfetch.WriteSample(writer, request, response, policy)` 将一次 HTTP 交换输出为版本化 JSON。它不创建文件，也不自动发送网络请求。默认省略所有头、查询参数及正文，并删除 URL userinfo 和 fragment；URL 路径仍保留，路径包含账号或签名时用 `RedactURL` 处理。

`SamplePolicy.Headers` / `QueryKeys` 是显式白名单，保留的值仍须由调用方确认。Authorization、Proxy-Authorization、Cookie、Set-Cookie 即使误入白名单也不会导出。`RedactBody(requestSide, body)` 在私有副本上处理正文；未提供时省略正文，提供时以其返回值为准。普通关键字过滤不能保证任意业务数据已脱敏，建议按已知 JSON schema 挑选可保留字段。

```go
// response 来自已检查错误的 FetchHTTP，request 是对应 HTTPRequest。
var sample bytes.Buffer
err := htmlfetch.WriteSample(&sample, request, response, htmlfetch.SamplePolicy{
    Headers: []string{"Content-Type"},
    QueryKeys: []string{"page"},
    RedactBody: func(requestSide bool, body []byte) ([]byte, error) {
        if requestSide { return []byte(`{}`), nil } // 不保留请求数据
        var source struct {
            Items []struct {
                ID json.Number `json:"id"`
                Title string `json:"title"`
            } `json:"items"`
        }
        if err := htmlfetch.DecodeJSON(body, &source); err != nil { return nil, err }
        return json.Marshal(source) // 只保留显式声明的字段
    },
})
if err != nil { return err }
// 此时 sample.Bytes() 可作为脱敏测试夹具保存；文件权限与原子写入由调用方管理。
fixture, err := htmlfetch.ReadSample(bytes.NewReader(sample.Bytes()), 16<<20)
if err != nil { return err }
offline, err := fixture.OfflineResponse()
if err != nil { return err }
var result any
if err := offline.DecodeJSON(&result); err != nil { return err }
```

所有脱敏回调成功后才开始编码给 Writer；回调报错不会向 Writer 输出数据，错误消息也不会复制回调中的敏感文本。Writer 自身写失败仍可能留下半份文件，需由调用方负责临时文件/原子发布。

`ReadSample` 默认输入上限 16 MiB，拒绝超限、未知版本、尾随 JSON；`OfflineResponse` 返回独立响应副本，仅供解析器测试。省略正文的样本会明确报错，不冒充空响应。样本 Body 在 JSON 文件中使用 base64 编码，编码不是脱敏。该能力不做实时请求回放，也不模拟浏览器状态。

浏览器样本先检查 `RequestBodyIncomplete`，再用 `CapturedResponse.Request` 的 URL/Method、RequestHeader/RequestBody 构造 HTTPRequest，以捕获到的 Header/Body/StatusCode 构造 htmlfetch.Response 后导出。这里的请求头来自 CDP 常规事件，不能当作包含 Cookie 等所有线上头的完整网络记录。普通 HTTPRequest 则是调用参数，不包含客户端最终合并的默认头。

### 3.6 网络录制、内容发现与质量治理

多响应录制使用 `Engine.RecordNetwork`；增量 GET 使用 `Engine.FetchIncremental`。`pkg/discovery` 提供链接/订阅源/robots 解析与分页停止判定，`pkg/extract` 提供字段和批次质量校验。完整用法、预算、状态语义与边界见 [CRAWL_TOOLKIT.md](./CRAWL_TOOLKIT.md)。

## 4. 场景一：JS 下拉加载 → 抓列表路由 + 详情信息

> 脚手架默认只生成单阶段 `catalog`。本节演示怎么扩成「列表 → 详情」两阶段 —— 写第二个 fetcher，在 `sites/<站名>.go` 里加一段 `StageSpec` 即可（main.go 不用动）。

> 下面几段浏览器示例用底层的 `Get` / `Put` + `MustPage` 展示时序；新代码更推荐
> `WithBrowser` / `WithPage`（见 §9），成功、报错、panic 都保证归还实例与关页。

典型例子：目录页要点「展开分类」下拉、滚动到底部触发懒加载，才能拿到所有文档的标题和链接。

### 4.1 在站点声明里加两个阶段

```go
// sites/mysite.go
Stages: []papa.StageSpec{
    {
        Fetcher:     &fetcher.FetchCatalog{}, // 列表页阶段
        WorkerCount: 1,
        QueueSize:   20,
        Delay:       "5m",
        Retry:       papa.RetrySpec{MaxAttempts: 3, Backoff: "30s"},
        Entry:       true, // 这个阶段有入口任务：启动那一趟投一轮（见 §7.1）
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

### 4.2 main.go

不用动 —— 各站点文件在自己的 `init()` 里登记，框架用 `papa.Sites()` 收集（见 7.2）。

### 4.3 catalog fetcher（列表页：点下拉 + 滚动 + 派发）

```go
package fetcher

import (
    "context"
    "strings"
    "time"

    "github.com/go-rod/rod"
    "github.com/ydtg1993/papa/v3"
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
            Site:  task.Site,
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

### 4.4 detail fetcher（详情页：提取字段 + 写库）

```go
package fetcher

import (
    "context"
    "time"

    "github.com/ydtg1993/papa/v3"
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

    // 结果落业务自己的表（见 §1.5）：engine.Upsert 冲突更新并回填主键
    series := &models.Series{SourceKey: task.URL, Title: title, CoverURL: deref(coverURL)}
    return engine.Upsert(series, []string{"source_key"}, []string{"title", "cover_url"})
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

> **先看这里：请求会自动发出时，不要手写事件循环。**
> 如果播放器自动播放、请求在页面加载过程中自己发出（不需要点击），用
> `engine.CaptureRequest` —— 它负责「导航前挂监听 → 截第一条匹配请求 → 关页面」，
> 失败时还会把**那一刻的页面 HTML 与期间发出的请求样本**写进归档和任务追踪
> （OA 抽屉里的「捕获网络请求」步骤，见 [MONITOR.md](./MONITOR.md) §3）：
>
> ```go
> res, err := engine.CaptureRequest(ctx, task.URL, browser.CaptureOptions{
>     Page: browser.PageOptions{Timeout: 45 * time.Second},
>     Match: func(r browser.CapturedRequest) bool {
>         u, err := url.Parse(r.URL)
>         return err == nil && strings.HasSuffix(strings.ToLower(u.Path), ".m3u8")
>     },
>     CaptureTimeout: 30 * time.Second,
> })
> if err != nil {
>     return err // 现场已登记；不要再自己归档/上报
> }
> mediaURL := res.URL
> ```
>
> 只有「必须先点一下/滚动/输入」的页面才继续看下面的手写方式（V1 的 `CaptureRequest`
> 不执行交互）。需要等渲染条件（元素 / JS 表达式 / 网络空闲）时用
> `engine.FetchRenderedPageWith(ctx, url, browser.PageOptions{WaitSelector: ..., WaitJS: ...,
> WaitRequestIdle: ...})`；要按页拦图片/统计脚本用 `browser.PageOptions{Block: ...}`。

### 5.1 关键点：先挂监听，再点播放

m3u8 地址是点播放后才产生的网络请求，所以**必须在导航后、点击前**用 Rod 的 `EachEvent` 监听网络请求。
下面的手写示例用 `Get` / `Put` 展示时序；实例与页面也可以套 `WithBrowser` / `WithPage`
（见 §9），归还与关页由框架保证。

```go
package fetcher

import (
    "context"
    "strings"
    "sync"
    "time"

    "github.com/go-rod/rod/lib/proto"
    "github.com/ydtg1993/papa/v3"
    "github.com/ydtg1993/papa/v3/pkg/middleware/m3u8"
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
    wait := page.EachEvent(func(e *proto.NetworkRequestWillBeSent) bool {
        if !strings.Contains(e.Request.URL, ".m3u8") {
            return false
        }
        mu.Lock()
        if m3u8URL == "" {
            m3u8URL = e.Request.URL
        }
        mu.Unlock()
        return true // 命中即收工
    })

    // 2. 点击播放按钮，触发视频请求
    page.MustElement(".play-btn").MustClick()
    page.MustWaitLoad()

    // 3. 等那个请求真的发出 —— wait() 阻塞到上面的回调返回 true
    wait()

    mu.Lock()
    url := m3u8URL
    mu.Unlock()
    if url == "" {
        return nil // 或返回 error 触发重试
    }

    // 3. 交给 m3u8 下载器（框架默认就有一份；按站点/阶段配过的话用 M3U8ForTask(task) 取，见 §5.4）
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

    // 4. 写库：结果落业务自己的表（见 §1.5），不改任务行。
    // res.OutputFile 是**相对** dl.OutputDir() 的路径，要落一个能直接指到文件的本地路径就拼起来。
    episode := &models.Episode{SourceKey: episodeKey, MediaURL: url, LocalPath: filepath.Join(dl.OutputDir(), res.OutputFile)}
    return engine.Upsert(episode, []string{"source_key"}, []string{"media_url", "local_path"})
}
```

### 5.2 下载器接线（main.go）

```go
import "github.com/ydtg1993/papa/v3/pkg/middleware/m3u8"

// 站点声明里（sites/<站>.go）—— 这就是全部的配置入口，没有"全局那层"：
SiteSpec{ M3U8: &papa.M3U8Spec{
    OutputDir: strPtr("./downloads/video"), // 输出目录
    Output:    modePtr(papa.OutputSegments),// 只留切片组（默认是合并成 mp4，见 §5.4）
} }
```

m3u8 下载器能力：并发下载片段、AES-128 解密、断点续传、限速、合并（`concatTSFiles` 或 ffmpeg 转 mp4）。`Download` 是同步阻塞的，返回 `DownloadResult{OutputFile, Segments, Size, Error}`。

filedown 与 m3u8 各自的下载器基座及 `With` 派生实例共享目标路径锁：不同 URL 写同一目标也会串行，目录资源与其子路径也互斥（例如分片目录 `movie/` 与另一下载的 `movie/segment_00000.ts`）。一次取得整组资源，等待时不占用其中一部分，支持 context 取消。m3u8 按最终合并扩展名确定目标，并对共享断点状态文件和切片目录加锁；仅共享父目录的不同文件、独立目标与独立断点目录可以并发。路径以规范化绝对路径识别，Windows 忽略大小写；不解析符号链接/硬链接别名，不跨独立 `NewDownloader` 实例或进程协调。filedown 单文件临时输出使用独占创建的随机文件名，避免固定 `.tmp` 与其它下载目标冲突。

### 5.3 静态页 / 普通文件用 filedown

不需要浏览器、直接下载文件（图片、附件等）时用 `filedown`：

```go
import "github.com/ydtg1993/papa/v3/pkg/middleware/filedown"

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

### 5.4 下载器配置：站点 / 阶段 / 随用随生成

下载器（`m3u8` 与 `filedown`）**实例是唯一真值，声明只是"帮你自动建一份"**。配置**只有两层**（站点与阶段），基座是框架按 `DefaultConfig()` 备好的那份 —— **没有"全局那层配置"可写**（`SetM3U8` / `SetFiledown` 已删除）：要么在声明里按站点/阶段写清楚，要么在 fetcher 里自己造一份。

```go
// ① 站点级：本站所有阶段默认这一份（sites/<站>.go）
SiteSpec{ Key: "example",
          M3U8: &papa.M3U8Spec{OutputDir: &dir, Output: &papa.OutputSegments},
          FileDown: &papa.FileDownSpec{MaxConcurrent: &n} }

// ② 阶段级：比站点更细，赢了就用它（StageSpec 上同名字段）
StageSpec{ Fetcher: &examplefetcher.FetchEpisode{}, M3U8: &papa.M3U8Spec{Output: &papa.OutputPlaylist} }

// ③ fetcher 里随用随生成：拿"这一阶段该用的那份"，叠一层只给这次调用用的改动
dl := engine.M3U8ForTask(task)                       // 阶段 > 站点 > 基座，不会拿错站
tmp := dl.With(m3u8.Overlay{MaxConcurrent: &n})      // 新配置，内部状态仍是同一份
own := m3u8.NewDownloader(&myCfg)                    // 完全独立的一份（孤岛，代价自负）
```

解析顺序一句话：**阶段 > 站点 > 基座（框架默认那份）**；逐次下载还能用 `DownloadOptions`（头/Referer/代理 + `Output`/`MergeExt`/`KeepSegments`）再叠一层。`Overlay` 字段**全是指针 —— nil = 不覆盖**，只写要改的那几项。

**`With` 与 `NewDownloader` 的区别（很重要）**：

| | 配置 | 内部状态（同一目标文件的串行锁、密钥缓存、代理连接池、消息队列） |
| --- | --- | --- |
| `dl.With(overlay)` | 独立 | **共享** —— 所以"每个阶段一份配置"不会丢掉"两条并发下载写同一个文件被串行化"这条保证，也不丢缓存/连接池 |
| `m3u8.NewDownloader(&cfg)` | 独立 | **独立** —— 真要一座谁也不共享的孤岛时才用；代价是并发写同一个目标文件时，串行由你自己负责 |

**产物形态**（`m3u8`，`OutputMode`）：`OutputMerge`（默认，ffmpeg `-c copy` remux 到 `MergeExt`，`.mp4`/`.mkv`/`.ts` 都行）、`OutputSegments`（**只留切片组**，不产出单文件，切片挪到 `<OutputDir>/<file>/` 下）、`OutputPlaylist`（**只要清单**，把 `.m3u8` 原文存下来，一个分片都不下）。`filedown` 没有这一维（它就是下一个文件）。

**"到底哪份生效"**：`papa check` 会把每站/每阶段**改过的项**打出来（只列改过的，没改的不打）；启动日志里也有同样一条。四层覆盖最容易变成"改一处要看几份声明"，这是唯一的便宜补偿。

## 6. 最小可跑骨架（脚手架已生成）

`papa new <name>` 会生成好 main.go / fetcher/fetch_catalog.go / models（建表清单 models.go）/ monitor（后台分层：表格页、控制器、视图、路由与中间件）/ **sites/（一站一个文件，自登记）** / configs/config.yaml + config.yaml.example / docker / docs / Makefile / logs，你只需把 fetcher 里的 TODO 换成真实逻辑。

其中 main.go 的阶段注册只有一行（声明在 `sites/<站名>.go`）：

```go
app.RegisterSites(papa.Sites()...)
```

生成后的 fetcher 长这样（`SubmitEntries` 是入口任务，见 7.1）：

```go
package fetcher

import (
	"context"
	"strings"

	"github.com/ydtg1993/papa/v3"
)

// FetchCatalog 阶段一：抓取目录/列表页。
// 职责：打开目标页 → 提取标题 + 详情链接 → 派发 detail 子任务。
type FetchCatalog struct{}

func (f *FetchCatalog) GetStage() string { return "catalog" } // 阶段名，本站内唯一（两个站可以同名）

// SubmitEntries：站点声明写了 Entry: true 时，框架在启动那一趟调它。
func (f *FetchCatalog) SubmitEntries(engine *papa.Engine, site papa.Site) {
	_ = engine.SubmitTask(&papa.Task{
		URL:   strings.TrimRight(site.BaseURL, "/") + "/list",
		Stage: "catalog",
		Site:  site.Key,
		// Repeatable: true + RepeatInterval 让这条入口任务参与周期轮询（首次入库时播种）。
		Repeatable: true,
	})
}

func (f *FetchCatalog) FetchHandler(ctx context.Context, task *papa.Task, engine *papa.Engine) error {
	// **默认静态客户端**：页面上有 JS/交互不改变这一点，只要目标节点在 HTML 里翻得到。
	// 只有数据必须等 JS/点击/滚动/登录/网络监听才产生时才上 rod（见 §3.2、§4、§5）。
	page, err := engine.FetchHTML(ctx, task.URL)
	if err != nil {
		// 步骤追踪（crawler.trace.enabled 开启时生效）：失败时带上现场数据。
		// task.Trace 关闭时是 nil，这些调用都是安全的 no-op —— 不需要判空。
		task.Trace.Fail("抓取列表页", err, map[string]string{"url": task.URL})
		return err
	}
	// Step 表示「这一步已经做完了」，耗时按距上一步的间隔算
	task.Trace.Step("抓取列表页", map[string]string{"url": page.URL.String()})

	// TODO: 遍历列表项，提取标题和详情链接，然后派发子任务：
	// engine.SubmitTask(&papa.Task{PID: task.ID, URL: detailURL, Stage: "detail", Site: task.Site})

	return nil
}
```

> `task.Trace.*` 是**框架的步骤上报接口**（见 1.3），关掉追踪时是 no-op —— 写新 fetcher 时照抄即可。

配套两处改动：
1. `sites/<站名>.go` 里加一段 `StageSpec`（WorkerCount / QueueSize / Delay / Retry，以及 `Entry`：这个阶段有没有入口任务）。
2. 若用下载器，**不用在 `main.go` 里接线**（框架按 `DefaultConfig` 备好基座）；要按站点/阶段各配一份就写在站点声明里（见 §5.4）。

---

## 7. 如何驱动任务（提交 + 监控）

Papa 没有 MCP 了，任务驱动靠四处：

1. **初始任务**：让 fetcher 自己实现 `SubmitEntries(*papa.Engine, papa.Site)`（见 1.1 的可选接口），框架在启动时自动调它，
   并把**本站声明的快照**一并交过来 —— 入口地址用 `site.BaseURL` 拼，入口路由表由 fetcher 自己维护：

```go
// fetcher/<站点>/fetch_catalog.go
var catalogRoutes = []struct {
    Key  string
    Path string
}{
    {Key: "日本", Path: "category/japanese/"},
}

func (f *FetchCatalog) SubmitEntries(engine *papa.Engine, site papa.Site) {
    for _, route := range catalogRoutes {
        url := strings.TrimRight(site.BaseURL, "/") + "/" + strings.TrimLeft(route.Path, "/")
        if err := engine.SubmitTask(&papa.Task{
            URL:            url,
            Stage:          "catalog",
            Site:           site.Key, // 必填：入口任务没有父任务，站点取自声明快照
            Meta:           map[string]string{"category_key": route.Key},
            IdempotencyKey: site.Key + ":catalog:" + url, // 规范格式：site:stage:<URL>
            Repeatable:     true,
        }); err != nil {
            engine.GetLoggerSet().Engine.Errorf("submit initial task: %s", err.Error())
            return
        }
    }
}
```

> 它跑在**所有阶段的池子都建好之后**，所以可以往任意阶段投递（不限于自己这个阶段）：
> 比如把多个站点的入口分别交给各自的阶段。调用顺序按阶段名字典序，启动期提交顺序是稳定的。
>
> **有没有入口、由声明说了算**：`StageSpec.Entry` 是 bool ——
> `Entry: true` 表示"这个阶段有入口任务"，启动那一趟就投一轮；不写就是"没有入口这回事"
> （回调压根不会调，后台也没有这一档）。
>
> **"启动先别跑"不是这个字段的事**（它曾经是三态，`manual` 管的就是这个 —— 结果是只管住了
> 入口那一投：启动恢复、轮询队列照样把这一站跑起来，看着像"声明没生效"）。要"先把入口排好、
> 再放开这一站"，写**站点级**的 `SiteSpec.StartPaused: true`：入口任务照投，但整站在入队点被闸住 ——
> 每一条都寄放到溢出表里、池子一个都不建，后台 Dashboard「站点控制」卡上点「恢复整站」才灌回。
> 它管的是**这一站的全部阶段**（包括 detail / episode 这种由上一级投出来的），而且暂停不跨重启，
> 每次启动都重新闸上。只想掐某一个阶段（不想让入口阶段又跑一遍、其它照跑）用
> `engine.PauseStage(site, stage, reason)` / `ResumeStage`，见 [MONITOR.md](./MONITOR.md) §9。
>
> 声明跟实现对不上会打 WARN，且文案指路：写了 `Entry: true` 却没实现 `SubmitEntries`，
> 或者**实现了入口却忘了写 `Entry`**（false 是默认值，这是最常见的那种手误）。
>
> **站点与阶段声明在一个文件里**：`sites/<站名>.go`（Key / BaseURL / Headers / 熔断阈值 /
> 下载器覆盖，以及每个阶段的并发·队列·间隔·重试·入口开关都在那一处；**三个治理队列不在这里，统一写在 `config.yaml`**。站点声明没有汇总清单 —— 文件自己在
> `init()` 里登记），`main.go` 只要一行 `app.RegisterSites(papa.Sites()...)`。
> 加阶段 = 写一个 fetcher（`GetStage()` 返回阶段名）+ 在这个站的文件里加一段 `StageSpec`。
> 阶段名取自 `GetStage()`，**同一个站里**重名启动就报错并点名；两个站之间同名是允许的（键是 `site:stage`，池子/统计/归档/唯一索引都按它分）。

### 7.2 多站：一站一个文件（框架自己收集）

登录账号按 `account.Key{Site, ID}` 进一步划分；任务只在既有 Meta 中保存账号标识，Fetcher 显式选择账号入口。账号暂停不会自动暂停站点队列，两者是不同作用域，见 [账号手册](./ACCOUNT_RUNTIME.md)。

一站一个文件放进 `sites/`（同一个包），**每个文件在自己的 `init()` 里登记** ——
框架把它们收在 `papa.Sites()` 里，**项目里没有汇总清单要维护**：

```
configs/
  config.yaml
  config.yaml.example
sites/
  example.go              // 一站一份：Key/BaseURL/Breaker/Stages + func init() { papa.RegisterSite(example()) }
  example2.go             // 加一个站 = 加一个文件（改 Key/BaseURL/阶段参数）
```

```go
// main.go
import (
    _ "yourmod/sites"   // 匿名导入：让各站点文件的 init 跑起来（它们自己登记）
)
app.RegisterSites(papa.Sites()...)
```

`configs/` 根目录只放数据（config.yaml / config.yaml.example），Go 包单独一层（`sites/`）。
**框架不需要"站点包"这个概念** —— `SiteSpec` 就是普通值类型；为什么必须"文件自己登记"：
Go 没有"枚举一个包里有哪些函数/类型"的能力（反射只能从值倒推名字），所以"框架自动发现"只有
代码生成或代码自登记两条路，这里选后者（零构建步骤、可 grep，也是 `database/sql` 驱动那套标准做法）。
登记顺序 = **文件名字典序**（Go 规范：同包 init 按文件名执行），稳定可复现；站点键/阶段名重复都会在启动时报错。

main.go 仍然一行：`app.RegisterSites(papa.Sites()...)`。**框架不需要"站点包"这个概念** —— `SiteSpec` 就是普通值类型，怎么组织是你项目的事。

要注意的：

- **三个治理队列在 `config.yaml` 里全局配**（error_queue / recover_queue / repeat_queue，各一段，不再按站点拆；
  改完要重启，没有热更）。没写哪段 = 那条不跑；开了就得写全，缺一项 / 越界启动就报。见 [CORE_CONFIG.md](./CORE_CONFIG.md)。
- **阶段名只在本站内唯一**：两个站可以各有自己的 `catalog`，各自一份池子、统计与归档目录（键是 `site:stage`，库里的唯一索引也是 `(url_hash, stage, site)`）。**同一个站里**重名会在启动时 panic 并点名。
- **别把 Go 包放进 `configs/`**：那是数据目录（config.yaml / config.yaml.example）。声明放 `sites/`。
  fetcher 怎么摆由你定：**脚手架就是一个扁平的 `fetcher/` 包，多站也照样能用**（加一个 fetcher 文件即可）；
  站点多了想按站点分包（`fetcher/<站点>/`，如 `fetcher/example/`）也行。硬要求只有一条 ——
  **阶段名（`GetStage()`）在本站内唯一**，跟 fetcher 放哪儿无关。
- **"加一个站"不等于只复制一个声明文件**：阶段的存在离不开 fetcher，而**阶段名取自 `GetStage()`**
  （不在声明文件里）。复制站点文件后新站若还指向原来那个 fetcher，**不会**再报重名（同名阶段在不同站是合法的），
  但两个站会跑同一份 fetcher —— 选择器与入口路由都是照旧站写的，抓出来的是错的、却不报错。
  副本要连带自己的 fetcher（新类型，或按站点分包）。
- **声明写完先自查**：项目目录里跑一次 `papa check` —— **不用起库**就能把上面这几项（阶段名重复、
  三个治理队列漏写/缺项、熔断阈值、Delay 与 Backoff 写法）一次报全。见 [CLI.md](./CLI.md)。
- **站点值只声明一处，fetcher 从框架取，别在 fetcher 里存副本**：
  handler 里 `site, _ := engine.Site(task.Site)` → `site.BaseURL` / `site.Headers`；
  入口任务里框架把同一份快照交给 `SubmitEntries(engine, site)`。
  BaseURL / Headers 在 fetcher 里存副本（`type Catalog struct{ base string }` 或包级常量）就会漂 ——
  声明里改了地址，副本不跟着变，而漂了不报错，只表现为"本站链接被判成站外"。
  入口路由表正相反：它**只**声明在 `fetcher/<站点>/fetch_catalog.go`（包级 `catalogRoutes` 那类，例见 §7.1），
  入口投递与 handler 反推任务的 key 共用同一张表 —— 别在 handler 里再抄一份 key → 路由的映射。

已按站切开的东西：**阶段键 `site:stage`**（阶段名只在本站内唯一，登记表/工作池/统计/覆盖层/溢出桶都按它分）、每站每阶段一个池子（并发/间隔/重试各自独立）、**每站一个熔断计数器**（没单配阈值的站用 `crawler.breaker` 那份默认值，但仍是自己那份；后台右上角铃铛逐 scope 一行、可逐个放行）；**暂停是一处信号**（人工暂停、熔断触发、以及声明期的 `SiteSpec.StartPaused` 写的是同一张表，来源记在 `source` 里；取任务点冻住、入队点寄放）、归档目录 `{dir}/{site}/{stage}/`、任务表 `site` 列（提交时**必填**，跨站派发自然落到目标站）、后台任务表的「站点」列与筛选、告警事件带 `site`、`engine.Site(task.Site)` 取站点声明。

**请求头按站切开了**：`SiteSpec.Headers` —— 这个站的任务抓任何页面都自动带上（静态抓取与浏览器渲染都认），
同键覆盖 `html.headers` / `browser.headers`，空值表示删掉那个头；handler 里对**单次请求**还能
`engine.FetchHTML(papa.WithHeaders(ctx, map[string]string{...}), url)` 再覆盖（叠加，只覆盖给到的键）。
优先级：框架默认 < 全局 headers 配置 < 站点 headers < 逐请求。
站点还有一项 `RestrictedKeywords`：本站自己的"受限页"文案，追加到上面那个默认词表之后。
**入口路由表不归站点声明管**：`SiteSpec` 就是那九项（Key / BaseURL / Headers / RestrictedKeywords / StartPaused / Breaker / M3U8 / FileDown / Stages），
"要抓哪些分类"（key → 相对 BaseURL 的路径）写在 `fetcher/<站点>/fetch_catalog.go` 的包级变量里（例见 §7.1）。
框架本来就不解析它 —— 站点声明里放一份，等于让框架背一个自己用不到的字段，而它还得和 handler 反推 key 的那张表
保持一致（两处，漂了不报错）。

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

2. **阶段间串联**：fetcher 里用 `engine.SubmitTask(&papa.Task{PID: task.ID, URL: ..., Stage: "detail", Site: task.Site})` 派发子任务（见 1.6）。

3. **定时重抓 / 恢复失败**：
   - 定时轮询：`repeat_queue` 配置开启后，`repeatable: true` 的任务在到点时自动重跑（周期可逐条定、后台可改）。详见 [REPEAT_QUEUE.md](./REPEAT_QUEUE.md)；
   - 中断恢复：`recover_queue` 配置开启后，**启动时**重投本进程启动前遗留的未终态任务（pending/processing，且 `updated_at` 早于本次启动时间）。这个边界用于排除本次运行已经碰过的行，不是按“卡死时长”猜测。详见 [RECOVER_QUEUE.md](./RECOVER_QUEUE.md)。

4. **失败任务再处理**：`error_queue` 配置开启后，`failed` 任务会被自动（`interval` 轮询）或手动（后台「队列治理」模块里该队列的「立即执行」按钮 / `POST /api/errorqueue/process`）重新投递，带 `max_retry` 再处理代数上限。详见 [ERROR_QUEUE.md](./ERROR_QUEUE.md)。

**看状态**：启动后打开监控页 `http://localhost:9090/monitor`（`server.enabled: true`），OA 后台布局：Dashboard 看机器 CPU/内存/磁盘、业务目录（downloads/logs）占用与任务队列概览（概览按站点分卡片，切到某站点 Tab 可看该站各阶段的五格数字、累计耗时与 Worker 明细），另有「队列治理」等模块。登录用**访问令牌**（`papa token add --operator <名字>` 创建，库里只存哈希）；来源 IP 限制在后台「设置」页改（存在 `crawler_config` 表，空 = 不限制）。

---

## 8. 常见坑

HTTP API 返回 2xx 不代表业务成功；JSON 解码后仍需验证业务状态和必填字段。有副作用的请求必须考虑阶段重试与重定向重放，Cookie 会话必须按账号复用并隔离。


1. **同一个站里阶段名重复** → `RegisterSites` 直接 panic 并点出是这个站的哪两个阶段（多半是复制站点文件后两份声明指向了同一个 fetcher）。两个站用同一个阶段名**不是**错误 —— 那是这一版的常态，不必为了避开重名去硬加前缀。
2. **浏览器池容量规划**：`browser.pool_size` 是浏览器抓取的全局并发上限，不必机械地大于等于所有阶段 `worker_count` 之和。容量较小时，超出的 browser worker 会在 `pool.Get(ctx)` 等待实例，这是正常背压；应按机器资源、目标站承载能力和可接受等待时间调优。`browser.enable: true` 时 `pool_size` 必须 > 0，配置加载会拒绝零/负值。
3. **m3u8 需要 referer/cookie**：多数 m3u8 站点校验 referer，用 `m3u8.OptionsFromRequest(ctx, task.URL)` 一步带上（站点头/逐请求头/代理一起过来），或自己写 `m3u8.DownloadOptions{Referer: ...}`。
4. **下载器不继承抓取的头**：`SiteSpec.Headers` / `papa.WithHeaders` 只作用于**抓取**（静态 + 浏览器），下载（filedown / m3u8）要自己带 —— 用 `OptionsFromRequest(ctx, referer)`，它不会自动发生。
5. **懒加载**：滚动加载别只滚一次，循环滚到底 + 等待，直到没有新元素。
6. **相对链接**：`href`/`src` 可能是相对路径，用 `papa.ResolveURL(base, href)` 解析成绝对地址再提交任务
   （rod 那条路的基础地址是 `page.Info().URL`，静态抓取是 `page.URL.String()`）；别用字符串拼接，也别把 `#锚点` 当成不同的页面（`ResolveURL` 会去掉它）。
7. **重试语义**：fetcher 返回普通 error 会触发重试；对「确实失败、重试无意义」的（页面 404、缺字段、验证码拦截等），返回 `papa.WrapNoRetryKind("structure", err)`（或 `WrapNoRetry(err)`），引擎不重试、直接标 failed 并告警，别再「返回 nil 假装成功」。
8. **写库用 task.ID**：子任务派发后，每个 fetcher 只写自己这个 `task.ID` 的记录。
9. **ffmpeg**：`AutoMerge: true` 转 mp4 需要系统装 ffmpeg；只想拼 TS 就 `AutoMerge: false`。
10. **延迟投递**：反爬随机间隔用 `task.Delay`（或 `NotBefore`）在派发时设置，别在 handler 里 `time.Sleep` 空等，那会浪费 worker 并发位。
11. **别重复记日志**：`FetchHandler` 的 error 和 `SubmitTask` 的提交错误框架都会自动记，fetcher 里**不必再** `logger.Errorf` 记一遍，否则会刷双份。你只需判断 error 要不要改变控制流（继续/中止/告警）。
12. **限流（429 / 503）现在按普通失败重试**：框架还不认 `Retry-After` —— 命中限流后它会立刻按该阶段的 `Retry.Backoff` 再打一次，容易把限流升级成封禁。反爬严的站点把 `delay` 调大（如 `"30s-60s"`），或对 429 返回 `papa.WrapNoRetryKind("rate_limited", err)`，交给 error_queue 稍后重投。

---

## 9. API 速查

通用 API：`FetchHTTP` / `HTTPRequest` / `HTTPResponse.DecodeJSON`；会话：`NewHTTPSession` / `WithHTTPSession`；浏览器响应：`CaptureResponse` / `browser.CopyCookiesToSession`。用法和边界见 §3.3–3.4。

账号运行：`NewAccountManager` / `AccountKey` / `AccountConfig`、`engine.SetAccountManager`、`FetchAccountHTTP` / `FetchAccountHTML` / `WithAccountBrowser` / `AccountStatus`，见 [ACCOUNT_RUNTIME.md](./ACCOUNT_RUNTIME.md)。离线规则：`crawlrule.Parse/Compile`、`Compiled.Evaluate/Verify`、`Registry.Activate/Rollback`，见 [CRAWL_RULES.md](./CRAWL_RULES.md)。


**go-rod（页面操作）**
- `bw.Browser.MustPage("")` 建空白页；`page.Close()` 关页。
- `page.Context(ctx).Timeout(d).Navigate(url)` 带取消+超时导航；`page.MustWaitLoad()`。
- `page.MustElement(sel)` / `page.MustElements(sel)`；`el.MustClick()` / `el.MustText()` / `el.Attribute(name)`。
- `page.MustEval(js)` 执行 JS（滚动、取全局变量等）。
- `page.EachEvent(func(e *proto.NetworkRequestWillBeSent) bool { ... })` 监听网络请求：**返回 wait 函数**（别在它后面多写一个 `()` —— 那会当场阻塞在事件循环里）。回调**必须返回 `bool`**，返回 `true` 才会让 `wait()` 退出；命中时 `wait()` 返回。

**浏览器托管 / 捕获 / 等待（推荐优先用这组）**
- `pool.WithBrowser(ctx, fn)` / `pool.WithDirectBrowser(ctx, fn)`：借实例执行回调，成功 / 报错 / panic 都保证归还。
- `bw.WithPage(ctx, fn)` / `bw.WithNavigatedPage(ctx, url, opts, fn)`：页面生命周期托管，返回后保证关页。
- `engine.CaptureRequest(ctx, url, browser.CaptureOptions{Page, Match, CaptureTimeout, WaitLoad})`：导航前挂监听、截第一条匹配请求；失败时现场 HTML 与候选请求自动进归档 + trace。
- `engine.FetchRenderedPageWith(ctx, url, browser.PageOptions{...})`：比 `FetchRenderedPage` 多支持 `WaitSelector` / `WaitJS`（表达式）/ `WaitRequestIdle` / `Block`（按页拦资源）。顺序：`WaitSelector → WaitJS → WaitRequestIdle`，共享 `Timeout` 的等待预算。
- 等待失败返回 `*browser.WaitError`（`Stage` / `HTML` / `Elapsed`）；引擎层已自动归档并写「等待渲染」步骤，业务直接返回 error 即可。

**任务身份 / 站点地址**
- `papa.PageIdentity(rawURL)`：`path[?query]`，与域名无关（`https://a.com` 与 `https://a.com/` 都是 `/`）。
- `papa.TaskKey(site, stage, rawURL)`：`site:stage:<PageIdentity>`，适合站点换域名/多镜像时的自定义幂等键；超过 1000 字符报错，不截断。
- `papa.ResolveSiteURL(baseURL, raw)`：把任务里存的旧地址钉回当前站点根（scheme/host 换成 baseURL 的），path/query 保留。

**静态 HTML（goquery）**
- `engine.FetchHTML(ctx, url)` → `*htmlfetch.Page`；`page.Document.Text/Texts/Attr/Attrs/HTML(selector)`。**默认路径**，见 §3 的规则。
- `page.Selection()`（= `page.Document.Selection()`）→ `*goquery.Selection`：要写复杂解析
  （`Find(...).Each`、Children/Parent/Siblings…）就拿这个根节点。解析函数统一收 `*goquery.Selection`，
  线上传 `page.Selection()`、测试传 `goquery.NewDocumentFromReader(...).Selection` —— 同一个类型，
  不用为"框架的文档 vs 测试的文档"再包一层接口。
- 链接与域名：`papa.ResolveURL(base, ref)` / `papa.SameHost(a, b)` / `papa.IsSubdomainOf(child, parent)`。
- 受限页：`engine.RestrictedError(task, page, stage)`（站点词表自动带上，不可重试）。page 来自 `FetchHTML` 或 `FetchRenderedPage` —— 两者都是 `*htmlfetch.Page`。

**下载器**
- m3u8：`NewDownloader(cfg)` / `Download(ctx, url, outDir, outFile, opts...) → DownloadResult`。
- filedown：`NewDownloader(cfg)` / `Download(ctx, url, outDir, fileName, opts...) → DownloadResult`。
- 两个下载器都有 `Downloader.OutputDir()`：`DownloadResult.OutputFile` 是**相对**它的路径，落库的本地路径靠它拼。
- 两个下载器都有 `OptionsFromRequest(ctx, referer)`：把抓取上下文里的站点级/逐请求头与显式代理转成下载选项
  （下载器不读 ctx，用不用由你显式决定）；`filedown.DownloadOptions.Direct` 走单次 GET（小文件）。

**数据库 / 结果落地**
- `engine.Upsert(record, conflictCols, updateCols)` 冲突更新并回填主键 —— 业务结果写自己的表就用它。
- `engine.GetDB()` 拿 gorm 实例（复杂查询、建自己的表时用）。
- 任务表只存调度状态（URL / 阶段 / 状态 / 重试 / 轮询 / `meta` 业务键）；结果的结构与落地全归业务，见 [DATA_LANDING_GUIDE.md](./DATA_LANDING_GUIDE.md)。

**任务派发 / 错误分类 / 告警**
- `engine.SubmitTask(&papa.Task{...})` 派发单个子任务；`engine.SubmitTasks([]*papa.Task{...})` 批量派发。
- `papa.WrapNoRetry(err)` / `papa.WrapNoRetryKind(kind, err)` 标记不可重试错误。
- `papa.Retryable(err)` / `papa.ErrorKind(err)` 判断错误是否可重试 / 取其分类。
- `engine.AddNotifier(notify.NewWebhook(url))` 注册失败告警；`notify` 在 `pkg/notify`。熔断触发时也会经这条通道发一条 `papa.AlertCritical`（级别比 `papa.AlertError` 高一级，可单独路由）。
- `engine.PauseCrawling(reason)` / `engine.ResumeCrawling()`：手动闸住 / 放行**所有站点**的抓取；逐站操作用 `engine.PauseSite(site, reason)` / `engine.ResumeSite(site)`，读状态用 `engine.BreakerStatuses() map[string]BreakerStatus`（键是站点，站点是必填维度）。配 `crawler.breaker` 可由框架自动触发（见 [CORE_CONFIG.md](./CORE_CONFIG.md)）。
- 入口任务由框架在**启动那一趟**投（阶段声明写了 `Entry: true` 的话）—— 引擎侧没有「手动投入口」这个动作。想控制「什么时候开始跑」就声明 `SiteSpec.StartPaused: true`，再到后台「站点控制」卡上点「恢复整站」（那是整站级的暂停信号，见 [MONITOR.md](./MONITOR.md) §10）。
- `engine.FetchRendered(ctx, url, waitSelector)` 借浏览器渲染并返回 `(*goquery.Document, finalURL, error)`。**只在静态 HTML 拿不到目标数据时用**，见 §3 的规则。
- `engine.FetchRenderedPage(ctx, url, waitSelector)` 同上，但返回 `*htmlfetch.Page`（`StatusCode` / `Content-Type` 为零值）—— 走浏览器的阶段要接 `engine.RestrictedError` 或复用静态路径那套解析时用它。
- `engine.ProcessErrorQueue()` 立即把失败任务重新投递（返回处理条数）；配 `error_queue` 可自动轮询或 OA 手动触发。

---

## 10. 运行观测（obs）：业务怎么接

观测层（事件契约 / 报告引擎 / 模式库）是框架通用的，业务只通过三个扩展点接入；详细手册见 [OBSERVABILITY.md](./OBSERVABILITY.md)。

**10.1 发业务事件**

```go
// 业务服务里：类型常量自己定义，attrs 是框架不解释的自由键值
if ev := app.Events(); ev != nil {
    ev.Emit(events.Event{
        Type:   "prepare_round_finished",
        Site:   "siteA",
        Stage:  "prepare",
        Counts: map[string]int{"ok": 12, "failed": 1},
        Attrs:  map[string]string{"round": "3"},
    })
}
```

`events.Event` 来自 `github.com/ydtg1993/papa/v3/pkg/events`；打开 `obs.events.enabled` 后业务事件与框架事件
共用同一个 sink、同一个 `run_id` / `seq` 序列。事件关闭时 `app.Events()` 返回 no-op，不判空也能跑。

**10.2 自定义报告段**

实现 `obs.Section`（`ID()` / `Title()` / `Build(ctx, *obs.Input) (SectionResult, error)`），在 `monitor.Register` 或 `Run` 之前注册：

```go
app.UseReportSection(sections.All()...)   // 业务自己的分析段
```

`obs.Input` 带事件、日志摘要与只读 DB；用 `obs.NewCheck(...)` 产出判定项、`obs.Table` 产出表格。
**业务段只读**：不写表、不重投、不调接收端，同一输入两次跑结果必须一致。

**10.3 自定义模式**

实现 `obs.Pattern`（从输入推断 `obs.Problem`，带 `Evidence` / `Hypothesis` / 验证命令），
`app.UseReportPattern(...)` 注册。模式只做假设与验证方法，不自动修数据。

**边界**：papa 不认任何业务语义（没有 prepare / push / 站点名 / 业务表）；业务阈值放 `business` 段
（`app.Config.BusinessSection`），不要塞进框架配置。
