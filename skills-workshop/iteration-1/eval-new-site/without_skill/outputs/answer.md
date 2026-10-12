# example-shop 站点接入方案

以下方案只改 `crawler` 业务项目，不修改 `papa` 框架源码。按当前项目约定，新增站点声明与独立 fetcher 包即可；`main.go` 已经匿名导入 `crawler/configs/sites` 并调用 `app.RegisterSites(papa.Sites()...)`，因此无需修改。

> 说明：题目未给出真实 HTML 样例，下面用常见且明确的选择器（`.book-card`、`.book-title` 等）展示关键代码。接入真实页面时，只需要按静态 HTML 调整选择器，错误分类、任务元数据、轮询和重试设计不变。

## 文件清单

建议新增：

```text
crawler/
├─ configs/sites/example_shop.go
├─ fetcher/example_shop/site.go
├─ fetcher/example_shop/fetch_catalog.go
├─ fetcher/example_shop/fetch_detail.go
├─ fetcher/example_shop/fetch_catalog_test.go   # 建议
└─ fetcher/example_shop/fetch_detail_test.go    # 建议
```

无需修改：

- `crawler/main.go`：站点声明通过 `init()` 自动登记。
- `crawler/crawlkit/crawlkit.go`：已有 `StructureError` 和 `PageRequestError` 正好满足本需求。
- `papa/**`：不得修改，也没有必要修改。

若只需将抓取结果保存在框架任务表的 `content` JSON 中，则不必新增业务表或改迁移清单。下面采用这种最小方案。

## 1. 站点声明

新增 `crawler/configs/sites/example_shop.go`：

```go
package sites

import (
    exampleshop "crawler/fetcher/example_shop"

    "github.com/ydtg1993/papa/v3"
)

func exampleShop() papa.SiteSpec {
    return papa.SiteSpec{
        Key:     "example-shop",
        BaseURL: "https://shop.example.com/",
        Headers: map[string]string{
            "User-Agent": "Mozilla/5.0 (compatible; example-shop-crawler/1.0)",
            "Accept":     "text/html,application/xhtml+xml",
        },
        Stages: []papa.StageSpec{
            {
                Fetcher:     &exampleshop.FetchCatalog{},
                WorkerCount: 1,
                QueueSize:   20,
                Delay:       "2s-5s",
                Retry:       papa.RetrySpec{MaxAttempts: 3, Backoff: "30s"},
                AutoStart:   true,
            },
            {
                Fetcher:     &exampleshop.FetchDetail{},
                WorkerCount: 2,
                QueueSize:   200,
                Delay:       "1s-3s",
                Retry:       papa.RetrySpec{MaxAttempts: 3, Backoff: "30s"},
            },
        },
    }
}

func init() { papa.RegisterSite(exampleShop()) }
```

`AutoStart: true` 让框架启动后调用分类 fetcher 的 `SubmitEntries`。网络错误返回普通 error 后，由各阶段自己的 `Retry` 配置分别重试，即“按阶段重试”。

## 2. 阶段名与站点辅助函数

新增 `crawler/fetcher/example_shop/site.go`：

```go
package example_shop

import (
    "fmt"

    "github.com/ydtg1993/papa/v3"
)

// 阶段名必须在整个程序中跨站唯一，所以必须带站点前缀。
const (
    StageCatalog = "example_shop_catalog"
    StageDetail  = "example_shop_detail"
)

func siteOf(engine *papa.Engine, task *papa.Task) (papa.Site, error) {
    site, ok := engine.Site(task.Site)
    if !ok {
        return papa.Site{}, papa.WrapNoRetryKind("configuration",
            fmt.Errorf("task %d references undeclared site %q", task.ID, task.Site))
    }
    return site, nil
}
```

这里没有使用通用的 `catalog` / `detail`，避免与现有站点阶段冲突。

## 3. 分类页：启动提交、每 8 小时轮询并派发详情任务

新增 `crawler/fetcher/example_shop/fetch_catalog.go`：

