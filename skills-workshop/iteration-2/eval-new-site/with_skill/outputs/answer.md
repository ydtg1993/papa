# example-shop 接入方案

下面只改业务项目 `crawler/`。**不修改 `papa/` 框架源码，也不为新站修改 `crawler/main.go`**：现有 `main.go` 已匿名导入 `crawler/configs/sites`，并通过 `app.RegisterSites(papa.Sites()...)` 注册所有自登记站点。

> 说明：题目没有提供真实页面 HTML，而且 `shop.example.com` 是示例域名。所以下面给出的是可落地的任务流、错误分类和解析边界；CSS 选择器只标为占位符，不能声称已生产验证。正式接入前应保存 `/category/books` 与 `/book/{id}` 的真实 HTML 到测试夹具中，再据此替换选择器并运行测试。

## 文件清单

新增：

```text
crawler/configs/sites/example_shop.go
crawler/fetcher/exampleshop/site.go
crawler/fetcher/exampleshop/fetch_catalog.go
crawler/fetcher/exampleshop/fetch_detail.go
crawler/fetcher/exampleshop/parser.go
crawler/fetcher/exampleshop/parser_test.go
crawler/docs/samples/example-shop/catalog.html   # 获取真实 HTML 后加入
crawler/docs/samples/example-shop/detail.html    # 获取真实 HTML 后加入
```

需要确认/修改：

```text
crawler/configs/config.yaml   # 只确认全局 repeat_queue 已启用；不在 YAML 中声明 stage
```

无需修改：

```text
crawler/main.go
papa/**
```

本例将结果保存在 `crawler_tasks.title/content`，因此暂不新增 GORM 业务表；如果后续需要按书籍字段筛选、统计或建立关系，再将结构迁移到 `crawler/models/` 并通过 repository 落库。

## 1. 站点自登记与阶段声明

`crawler/configs/sites/example_shop.go`：

```go
package sites

import (
    exampleshopfetcher "crawler/fetcher/exampleshop"

    "github.com/ydtg1993/papa/v3"
)

func exampleShop() papa.SiteSpec {
    return papa.SiteSpec{
        Key:     "example-shop",
        BaseURL: "https://shop.example.com/",
        Headers: map[string]string{
            "User-Agent":      "Mozilla/5.0 (compatible; ExampleShopCrawler/1.0)",
            "Accept":          "text/html,application/xhtml+xml",
            "Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
        },
        Stages: []papa.StageSpec{
            {
                Fetcher:     &exampleshopfetcher.FetchCatalog{},
                WorkerCount: 1,
                QueueSize:   32,
                Delay:       "2s-5s",
                Retry: papa.RetrySpec{
                    MaxAttempts: 3,
                    Backoff:     "20s",
                },
                AutoStart: true,
            },
            {
                Fetcher:     &exampleshopfetcher.FetchDetail{},
                WorkerCount: 2,
                QueueSize:   256,
                Delay:       "1s-3s",
                Retry: papa.RetrySpec{
                    MaxAttempts: 3,
                    Backoff:     "20s",
                },
            },
        },
    }
}

func init() { papa.RegisterSite(exampleShop()) }
```

阶段参数留在 `StageSpec` 中，不新增 `crawler.stages`、`scheduler` 或 `RegisterCronJob`。阶段名是进程级全局名字，因此统一定义带站点前缀的常量。

## 2. 阶段常量与站点获取

`crawler/fetcher/exampleshop/site.go`：

```go
package exampleshop

import (
    "fmt"

    "github.com/ydtg1993/papa/v3"
)

const (
    StageCatalog = "example_shop_catalog"
    StageDetail  = "example_shop_detail"
)

func siteOf(engine *papa.Engine, task *papa.Task) (papa.Site, error) {
    site, ok := engine.Site(task.Site)
    if !ok {
        return papa.Site{}, papa.WrapNoRetryKind(
            "configuration",
            fmt.Errorf("task %d references undeclared site %q", task.ID, task.Site),
        )
    }
    return site, nil
}
```

`GetStage`、站点声明和所有子任务都只使用这两个常量，避免字符串漂移或与其他站点碰撞。

## 3. 分类入口：启动提交并每 8 小时轮询

`crawler/fetcher/exampleshop/fetch_catalog.go`：

