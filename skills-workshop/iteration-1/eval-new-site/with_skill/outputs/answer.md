# example-shop 接入方案

## 边界与前置证据

只改 `crawler/` 业务项目；**不改 `papa/` 框架源码，也不改 `crawler/main.go`**。现有 `main.go` 已匿名导入 `crawler/configs/sites`，并统一调用 `app.RegisterSites(papa.Sites()...)`，新站通过 `init()` 自登记即可。

题目只给了路由，没有提供可达页面或保存的 HTML，因此下面不会臆造 CSS 选择器。先用 `papa html`（必要时 `papa select`）采集真实页面，并保存为：

- `crawler/docs/samples/example_shop/catalog.html`
- `crawler/docs/samples/example_shop/detail.html`
- 可选：`catalog-empty.html`、404/结构异常样本

后文的 `parseCatalogPage` / `parseDetailPage` 是必须实现的纯解析函数，但其中选择器必须依据这些样本确定后才能视为生产可用。

## 文件清单

新增：

```text
crawler/configs/sites/example_shop.go
crawler/fetcher/example_shop/site.go
crawler/fetcher/example_shop/fetch_catalog.go
crawler/fetcher/example_shop/fetch_detail.go
crawler/fetcher/example_shop/fetch_catalog_test.go
crawler/fetcher/example_shop/fetch_detail_test.go
crawler/docs/samples/example_shop/catalog.html
crawler/docs/samples/example_shop/detail.html
```

按数据需求二选一：

- 若只需保存抓取结果：无需新增模型，详情阶段用 `engine.SaveResult`/`SaveContent` 写任务 `title/content`。
- 若以后需按书籍字段查询、关联、统计或后台筛选，再在 `crawler/models/` 增加模型与 repository，并注册到 `models.Models()`；本需求本身不足以强制新建业务表。

`crawler/configs/config.yaml` 只需确认全局 `repeat_queue` 已启用；当前项目已经有该段且 `enabled: true`，因此无需为本站增加阶段 YAML。若目标分支没有，则补全全局治理队列配置。不要增加 `crawler.stages` 或旧 `scheduler` 配置。

## 1. 站点与阶段声明

`crawler/configs/sites/example_shop.go`：

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
            "User-Agent": "crawler/1.0",
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

阶段名属于进程全局命名空间，必须统一使用带站点前缀的常量。`crawler/fetcher/example_shop/site.go`：

```go
package example_shop

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
        return papa.Site{}, papa.WrapNoRetryKind("configuration",
            fmt.Errorf("task %d references undeclared site %q", task.ID, task.Site))
    }
    return site, nil
}
```

`GetStage()` 和所有提交任务都只能引用上述常量，不能散落字符串：

```go
func (*FetchCatalog) GetStage() string { return StageCatalog }
func (*FetchDetail) GetStage() string  { return StageDetail }
```

## 2. 自动提交分类入口并每 8 小时轮询

`crawler/fetcher/example_shop/fetch_catalog.go` 的关键骨架：