```go
package example_shop

import (
    "context"
    "fmt"
    "net/url"
    "regexp"
    "strings"
    "time"

    "crawler/crawlkit"
    "github.com/PuerkitoBio/goquery"
    "github.com/ydtg1993/papa/v3"
)

const booksCategoryPath = "/category/books"

// 详情路由严格限定为 /book/{id}。
var bookPathPattern = regexp.MustCompile(`^/book/([^/]+)/?$`)

type CatalogBook struct {
    BookID string `json:"book_id"`
    Title  string `json:"title"`
    URL    string `json:"url"`
}

type FetchCatalog struct{}

func (*FetchCatalog) GetStage() string { return StageCatalog }

// StageSpec.AutoStart=true 时，框架启动后自动调用。
func (*FetchCatalog) SubmitEntries(engine *papa.Engine, site papa.Site) {
    categoryURL := strings.TrimRight(site.BaseURL, "/") + booksCategoryPath
    _ = engine.SubmitTask(&papa.Task{
        URL:            categoryURL,
        Stage:          StageCatalog,
        Site:           site.Key,
        Meta:           map[string]string{"category": "books"},
        IdempotencyKey: "example-shop:category:books",
        Repeatable:     true,
        RepeatInterval: 8 * time.Hour,
    })
}

func (*FetchCatalog) FetchHandler(
    ctx context.Context,
    task *papa.Task,
    engine *papa.Engine,
) error {
    site, err := siteOf(engine, task)
    if err != nil {
        return err
    }

    categoryURL, err := crawlkit.ResolvePageURL(site.BaseURL, task.URL)
    if err != nil || !crawlkit.HasPathPrefix(categoryURL, booksCategoryPath) {
        return crawlkit.StructureError(
            fmt.Errorf("invalid books category URL %q: %w", task.URL, err))
    }

    page, err := engine.FetchHTML(ctx, categoryURL)
    if err != nil {
        // 超时、连接失败、5xx 等保持普通错误，由 example_shop_catalog
        // 阶段的 RetrySpec 重试；404/410 会标记为不可重试 not_found。
        return crawlkit.PageRequestError(StageCatalog, categoryURL, err)
    }

    books, err := parseCatalog(page.Selection(), page.URL.String(), site.BaseURL)
    if err != nil {
        // 列表、标题、链接、book id 缺失属于结构错误，不可重试。
        return crawlkit.StructureError(err)
    }

    children := make([]*papa.Task, 0, len(books))
    for _, book := range books {
        children = append(children, &papa.Task{
            PID:            task.ID,
            URL:            book.URL,
            Stage:          StageDetail,
            Site:           task.Site,
            Meta:           map[string]string{"book_id": book.BookID},
            IdempotencyKey: "example-shop:book:" + book.BookID,
        })
    }
    if len(children) > 0 {
        if err := engine.SubmitTasks(children); err != nil {
            return err
        }
    }

    return engine.SaveResult(task.ID, "books", books)
}

func parseCatalog(
    doc *goquery.Selection,
    pageURL string,
    baseURL string,
) ([]CatalogBook, error) {
    cards := doc.Find(".book-card")
    if cards.Length() == 0 {
        return nil, fmt.Errorf("catalog list .book-card missing on %s", pageURL)
    }

    books := make([]CatalogBook, 0, cards.Length())
    var parseErr error
    cards.EachWithBreak(func(index int, card *goquery.Selection) bool {
        title := crawlkit.NormalizeText(card.Find(".book-title").First().Text())
        if title == "" {
            parseErr = fmt.Errorf("book card %d is missing title", index)
            return false
        }

        href, ok := card.Find(`a[href^="/book/"]`).First().Attr("href")
        if !ok || strings.TrimSpace(href) == "" {
            parseErr = fmt.Errorf("book card %d is missing detail href", index)
            return false
        }

        detailURL, err := crawlkit.ResolvePageURL(baseURL, href)
        if err != nil {
            parseErr = fmt.Errorf("book card %d has invalid detail URL: %w", index, err)
            return false
        }
        parsed, err := url.Parse(detailURL)
        if err != nil {
            parseErr = fmt.Errorf("parse book detail URL %q: %w", detailURL, err)
            return false
        }
        match := bookPathPattern.FindStringSubmatch(parsed.Path)
        if len(match) != 2 || strings.TrimSpace(match[1]) == "" {
            parseErr = fmt.Errorf("book card %d does not use /book/{id}: %s", index, detailURL)
            return false
        }

        books = append(books, CatalogBook{
            BookID: match[1],
            Title:  title,
            URL:    detailURL,
        })
        return true
    })
    if parseErr != nil {
        return nil, parseErr
    }
    if len(books) == 0 {
        return nil, fmt.Errorf("catalog has no usable books on %s", pageURL)
    }
    return books, nil
}
```

关键点：

- 启动自动提交：`AutoStart: true` + `SubmitEntries`。
- 固定每 8 小时轮询：入口任务设置 `Repeatable: true` 与 `RepeatInterval: 8*time.Hour`。
- 详情任务明确携带 `Meta["book_id"]`；`Meta` 会随任务行持久化，可用于恢复和重投。
- `IdempotencyKey` 按业务 ID 去重，分类页周期抓取不会重复创建同一本书的详情任务。

## 4. 详情页：校验 book_id 并保存静态 HTML 结果

新增 `crawler/fetcher/example_shop/fetch_detail.go`：

