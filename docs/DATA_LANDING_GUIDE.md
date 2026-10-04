# Papa 数据落地手册

> 面向：写完 fetcher 之后，怎么把抓到的内容「落地」成模型、写进数据库，以及写在哪里。
> 配套阅读：[FETCHER_WRITING_GUIDE.md](./FETCHER_WRITING_GUIDE.md)（抓取逻辑怎么写）。

---

## 0. 一句话

Papa 的落地是**「单表 + JSON 内容」**：所有阶段的任务共用一张 `crawler_tasks` 表，用 `stage` 列区分阶段，抓到的结构化内容以 JSON 塞进 `content` 列。你要做的是：**把页面提取成 Go 结构体，序列化后写回对应任务的 `content`（和 `title`）列**。

> 框架一共内置 4 张表，业务要写的只有 `crawler_tasks` 一张（后三张是后台自用，见 §1）。业务的内容结构（写进 `content` 列的 JSON）由你自己在项目的 `models` 包里按需定义 —— 脚手架不预置示例。

---

## 1. 读懂现状：已有哪些模型

框架一共内置 4 张表，**只有第一张是业务要写的**，其余三张是后台自用（业务通过结果 API 或 `task.Trace` 间接用，不直接读写表）：

| 表 | 用途 | 业务碰不碰 |
| --- | --- | --- |
| `crawler_tasks` | 任务与抓取结果 —— `title` / `content` 两列就是落地点 | **是**（`engine.SaveResult` / `SaveContent` / `Upsert`） |
| `crawler_access_token` | 后台访问令牌（`papa token add` 或后台「访问令牌」页维护） | 否 |
| `crawler_operation_log` | 后台操作审计，开关 `server.operation_log` | 否 |
| `crawler_task_trace` | 任务步骤追踪，开关 `crawler.trace.enabled`（handler 里 `task.Trace.Step` 上报） | 否 |

后三张跟着开关走，建表与开关的对应关系见 [CORE_CONFIG.md](./CORE_CONFIG.md) 第 2 节。本手册不展开它们（业务不碰）—— 各有归属：访问令牌见 [MONITOR.md](./MONITOR.md) 第 1 节（含字段表）、操作日志见 [CORE_CONFIG.md](./CORE_CONFIG.md) 的 `server.operation_log`、步骤追踪见 [MONITOR.md](./MONITOR.md) 第 3 节的「追踪」抽屉。

### 1.1 任务表 `crawler_tasks`（业务唯一要写的一张）

业务关心的只有两个落地列：

| 列 | 说明 |
| --- | --- |
| `title` | 页面标题（string） |
| `content` | 结构化内容（JSON，本质 `[]byte`），存什么结构由**每个 stage 自己决定** |

其余列（`status` / `retry` / `error` / `repeat` 等）是框架运行态，由引擎自动维护，**业务不要直接读写**。

- 内置唯一约束 `(stage, url)`：同一个 URL 在一个阶段只会有一条记录。
- **`IdempotencyKey` 是「软约束」**：任务可以自定义 `IdempotencyKey`（`papa.Task{ IdempotencyKey: "..." }`）作为去重键。提交时引擎会**按它查库**（`findTaskRecord` 优先用 `idempotency_key`、其次 `stage + url`），命中就复用已有记录、不重复入库；内存去重表只是前面的一道快取，被淘汰了也不影响正确性。
  但数据库**唯一索引只建在 `(stage, url)` 上**，「同幂等键、不同 URL」这种组合它拦不住：两个这样的任务并发提交时，
  两边查库都没查到、又都能插进去，就可能各留一行。约定：使用 `IdempotencyKey` 时保证同一幂等键始终对应同一 `(stage, url)`；
  否则其去重是「尽力而为」而非强一致。
- 业务**不直接碰 model**，而是通过结果 API 读写（见第 2 节）。

### 1.2 业务内容结构（你自己在 `models` 包里定义）

下面是一个**更完整的动漫示例**，把这些结构写进你的 `models/` 包即可：