```go
package example_shop

import (
    "context"
    "fmt"
    "net/http"
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
        // 配置常量错误；SubmitEntries 无 error 返回值，停止提交即可。
        return
    }
    if err := engine.SubmitTask(&papa.Task{
        URL:            catalogURL,
        Stage:          StageCatalog,
        Site:           site.Key,
        Meta:           map[string]string{"category_key": "books"},
        IdempotencyKey: "example-shop:catalog:books",
        Repeatable:     true,
        RepeatInterval: 8 * time.Hour,
    }); err != nil {
        // SubmitTask 已记录结构化日志；控制流上停止即可，不重复打印。
        return
    }
}

type catalogBook struct {
    BookID    string `json:"book_id"`
    Title     string `json:"title"`
    DetailURL string `json:"detail_url"`
}

func (*FetchCatalog) FetchHandler(ctx context.Context, task *papa.Task, engine *papa.Engine) error {
    site, err := siteOf(engine, task)
    if err != nil {
        return err
    }
    catalogURL, err := papa.ResolveURL(site.BaseURL, task.URL)
    if err != nil || !papa.SameHost(site.BaseURL, catalogURL) {
        return papa.WrapNoRetryKind("configuration",
            fmt.Errorf("invalid catalog URL %q: %w", task.URL, err))
    }

    page, err := engine.FetchHTML(ctx, catalogURL)
    if err != nil {
        return classifyFetchError(err)
    }
    if err := engine.RestrictedError(task, page, "example-shop catalog"); err != nil {
        return err
    }

    books, explicitlyEmpty, err := parseCatalogPage(page.Selection(), page.URL.String())
    if err != nil {
        return papa.WrapNoRetryKind("structure", err)
    }
    if len(books) == 0 && !explicitlyEmpty {
        return papa.WrapNoRetryKind("structure",
            fmt.Errorf("catalog contains no books and no verified empty-state marker"))
    }

    children := make([]*papa.Task, 0, len(books))
    for _, book := range books {
        detailURL, err := papa.ResolveURL(page.URL.String(), book.DetailURL)
        if err != nil || !papa.SameHost(site.BaseURL, detailURL) {
            return papa.WrapNoRetryKind("structure",
                fmt.Errorf("book %q has invalid detail URL %q", book.BookID, book.DetailURL))
        }
        // 解析器还应校验路径严格符合 /book/{id}，且 URL 中 id 与 BookID 一致。
        children = append(children, &papa.Task{
            PID:            task.ID,
            URL:            detailURL,
            Stage:          StageDetail,
            Site:           task.Site,
            Meta:           map[string]string{"book_id": book.BookID},
            IdempotencyKey: "example-shop:book:" + book.BookID,
        })
    }
    if len(children) > 0 {
        return engine.SubmitTasks(children)
    }
    return nil // 只有已确认的显式空分类才会走到这里
}

func classifyFetchError(err error) error {
    if code, ok := htmlfetch.StatusCode(err); ok {
        switch code {
        case http.StatusNotFound, http.StatusGone:
            return papa.WrapNoRetryKind("not-found", err)
        }
    }
    // 超时、连接失败、429、5xx 等保持普通 error，由 StageSpec.Retry 重试。
    return err
}
```

注意：

- `AutoStart: true` 只决定启动时是否调用 `SubmitEntries`。
- `Repeatable: true` + `RepeatInterval: 8*time.Hour` 是分类任务本身的周期；二者只在首次入库时播种，不覆盖后台后来调整的值。
- 自动轮询还要求 `configs/config.yaml` 的全局 `repeat_queue.enabled: true`。全局 `interval` 是扫描兜底，不应拿它表达本站的 8 小时周期。
- 自定义幂等键必须始终对应同一个 `(stage, URL)`；分类键固定为 books，详情键稳定地基于 `book_id`。
- 详情任务按题意是一次性任务，因此不设置 `Repeatable`。

## 3. 详情抓取与保存

`crawler/fetcher/example_shop/fetch_detail.go`：