```go
package example_shop

import (
    "context"
    "fmt"
    "net/url"
    "strings"

    "crawler/crawlkit"
    "github.com/PuerkitoBio/goquery"
    "github.com/ydtg1993/papa/v3"
)

type BookDetail struct {
    BookID      string `json:"book_id"`
    Title       string `json:"title"`
    Author      string `json:"author"`
    Price       string `json:"price"`
    Description string `json:"description"`
    URL         string `json:"url"`
}

type FetchDetail struct{}

func (*FetchDetail) GetStage() string { return StageDetail }

func (*FetchDetail) FetchHandler(
    ctx context.Context,
    task *papa.Task,
    engine *papa.Engine,
) error {
    site, err := siteOf(engine, task)
    if err != nil {
        return err
    }

    bookID := strings.TrimSpace(task.Meta["book_id"])
    if bookID == "" {
        return crawlkit.StructureError(fmt.Errorf("detail task is missing Meta.book_id"))
    }

    detailURL, err := crawlkit.ResolvePageURL(site.BaseURL, task.URL)
    if err != nil {
        return crawlkit.StructureError(fmt.Errorf("invalid detail URL %q: %w", task.URL, err))
    }
    parsed, err := url.Parse(detailURL)
    if err != nil {
        return crawlkit.StructureError(fmt.Errorf("parse detail URL %q: %w", detailURL, err))
    }
    match := bookPathPattern.FindStringSubmatch(parsed.Path)
    if len(match) != 2 {
        return crawlkit.StructureError(fmt.Errorf("detail URL must match /book/{id}: %s", detailURL))
    }
    if match[1] != bookID {
        return crawlkit.StructureError(fmt.Errorf(
            "detail URL book id %q differs from Meta.book_id %q", match[1], bookID))
    }

    page, err := engine.FetchHTML(ctx, detailURL)
    if err != nil {
        // 普通网络错误交给 example_shop_detail 阶段自己的 RetrySpec。
        return crawlkit.PageRequestError(StageDetail, detailURL, err)
    }

    detail, err := parseDetail(page.Selection(), page.URL.String(), bookID)
    if err != nil {
        return crawlkit.StructureError(err)
    }
    return engine.SaveResult(task.ID, detail.Title, detail)
}

func parseDetail(doc *goquery.Selection, pageURL, bookID string) (BookDetail, error) {
    detail := BookDetail{
        BookID:      bookID,
        Title:       crawlkit.NormalizeText(doc.Find("h1.book-title").First().Text()),
        Author:      crawlkit.NormalizeText(doc.Find(".book-author").First().Text()),
        Price:       crawlkit.NormalizeText(doc.Find(".book-price").First().Text()),
        Description: crawlkit.NormalizeText(doc.Find(".book-description").First().Text()),
        URL:         pageURL,
    }

    // 以下按业务必填字段处理。任一缺失都表示页面结构不符合预期，外层会
    // WrapNoRetryKind("structure", ...)，不会浪费阶段重试次数。
    switch {
    case detail.Title == "":
        return BookDetail{}, fmt.Errorf("detail title missing on %s", pageURL)
    case detail.Author == "":
        return BookDetail{}, fmt.Errorf("detail author missing on %s", pageURL)
    case detail.Price == "":
        return BookDetail{}, fmt.Errorf("detail price missing on %s", pageURL)
    case detail.Description == "":
        return BookDetail{}, fmt.Errorf("detail description missing on %s", pageURL)
    }
    return detail, nil
}
```

## 5. 错误与重试语义

必须保持以下边界：

```go
// 页面结构/必填字段/任务元数据缺失：永久错误，不重试。
return crawlkit.StructureError(err)

// 静态 HTTP 抓取失败：按当前阶段 RetrySpec 重试；404/410 不重试。
return crawlkit.PageRequestError(StageCatalog, pageURL, err)
return crawlkit.PageRequestError(StageDetail, pageURL, err)
```

现有 `crawlkit.StructureError` 内部使用：

```go
papa.WrapNoRetryKind("structure", err)
```

现有 `crawlkit.PageRequestError` 对 404/410 包装为不可重试 `not_found`，其余错误保持普通 error，因此会分别进入 `example_shop_catalog` 或 `example_shop_detail` 的阶段重试策略。

不要把结构错误直接 `return err`，否则框架会误认为网络类临时失败并重试；也不要把网络错误包装成 `WrapNoRetryKind("structure", ...)`。

## 6. 建议测试

为保证边界正确，至少增加以下表驱动测试：

- 分类页能从 `/book/123` 提取 `book_id=123`。
- 分类页缺 `.book-card`、标题或链接时，handler 返回不可重试错误。
- 详情任务缺 `Meta["book_id"]` 时不可重试。
- URL `/book/123` 与 `Meta.book_id=456` 不一致时不可重试。
- 详情页缺任一必填字段时不可重试。
- `engine.FetchHTML` 的超时/5xx 错误仍为可重试。
- 404/410 为不可重试。
- `GetStage()` 分别返回 `example_shop_catalog` 和 `example_shop_detail`。
- 入口任务具有 `Repeatable=true`、`RepeatInterval=8*time.Hour`。

完成后可在 `crawler` 目录运行：

```bash
go test ./...
papa check
```

`papa check` 无需启动数据库，可提前检查站点键、跨站阶段名、阶段并发/队列、延迟和重试配置是否合法。