```models/comic.go
package models

import "time"

// ComicContent 动漫/漫画详情页内容
type ComicContent struct {
	ID        int       `json:"id"`         // 主键 ID
	SourceURL string    `json:"source_url"` // 来源页面 URL
	Cover     string    `json:"cover"`      // 封面本地化地址
	CoverURL  string    `json:"cover_url"`  // 封面原始 URL
	Title     string    `json:"title"`      // 标题
	Author    string    `json:"author"`     // 作者
	Tags      []string  `json:"tags"`       // 标签
	Category  string    `json:"category"`   // 分类
	CreatedAt time.Time `json:"created_at"` // 创建时间
	UpdatedAt time.Time `json:"updated_at"` // 更新时间
}

// TableName 指定表名
func (ComicContent) TableName() string {
	return "comic_contents"
}
```

```models/chapter.go
package models

import "time"

// ChapterContent 动漫/漫画章节内容
type ChapterContent struct {
	ID              int               `json:"id"`               // 主键 ID
	ComicID         int               `json:"comic_id"`         // 所属漫画 ID（外键）
	SourceURL       string            `json:"source_url"`       // 章节来源 URL
	Title           string            `json:"title"`            // 章节标题
	Order           int               `json:"order"`            // 章节序号（用于排序）
	Images          map[string]string `json:"images"`           // 序号 -> 图片原始 URL
	DownloadsImages map[string]string `json:"download_images"`  // 序号 -> 已下载图片本地路径
	ImageCount      int               `json:"image_count"`      // 图片总数
	DownloadedCount int               `json:"downloaded_count"` // 已下载数量
	Status          int               `json:"status"`           // 状态: 0-未开始 1-下载中 2-已完成 3-失败
	CreatedAt       time.Time         `json:"created_at"`       // 创建时间
	UpdatedAt       time.Time         `json:"updated_at"`       // 更新时间
}

// TableName 指定表名
func (ChapterContent) TableName() string {
	return "chapter_contents"
}

// 章节状态常量
const (
	ChapterStatusPending    = 0 // 未开始
	ChapterStatusDownloading = 1 // 下载中
	ChapterStatusCompleted  = 2 // 已完成
	ChapterStatusFailed     = 3 // 失败
)

// Progress 返回下载进度（0.0 ~ 1.0）
func (c *ChapterContent) Progress() float64 {
	if c.ImageCount == 0 {
		return 0
	}
	return float64(c.DownloadedCount) / float64(c.ImageCount)
}

// IsCompleted 判断章节是否下载完成
func (c *ChapterContent) IsCompleted() bool {
	return c.Status == ChapterStatusCompleted && c.DownloadedCount >= c.ImageCount
}
```

对应三级爬虫：

| 阶段 | 抓什么 | 落地结构 |
| --- | --- | --- |
| `catalog` | 分类目录页：漫画标题 + 详情页链接 | 通常只派发子任务，也可写一个列表结构 |
| `detail` | 详情页：封面/标题/作者/标签/章节列表 | ComicContent（含 ChapterItem 列表） |
| `video` | 章节页：每页图片 URL + 本地文件 | ChapterContent（含 ImageItem 列表） |

> 视频场景是 catalog → detail → video（播放页 m3u8）； 漫画场景对应的是 catalog → detail → chapter（章节页多张图片）， 因为漫画一个章节通常有几十张图，不是单一媒体流。

---

## 2. 三条落地路径（按需求选）

### 路径 A：直接写在 `FetchHandler` 里（默认，最推荐起步）

适合：单任务、单次写、逻辑简单。写完就完事，代码就在抓取逻辑旁边，一眼看懂。

```go
func (f *FetchDetail) FetchHandler(ctx, task, engine) error {
    // ... 抓取得到 title / cover / series ...
    content := models.DetailContent{ Title: title, CoverURL: coverURL, /* ... */ }
    return engine.SaveResult(task.ID, title, content)
}
```

### 路径 B：抽一个 repository / 保存函数

适合：**多个 fetcher 复用同一套落地逻辑**、需要 upsert / 事务 / 批量写、想写单测。

```go
// models/repo.go（你自己的 models 包里）
func SaveDetail(engine *papa.Engine, taskID int, dc DetailContent) error {
    return engine.SaveResult(taskID, dc.Title, dc)
}
```

fetcher 里只调 `models.SaveDetail(engine, task.ID, content)`。

### 路径 C：新建独立 GORM 表

适合：内容要被**按列查询 / 关联 / 统计**（例如「查所有已下载的剧集」「按作者聚合」），而不是塞在一个 JSON 里。