```go
package example_shop

import (
    "context"
    "encoding/json"
    "fmt"
    "regexp"
    "strings"

    "github.com/ydtg1993/papa/v3"
)

var bookPath = regexp.MustCompile(`^/book/([^/]+)/?$`)

type FetchDetail struct{}

func (*FetchDetail) GetStage() string { return StageDetail }

type bookResult struct {
    BookID      string `json:"book_id"`
    Title       string `json:"title"`
    Description string `json:"description,omitempty"`
    SourceURL   string `json:"source_url"`
}

func (*FetchDetail) FetchHandler(ctx context.Context, task *papa.Task, engine *papa.Engine) error {
    site, err := siteOf(engine, task)
    if err != nil {
        return err
    }
    bookID := strings.TrimSpace(task.Meta["book_id"])
    if bookID == "" {
        return papa.WrapNoRetryKind("structure", errors.New("detail task missing book_id in Meta"))
    }

    detailURL, err := papa.ResolveURL(site.BaseURL, task.URL)
    if err != nil || !papa.SameHost(site.BaseURL, detailURL) {
        return papa.WrapNoRetryKind("configuration",
            fmt.Errorf("invalid detail URL %q: %w", task.URL, err))
    }
    // 用 net/url.Parse(detailURL).Path 与 bookPath 校验 /book/{id}，并确认捕获的 id == bookID。

    page, err := engine.FetchHTML(ctx, detailURL)
    if err != nil {
        return classifyFetchError(err)
    }
    if err := engine.RestrictedError(task, page, "example-shop detail"); err != nil {
        return err
    }

    result, err := parseDetailPage(page.Selection(), page.URL.String(), bookID)
    if err != nil {
        return papa.WrapNoRetryKind("structure", err)
    }
    if strings.TrimSpace(result.Title) == "" {
        return papa.WrapNoRetryKind("structure", errors.New("detail title missing"))
    }

    raw, err := json.Marshal(result)
    if err != nil {
        return papa.WrapNoRetryKind("serialization", err)
    }
    return engine.SaveResult(task, result.Title, string(raw))
}
```

上面省略的 `errors`、`net/url` import 应在落地时补上。若当前导出的 `SaveResult` 签名与示意不同，应以当前框架源码为准改成对应的 `SaveResult`/`SaveContent` 调用；不要因此绕过公开 API 或直接修改任务状态。

`parseCatalogPage` 与 `parseDetailPage` 应保持纯函数：接收 `*goquery.Selection` 和页面 URL，返回结构化数据或明确错误。规则是：

- 分类卡片缺少 `book_id`、标题或详情链接：返回结构错误。
- 详情页缺少必须字段：返回结构错误。
- 只有样本中存在且测试覆盖的明确“暂无书籍”文案，才能返回 `explicitlyEmpty=true`。
- 不用错误文本判断 HTTP 状态；404/410 通过 `htmlfetch.StatusCode` 标为 `not-found` 且不可重试。
- 网络超时、连接错误、429/5xx 等返回普通错误，由两个阶段各自的 `Retry` 策略处理。
- 静态 HTML 已满足需求，只使用 `engine.FetchHTML`，不启用 Rod，也不实现 `NeedsBrowser`。

## 4. 测试与验证

在保存真实 HTML 后增加表驱动测试：

1. `fetch_catalog_test.go`
   - 从 `catalog.html` 解析出预期书目；
   - 每条详情 URL 均为本站 HTTP(S)、路径为 `/book/{id}`；
   - `book_id` 提取正确；
   - 缺列表/缺 ID/缺标题时返回错误；
   - 只有 `catalog-empty.html` 的已验证空态才允许空结果；
   - 构造的详情任务使用 `StageDetail`、持久化 `Meta["book_id"]` 和稳定幂等键。
2. `fetch_detail_test.go`
   - 从 `detail.html` 解析标题和所需字段；
   - 缺标题或关键结构时报错；
   - URL 中 ID 与 `Meta.book_id` 不一致时报不可重试错误。
3. 声明/入口测试
   - `GetStage()` 分别等于 `example_shop_catalog`、`example_shop_detail`；
   - 分类阶段 `AutoStart=true`；
   - 入口任务 `Repeatable=true`、`RepeatInterval==8*time.Hour`；
   - 两阶段名与现有站点不冲突。
4. 错误分类测试
   - 404/410 => `not-found`、不可重试；
   - 普通网络错误和 5xx => 仍为可重试普通错误；
   - 结构缺失 => `structure`、不可重试。

建议依次运行：

```text
go test ./fetcher/example_shop/...
go test ./...
go vet ./...
papa check <crawler-project-path>
```

`papa check` 可无数据库检查配置和站点/阶段声明；只有需要验证数据库启动接线时才用 `papa check <path> --boot`。本方案不新增模型，所以通常无需迁移；若后续改为专表落库，则注册模型并显式运行 `papa migrate`，正常启动不应自动迁移。
