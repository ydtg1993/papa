# Papa 数据落地手册

> 面向：写完 fetcher 之后，怎么把抓到的内容「落地」成模型、写进数据库，以及写在哪里。
> 配套阅读：[FETCHER_WRITING_GUIDE.md](./FETCHER_WRITING_GUIDE.md)（抓取逻辑怎么写）。

---

## 0. 一句话

Papa 的落地是**「单表 + JSON 内容」**：所有阶段的任务共用一张 `crawler_tasks` 表，用 `stage` 列区分阶段，抓到的结构化内容以 JSON 塞进 `content` 列。你要做的是：**把页面提取成 Go 结构体，序列化后写回对应任务的 `content`（和 `title`）列**。

> 框架只内置一张 `crawler_tasks` 任务表（业务通过结果 API 读写，不直接碰 model）。业务内容结构（`DetailContent` 等）由 `papa new` 生成在**你自己的 `models` 包**里。

---

## 1. 读懂现状：已有哪些模型

### 1.1 任务表 `crawler_tasks`（框架内置）

框架内置一张 `crawler_tasks` 任务表，业务关心的只有两个落地列：

| 列 | 说明 |
| --- | --- |
| `title` | 页面标题（string） |
| `content` | 结构化内容（JSON，本质 `[]byte`），存什么结构由**每个 stage 自己决定** |

其余列（`status` / `retry` / `error` / `repeat` 等）是框架运行态，由引擎自动维护，**业务不要直接读写**。

- 内置唯一约束 `(stage, url)`：同一个 URL 在一个阶段只会有一条记录。
- 业务**不直接碰 model**，而是通过结果 API 读写（见第 2 节）。

### 1.2 业务内容结构（你自己的 `models` 包，`papa new` 已生成）

脚手架生成的 `models/content.go` 是**极简起点**，字段如下（加字段只影响 `content` 里的 JSON，不影响表结构）：

```go
type DetailContent struct {
    Title string   `json:"title"`
    Cover string   `json:"cover"`
    Tags  []string `json:"tags"`
}
```

下面是一个**更完整的动漫示例**，你可以把这些结构写进 `models/content.go` 按需使用：

```go
// 动漫详情页内容
type DetailContent struct {
    Cover         string        `json:"cover"`      // 封面本地化地址
    CoverURL      string        `json:"cover_url"`  // 封面原始 URL
    Title         string        `json:"title"`
    Author        string        `json:"author"`
    Tags          []string      `json:"tags"`
    SeriesContent `json:"series_info"`              // 剧集列表
}

// 剧集列表
type SeriesContent struct {
    Series    map[string]string `json:"series"`   // 标题 -> 剧集 URL
    Downloads map[string]bool   `json:"download"` // 剧集 URL -> 是否已下载
}

// 视频资源（m3u8）
type VideoContent struct {
    Dir    string `json:"dir"`    // 本地输出目录/路径
    Source string `json:"source"` // m3u8 地址
}
```

对应三级爬虫：

| 阶段 | 抓什么 | 落地结构 |
| --- | --- | --- |
| `catalog` | 分类目录页：标题 + 链接 | 通常只派发子任务，也可写一个列表结构 |
| `detail` | 详情页：封面/标题/作者/标签/剧集列表 | `DetailContent`（含 `SeriesContent`） |
| `video` | 视频页：m3u8 地址 + 本地文件 | `VideoContent` |

> 剧集列表的 `Series` 用 `map[标题]url` 存，天然去重；`Downloads` 用 `map[url]bool` 记录下载进度，配合断点续传用。

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
1. 定义模型：`models/xxx.go`（你自己的 models 包）
2. 在 `main.go` 里用 `papa.New(papa.WithModels(&models.YourNewTable{}))` 注册迁移
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

- 先看脚手架生成的 `models/content.go` 里的结构是否够用；不够就**加字段**或**定义新结构**，都只影响 `content` 里的 JSON，不用改表结构。
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

然后在 `main.go`：

```go
app, err := papa.New(papa.WithModels(&models.Episode{}))
```

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
```

---

## 6. 常见坑

1. **结果 API 帮你序列化**：`SaveResult` / `SaveContent` 内部自动 `json.Marshal`，`GetResult` 自动 `json.Unmarshal`，业务不用再碰 `datatypes.JSON` / `json.Marshal`。
2. **写库只写自己的 `task.ID`**：子任务各自写各自记录，不要越界改别人的任务。
3. **改 JSON 里的嵌套字段要「读-改-写」**：`content` 是整列 JSON，没有嵌套路径更新，改 `Downloads` 这类内层字段必须整列读出来改完写回。
4. **新增独立表记得注册迁移**：`papa.New(papa.WithModels(&models.YourModel{}))`（dev 环境自动迁移，生产迁移要另外走正式流程）。
5. **并发写同一记录**：detail 派发多个 video 子任务时，多个子任务可能同时回写父任务的 `Downloads`，会丢更新。需要的话对父任务加锁或串行回写（简单做法：用数据库事务或 `gorm` 的 `clause.Locking`）。
6. **Content 空值 / nil map**：确保传给 `SaveResult` / `SaveContent` 的结构体已初始化（尤其 `map` 字段，`nil` map 序列化是 `null` 不是 `{}`）。
7. **字段命名**：JSON tag 用下划线风格，和现有 `cover_url` / `series_info` 一致，避免和别处拼写不一致。

---

## 7. 落地位置小结

| 问题 | 答案 |
| --- | --- |
| 结果写哪张表 | `crawler_tasks` 表的 `content`（JSON）+ `title` 列（通过 `engine.SaveResult` / `SaveContent`） |
| 用什么结构 | 复用脚手架 `models/content.go` 里的结构，或自定义 struct |
| 写在哪 | 默认直接写 `FetchHandler`；复用/复杂了再抽 repository；要按列查询才建独立表 |
| 建表要不要迁移 | 是，`papa.New(papa.WithModels(...))` 里注册（dev 环境） |