做法：
1. 定义模型：**一张表一个文件** —— `models/episode.go`、`models/series.go`……（`models/models.go`
   只放建表清单，不堆表结构）
2. 注册迁移：脚手架项目里是加进根 `models` 包的 `Models()` 清单（`make migrate` 时一起建）；
   不用脚手架的话就是 `papa.New(papa.WithModels(&models.YourNewTable{}))`，或在 New 之后 `app.UseModels(...)`
3. fetcher 里 `db.Create(&models.YourNewTable{...})`

### 决策表

| 需求 | 选择 |
| --- | --- |
| 单个 fetcher 写自己的结果，够了 | 路径 A |
| 多个 fetcher 写同一类数据 / 要 upsert / 要单测 | 路径 B |
| 要按字段查询、关联、统计，或跨任务汇总 | 路径 C |

> 起步先走 A，发现重复了再抽 B，真有查询需求再上 C。不要一上来就建一堆表。

---

## 3. 分阶段落地（结合动漫爬虫示例）

### 3.1 catalog 阶段：抓目录页

目录页一般是「标题 + 详情链接」，落地通常只做**派发子任务**，不写正文（或只写一个轻量列表结构）：

```go
// 派发详情子任务
engine.SubmitTask(&papa.Task{ PID: task.ID, URL: detailURL, Stage: "detail" })
```

如果目录页本身要留档（比如「当日上新列表」），可以写一个自定义结构到当前任务的 `content`。

### 3.2 detail 阶段：抓详情页 → 写 `DetailContent`

```go
import (
    "github.com/ydtg1993/papa/v2"
    "yourproject/models"
)

// 提取
title := page.MustElement("h1").MustText()
coverURL, _ := page.MustElement(".cover img").Attribute("src")
author := page.MustElement(".author").MustText()
tags := page.MustElements(".tag").Texts()

// 剧集列表：标题 -> 播放页 URL
series := map[string]string{}
for _, el := range page.MustElements(".episode a") {
    name := el.MustText()
    href, _ := el.Attribute("href")
    if href != nil {
        series[name] = *href
    }
}

content := models.DetailContent{
    Title:    title,
    CoverURL: deref(coverURL),
    Author:   author,
    Tags:     tags,
    SeriesContent: models.SeriesContent{
        Series:    series,
        Downloads: map[string]bool{}, // 初始为空
    },
}
return engine.SaveResult(task.ID, title, content)
```

### 3.3 video 阶段：抓视频 → 写 `VideoContent`，并回写详情页下载标记

视频阶段产出 m3u8 地址和本地文件：

```go
content := models.VideoContent{ Dir: res.OutputFile, Source: m3u8URL }
engine.SaveContent(task.ID, content)

// 回写父任务(detail)的下载标记：读-改-写
markDownloaded(engine, task.PID, m3u8URL)
```

`markDownloaded` 需要读父任务的 `DetailContent`，改 `Downloads` 再写回：

```go
func markDownloaded(engine *papa.Engine, detailTaskID int, seriesURL string) error {
    var dc models.DetailContent
    if err := engine.GetResult(detailTaskID, &dc); err != nil {
        return err
    }
    if dc.SeriesContent.Downloads == nil {
        dc.SeriesContent.Downloads = map[string]bool{}
    }
    dc.SeriesContent.Downloads[seriesURL] = true
    return engine.SaveContent(detailTaskID, dc)
}
```

> 这样 video 阶段每下载一集，详情页的 `Downloads` 就多一个 `true`，配合断点续传和重复轮询，下次只下没下过的。

---

## 4. 生成 model：从抓取内容到结构体

### 4.1 优先复用，其次扩展

- 内容结构没有框架预置版本 —— 在 `models` 包里按页面需要**加字段**或**定义新结构**，都只影响 `content` 里的 JSON，不用改表结构。
- 字段命名用 `json` tag 明确序列化名（下划线风格，和现有保持一致）。

### 4.2 自定义一个内容结构（示例：目录页留档）

```go
// models/catalog_content.go（你自己的 models 包）
package models

type CatalogContent struct {
    Items []CatalogItem `json:"items"`
}

type CatalogItem struct {
    Title string `json:"title"`
    URL   string `json:"url"`
}
```