```go
package exampleshop

import (
    "context"
    "fmt"
    "strings"
    "time"

    "github.com/ydtg1993/papa/v3"
    "github.com/ydtg1993/papa/v3/pkg/htmlfetch"
)

type FetchCatalog struct{}

func (*FetchCatalog) GetStage() string { return StageCatalog }

func (*FetchCatalog) SubmitEntries(engine *papa.Engine, site papa.Site) {
    catalogURL, err := papa.ResolveURL(site.BaseURL, "/category/books")
    if err != nil {
        // 入口接口无 error 返回值；非法的声明 URL 属于配置问题。
        // 实际项目可通过项目 logger 暴露该错误，但不要重复记录 SubmitTask 自己的错误。
        return
    }

    if err := engine.SubmitTask(&papa.Task{
        URL:            catalogURL,
        Site:           site.Key,
        Stage:          StageCatalog,
        Meta:           map[string]string{"category": "books"},
        IdempotencyKey: "example-shop:catalog:books",
        Repeatable:     true,
        RepeatInterval: 8 * time.Hour,
    }); err != nil {
        // SubmitTask 已记录结构化错误；停止入口提交即可。
        return
    }
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

    page, err := engine.FetchHTML(ctx, task.URL)
    if err != nil {
        return classifyFetchError("catalog", task.URL, err)
    }
    if err := engine.RestrictedError(task, page, "catalog"); err != nil {
        return err
    }

    books, empty, err := parseCatalog(page.Selection(), page.URL.String())
    if err != nil {
        return papa.WrapNoRetryKind("structure", err)
    }
    if empty {
        return engine.SaveResult(task.ID, "books catalog", catalogResult{Books: nil})
    }

    children := make([]*papa.Task, 0, len(books))
    for _, book := range books {
        bookID := strings.TrimSpace(book.ID)
        if bookID == "" {
            return papa.WrapNoRetryKind("structure", fmt.Errorf("catalog item has no book id"))
        }
        detailURL, err := papa.ResolveURL(page.URL.String(), book.Href)
        if err != nil {
            return papa.WrapNoRetryKind("structure", fmt.Errorf("resolve book %q URL: %w", bookID, err))
        }
        if !papa.SameHost(site.BaseURL, detailURL) {
            return papa.WrapNoRetryKind("structure", fmt.Errorf("book %q links outside example-shop: %s", bookID, detailURL))
        }

        children = append(children, &papa.Task{
            PID:            task.ID,
            URL:            detailURL,
            Site:           task.Site,
            Stage:          StageDetail,
            Meta:           map[string]string{"book_id": bookID},
            IdempotencyKey: "example-shop:book:" + bookID,
            // 详情任务是一次性任务，不设置 Repeatable。
        })
    }

    if len(children) > 0 {
        if err := engine.SubmitTasks(children); err != nil {
            return err
        }
    }
    return engine.SaveResult(task.ID, "books catalog", catalogResult{Books: books})
}

func classifyFetchError(stage, pageURL string, err error) error {
    wrapped := fmt.Errorf("%s: fetch %s: %w", stage, pageURL, err)
    if code, ok := htmlfetch.StatusCode(err); ok && (code == 404 || code == 410) {
        return papa.WrapNoRetryKind("not-found", wrapped)
    }
    // 超时、连接失败、临时代理问题、5xx 等保持普通 error，交给 StageSpec.Retry。
    return wrapped
}
```

关键语义：

- `AutoStart: true` 只负责让框架调用 `SubmitEntries`；真正入口任务由 fetcher 提交。
- `Repeatable: true` 与 `RepeatInterval: 8*time.Hour` 都在分类任务首次落库时播种；同一任务之后再次提交不会覆盖运营在后台修改过的轮询设置。
- 详情任务把 `book_id` 放在持久化的 `Task.Meta` 中，而不是编码进 URL；`IdempotencyKey` 以站点与业务主键组成，稳定且与该详情任务含义一致。
- 只有分类任务轮询；详情任务默认不轮询。

## 4. 详情抓取

`crawler/fetcher/exampleshop/fetch_detail.go`：

```go
package exampleshop

import (
    "context"
    "fmt"
    "strings"

    "github.com/ydtg1993/papa/v3"
)

type FetchDetail struct{}

func (*FetchDetail) GetStage() string { return StageDetail }

func (*FetchDetail) FetchHandler(
    ctx context.Context,
    task *papa.Task,
    engine *papa.Engine,
) error {
    if _, err := siteOf(engine, task); err != nil {
        return err
    }

    bookID := strings.TrimSpace(task.Meta["book_id"])
    if bookID == "" {
        return papa.WrapNoRetryKind(
            "structure",
            fmt.Errorf("detail task %d has no book_id in Meta", task.ID),
        )
    }

    page, err := engine.FetchHTML(ctx, task.URL)
    if err != nil {
        return classifyFetchError("detail", task.URL, err)
    }
    if err := engine.RestrictedError(task, page, "detail"); err != nil {
        return err
    }

    result, err := parseDetail(page.Selection(), page.URL.String())
    if err != nil {
        // 必填结构、字段或选择器缺失，重复请求不会修复，直接不可重试。
        return papa.WrapNoRetryKind("structure", err)
    }
    result.BookID = bookID

    return engine.SaveResult(task.ID, result.Title, result)
}
```

静态 HTML 已满足需求，两个阶段均使用 `engine.FetchHTML`，不引入 Rod，也不实现 `NeedsBrowser`。网络/超时/5xx 返回普通 error，由阶段的 `MaxAttempts: 3` 和 `Backoff: 20s` 重试；404/410、配置错误及确认的结构缺失使用 `WrapNoRetryKind`。错误分类依赖 `htmlfetch.StatusCode(err)`，不匹配错误字符串，也不手动调用 `task.UpdateStatus`。

## 5. 纯解析函数：选择器必须由真实 HTML 校准

`crawler/fetcher/exampleshop/parser.go`：

```go
package exampleshop

import (
    "fmt"
    "regexp"
    "strings"

    "github.com/PuerkitoBio/goquery"
)

type catalogBook struct {
    ID    string `json:"book_id"`
    Title string `json:"title"`
    Href  string `json:"href"`
}

type catalogResult struct {
    Books []catalogBook `json:"books"`
}

type detailResult struct {
    BookID     string `json:"book_id"`
    Title      string `json:"title"`
    Author     string `json:"author,omitempty"`
    Description string `json:"description,omitempty"`
}

// TODO: 必须以保存的真实页面文案确认，不可把这个示例词表直接视为已验证。
var explicitEmptyCatalog = regexp.MustCompile(`(?i)no books found|暂无图书|没有找到图书`)

func parseCatalog(doc *goquery.Selection, pageURL string) ([]catalogBook, bool, error) {
    // PLACEHOLDER：根据真实 catalog.html 替换成稳定的语义选择器。
    items := doc.Find(`[data-book-id]`)
    if items.Length() == 0 {
        body := strings.Join(strings.Fields(doc.Find("body").Text()), " ")
        if explicitEmptyCatalog.MatchString(body) {
            return nil, true, nil
        }
        return nil, false, fmt.Errorf("catalog structure missing on %s", pageURL)
    }

    books := make([]catalogBook, 0, items.Length())
    var parseErr error
    items.EachWithBreak(func(index int, item *goquery.Selection) bool {
        id, _ := item.Attr("data-book-id")
        // PLACEHOLDER：链接与标题选择器必须由真实 HTML 确认。
        link := item.Find(`a[href^="/book/"]`).First()
        href, ok := link.Attr("href")
        title := strings.Join(strings.Fields(link.Text()), " ")
        if strings.TrimSpace(id) == "" || !ok || strings.TrimSpace(href) == "" || title == "" {
            parseErr = fmt.Errorf("catalog item %d lacks book_id, href, or title", index)
            return false
        }
        books = append(books, catalogBook{ID: id, Title: title, Href: href})
        return true
    })
    if parseErr != nil {
        return nil, false, parseErr
    }
    return books, false, nil
}

func parseDetail(doc *goquery.Selection, pageURL string) (detailResult, error) {
    // PLACEHOLDER：以下字段选择器只展示 parser 边界，不是生产选择器。
    title := strings.Join(strings.Fields(doc.Find("h1").First().Text()), " ")
    if title == "" {
        return detailResult{}, fmt.Errorf("detail title missing on %s", pageURL)
    }
    return detailResult{
        Title:       title,
        Author:      strings.Join(strings.Fields(doc.Find(`[data-field="author"]`).First().Text()), " "),
        Description: strings.Join(strings.Fields(doc.Find(`[data-field="description"]`).First().Text()), " "),
    }, nil
}
```

不能把空匹配直接当成功：分类页只有命中经过真实页面确认的“明确空列表”文案时才能成功返回空结果；否则按结构缺失处理且不可重试。详情页标题等必填字段缺失同理。

## 6. 全局轮询队列

`crawler/configs/config.yaml` 中需要存在且启用全局治理队列；本项目当前已有这一段，可保留：

```yaml
repeat_queue:
  enabled: true
  worker_count: 1
  interval: "2h"   # 全局扫描兜底，不是该分类任务的实际周期
  batch_size: 100
```

分类任务的实际周期由 `RepeatInterval: 8 * time.Hour` 决定。若 `repeat_queue.enabled` 为 `false` 或整段缺失，任务即使 `Repeatable: true` 也不会自动轮询。开启 repeat 相关列后应执行一次 `papa migrate`，但不要在普通启动流程里自动迁移。

## 7. 测试要求

`crawler/fetcher/exampleshop/parser_test.go` 至少覆盖：

1. 保存的真实分类页：解析出预期 `book_id`、标题和详情链接。
2. 保存的真实详情页：解析出标题及所需字段。
3. 明确空分类页：只有确认的空态文案才返回 `empty=true`。
4. 结构破损分类页：零条目且无空态文案时返回错误。
5. 结构破损详情页：必填标题缺失时返回错误。
6. URL：相对 `/book/{id}` 正确解析，站外、非 HTTP(S) 链接被拒绝。
7. 声明与任务：`GetStage()` 分别为 `example_shop_catalog`、`example_shop_detail`；入口任务为 repeatable、8 小时周期，详情任务包含 `Meta["book_id"]` 和稳定幂等键。
8. 错误分类：404/410 不重试；模拟超时/5xx 保持普通错误以触发阶段重试。

建议验证顺序：

```text
cd crawler
go test ./fetcher/exampleshop/...
papa check .
go test ./...
go vet ./...
```

获得真实可访问页面后，再用 `papa html` 保存原始 HTML、`papa select` 验证选择器，并将样本固定为 parser fixture；在此之前不应把上面的占位选择器标记为生产可用。