fetcher 里序列化写入即可，和 `DetailContent` 完全一样的写法。

### 4.3 真要建独立表（路径 C）

**一张表一个文件** —— 比如 `models/episode.go`：

```go
// models/episode.go（你自己的 models 包）
type Episode struct {
    ID         uint   `gorm:"primarykey"`
    DetailURL  string `gorm:"index"`
    Title      string
    VideoURL   string
    Downloaded bool
}
```

再登记进建表清单：脚手架项目里是加到 `models/models.go` 的 `Models()`
（`make migrate` 时一起建）；不用脚手架则是 `papa.New(papa.WithModels(&models.Episode{}))`。

如果还想在监控后台里分页/筛选这张表，用 `app.UseTables` 注册一个表格页（声明列怎么显示，
数据由业务实现 `oao.Source` 提供）。详见 [MONITOR.md](./MONITOR.md) 的「表格页」。

---

## 5. 落地代码速查

```go
import (
    "github.com/ydtg1993/papa/v2"
)

// 写 content + title（最常用）
engine.SaveResult(task.ID, t, someStruct)

// 只写 content
engine.SaveContent(task.ID, someStruct)

// 读 content 回结构体
var dc models.DetailContent
engine.GetResult(task.ID, &dc)

// 独立表 upsert：按冲突列更新，并回填主键（无需再手写 OnConflict + if ID==0 查回）
engine.Upsert(&models.Episode{SeriesID: 1, EpisodeNo: 2, Title: "第2集"},
    []string{"series_id", "episode_no"},
    []string{"title", "source_url", "updated_at"})
// 调用后 ep.ID 已被回填
```

---

## 6. 常见坑

1. **结果 API 帮你序列化**：`SaveResult` / `SaveContent` 内部自动 `json.Marshal`，`GetResult` 自动 `json.Unmarshal`，业务不用再碰 `datatypes.JSON` / `json.Marshal`。
2. **写库只写自己的 `task.ID`**：子任务各自写各自记录，不要越界改别人的任务。
3. **改 JSON 里的嵌套字段要「读-改-写」**：`content` 是整列 JSON，没有嵌套路径更新，改 `Downloads` 这类内层字段必须整列读出来改完写回。
4. **新增独立表记得注册迁移**：脚手架里是加进根 `models` 包的 `Models()` 清单，非脚手架则是 `papa.New(papa.WithModels(&models.YourModel{}))`（或 New 之后 `app.UseModels(...)`）。这些模型会被 `App.Migrate()` 一起建 —— 脚手架里就是 `make migrate`（在项目目录里 `papa migrate` 等价，它会转交过来）。若要后台浏览/筛选该表，在 `monitor/tables.go` 的 `Tables(db)` 里注册表格页（见 [MONITOR.md](./MONITOR.md)「表格页」）。
5. **并发写同一记录**：detail 派发多个 video 子任务时，多个子任务可能同时回写父任务的 `Downloads`，会丢更新。需要的话对父任务加锁或串行回写（简单做法：用数据库事务或 `gorm` 的 `clause.Locking`）。
6. **Content 空值 / nil map**：确保传给 `SaveResult` / `SaveContent` 的结构体已初始化（尤其 `map` 字段，`nil` map 序列化是 `null` 不是 `{}`）。
7. **字段命名**：JSON tag 用下划线风格，和现有 `cover_url` / `series_info` 一致，避免和别处拼写不一致。
8. **upsert 回填主键**：`gorm` 的 `OnConflict` 在「冲突更新」路径不回填自增 ID，别手写 `if ID==0 { 按唯一键查回 }`；直接用 `engine.Upsert(record, conflictCols, updateCols)`，它自动回填。

---

## 7. 落地位置小结

| 问题 | 答案 |
| --- | --- |
| 结果写哪张表 | `crawler_tasks` 表的 `content`（JSON）+ `title` 列（通过 `engine.SaveResult` / `SaveContent`） |
| 用什么结构 | 自己在 `models` 包里定义的 struct（序列化进 `content` 列） |
| 写在哪 | 默认直接写 `FetchHandler`；复用/复杂了再抽 repository；要按列查询才建独立表 |
| 建表要不要迁移 | 是，`papa.New(papa.WithModels(...))` 里注册（dev 环境） |
