# Papa - 高性能分布式爬虫框架

> Papa 是一个基于 Go 语言和 [Rod](https://github.com/go-rod/rod) 的高性能、可扩展的浏览器自动化爬虫框架。
> 它内置了浏览器池、多阶段工作池、任务持久化与恢复、M3U8 视频下载（支持断点续传、自动合并）、文件下载、定时任务调度、Web 监控等特性，
> 适用于需要处理 JavaScript 渲染、反爬严格的网站以及流媒体资源的抓取与下载。

### 📐 架构设计
![整体架构图](https://github.com/ydtg1993/papa/blob/master/storage/architecture.png)

### ✨ 核心特性
- 🚀 **多阶段爬取** – 支持 `catalog` → `detail` 等多阶段流水线，每个阶段可独立配置并发数和队列大小。
- 🌐 **浏览器池** – 基于 Rod 封装浏览器池，支持无头/有头模式，自动管理浏览器生命周期。
- 💾 **任务持久化与恢复** – 基于 GORM 将任务状态持久化到 MySQL，支持断点续爬，引擎启动时自动恢复未完成或超时的任务。
- 📊 **可观测性** – 工作池提供活动事件通道，监控模块可实时统计各阶段任务执行情况（成功/失败/耗时），并提供 Web 界面与 JSON API。
- ⚙️ **灵活配置** – 通过 YAML 配置文件设置各阶段 worker 数量、队列大小、重试次数、浏览器参数等。
- ⏰ **定时任务调度** – 基于 Cron 表达式，支持周期执行 `catalog` 轮询（如每日检查新视频）、`recover` 恢复未完成任务等。
- 🔧 **可扩展** – 清晰的接口设计（`Fetcher`），方便自定义抓取逻辑和下载器。
- 📦 **M3U8 下载器** – 高性能 M3U8 视频下载模块，支持：
  - 多线程并发下载
  - 断点续传（任务级 + 片段级）
  - AES-128 解密（自动处理 PKCS#7 填充）
  - 自动合并为 MP4（需 ffmpeg）
  - 多码率自适应（自动选择最高码率）
- 📎 **通用文件下载器** – 支持 HTTP Range 分片并发下载、断点续传、进度回调，适用于图片、音频、普通视频等文件。
- 🔄 **代理轮换** – 集成代理管理器，支持从 API 动态获取代理列表并轮换使用。

#### 模块说明
| 模块 | 描述 |
| :--- | :--- |
| **Engine** | 核心引擎，管理多个爬取阶段，负责任务注册、提交、恢复和生命周期控制。 |
| **Stage** | 每个阶段包含一个独立的工作池（WorkerPool）和对应的抓取处理器（Fetcher），用户需在 FetchHandler 中调用 SubmitTask 实现阶段跳转。 |
| **WorkerPool** | 泛型工作池，消费任务队列，调用 Fetcher 执行具体抓取逻辑，并发布活动事件供监控。 |
| **Fetcher** | 业务实现接口，每个阶段需实现 GetStage + FetchHandler 方法，负责页面抓取和链接解析。 |
| **Browser Pool** | 管理 Rod 浏览器实例，支持代理注入、空闲回收，提供 Get/Put 方法。 |
| **Monitor** | 消费 WorkerPool 的活动事件，统计任务执行情况（按 worker 和全局），并通过 HTTP 服务展示。 |
| **Scheduler** | 基于 Cron 的定时任务调度器，支持周期性提交 catalog 任务或执行恢复任务。 |
| **Database** | 通过 GORM 连接 MySQL，存储任务状态（crawler_tasks）和页面数据。 |
| **Proxy Manager** | 从 API 获取代理列表，轮询返回，支持定时刷新。 |
| **M3U8 downloader** | 下载 M3U8 视频流，支持切片合并、解密、断点续传。 |
| **File Downloader** | 下载普通文件（图片、音频、MP4 等），支持分片并发和断点续传。 |

#### 数据流
1. **任务提交**：入口（`main.go`）调用 `engine.SubmitTask`，任务先写入数据库（状态 `pending`），然后提交到对应阶段的队列。
2. **任务处理**：Worker 从队列获取任务，调用 Fetcher 的 `FetchHandler`。处理前将任务状态更新为 `processing`，成功后更新为 `success`，失败则重试（最多 `MaxAttempts` 次），最终状态为 `failed`。
3. **阶段流转**：Fetcher 在解析页面后，可通过 `engine.SubmitTask` 将新任务提交到下一阶段（如 `detail`）。
4. **恢复机制**：引擎启动时调用 `RecoverTasks`，加载所有 `pending` 或超时 `processing` 的任务，重置状态后重新提交。
5. **监控**：WorkerPool 将任务开始/结束事件发送到 `Activities` 通道，Monitor 消费并更新统计。
6. **定时任务**：Scheduler 根据配置的 Cron 表达式，定时执行 `catalog` 提交或 `recover` 恢复，实现自动化维护。

📁 目录结构

      ├── papa.go                 # 门面包：papa.New() + 类型别名（Fetcher/Task/Engine/Config/CrawlerTask）
      ├── app/                    # 应用组装（依赖注入、启动、选项）
      ├── config/                 # 配置加载（viper）
      ├── crawler/                # 引擎核心（Engine, Task, Fetcher）
      ├── models/                 # 数据模型（CrawlerTask）
      ├── scheduler/              # 定时任务调度器（cron jobs）
      ├── server/                 # Web 监控服务（HTML + JSON API）
      ├── pkg/                    # 公共可复用包
      │   ├── browser/            # 浏览器池（基于 rod）
      │   ├── database/           # 数据库连接（GORM）
      │   ├── htmlfetch/          # 静态 HTML 抓取（goquery）
      │   ├── loggers/            # 日志封装（lumberjack + logrus）
      │   ├── middleware/         # 下载中间件
      │   │   ├── filedown/       # 文件下载器
      │   │   ├── m3u8/           # M3U8 视频下载器
      │   │   └── proxy/          # 代理管理器
      │   ├── queue.go            # 通用消息队列（错误/活动）
      │   ├── track/              # 监控统计（StatsQueue）
      │   └── workerpool/         # 泛型工作池
      ├── cmd/papa/               # 脚手架 CLI：papa new <name> 生成新爬虫项目
      ├── configs/                # 示例配置文件
      ├── logs/                   # 日志文件目录（运行时生成）
      ├── scripts/                # 辅助脚本（Docker、数据库迁移等）
      ├── storage/                # 其他存储（架构图等）
      ├── go.mod
      └── go.sum


### 🚀 快速开始

Papa 是框架包，你在**自己的项目里 `import "github.com/ydtg1993/papa"`** 使用它，不需要 clone 这个仓库。

#### 环境要求
- Go 1.21+
- MySQL 5.7+ 或 8.0
- Chrome/Chromium 浏览器（用于 Rod，可自动下载或指定路径）
- （可选）ffmpeg（用于自动合并 MP4）

#### 方式一：用脚手架生成新项目（推荐）

**框架作者本地验证（无需发布版本）** —— 先安装脚手架到本机，再用 `--replace` 指向本地仓库：

```bash
go install ./cmd/papa        # 一次性安装，得到 papa 命令

cd /some/where
papa new mycrawler --replace E:/go-project/papa   # 生成的 go.mod 直接指向本地 papa 仓库
cd mycrawler && go mod tidy && go run .
```

> 不想安装就直接 `go run ./cmd/papa new mycrawler --replace ..`（在 papa 仓库根目录下运行，`..` 指回 papa 仓库）。

**外部用户（框架发布后）** —— 从 GitHub 拉取脚手架：

```bash
go run github.com/ydtg1993/papa/cmd/papa@latest new mycrawler
cd mycrawler
go mod tidy && go run .
```

完整流程见下方「具体怎么用」一节。

#### 方式二：在已有项目里手动引入

```bash
cd your-project
go get github.com/ydtg1993/papa@latest
```

然后在 `main.go` 里 `import "github.com/ydtg1993/papa"`，用 `papa.New()` 创建应用、注册阶段即可。

#### 运行内置示例（可选）

只想跑仓库自带的动漫爬虫示例时，才需要 clone 本仓库：

```bash
git clone https://github.com/ydtg1993/papa.git
cd papa
go run ./examples/video
```

#### 配置 / 初始化数据库

编辑 `configs/config.yaml`（脚手架生成，含中文注释），改数据库连接、爬虫阶段、浏览器参数等。

首次运行前把 `app.env` 设为 `dev` 以自动迁移表结构：

```yaml
# configs/config.yaml
app:
  env: dev   # loc/dev/prod，dev 时自动迁移表结构
```

### 📖 具体怎么用（从零写一个爬虫）

#### 1. 生成项目骨架

```bash
# 本地验证（推荐）：先 go install ./cmd/papa，再
papa new mycrawler --replace /path/to/papa
# 或外部拉取：go run github.com/ydtg1993/papa/cmd/papa@latest new mycrawler

cd mycrawler
go mod tidy
```

生成结构：

```
mycrawler/
├── main.go               # 入口：papa.New() + RegisterStage
├── configs/config.yaml   # 配置
├── fetcher/fetcher.go    # 两个 fetcher 伪代码（catalog / detail）
├── models/content.go     # 业务内容结构
└── logs/                 # 日志目录
```

#### 2. 改配置 `configs/config.yaml`

改数据库连接；`crawler.stages` 已预置 `catalog`、`detail` 两个阶段，可按需调并发/队列/延迟/重试：

```yaml
crawler:
  target: "https://example.com/"   # 起始目标站（main.go 里拼起始 URL 用）
  stages:
    catalog:
      worker_count: 1
      queue_size: 20
      delay: "5m"
      retry: { max_attempts: 3, backoff: "30s" }
    detail:
      worker_count: 3
      queue_size: 500
      delay: "3m"
      retry: { max_attempts: 3, backoff: "30s" }
db:
  dsn: "root:123456@tcp(127.0.0.1:3306)/crawler?charset=utf8mb4&parseTime=True&loc=Local"
```

#### 3. 写 fetcher 逻辑（`fetcher/fetcher.go`）

每个 fetcher 实现 `papa.Fetcher`：`GetStage()` 返回阶段名，`FetchHandler` 写抓取逻辑。

```go
package fetcher

import (
    "context"
    "time"

    "github.com/ydtg1993/papa"
)

// 阶段一：目录页 —— 抓详情链接，派发子任务
type FetchCatalog struct{}

func (f *FetchCatalog) GetStage() string { return "catalog" }

func (f *FetchCatalog) FetchHandler(ctx context.Context, task *papa.Task, engine *papa.Engine) error {
    bw, err := engine.GetBrowserPool().Get(ctx)   // 取一个浏览器实例
    if err != nil {
        return err
    }
    defer engine.GetBrowserPool().Put(bw)         // 用完归还

    page := bw.Browser.MustPage("")
    defer page.Close()
    if err := page.Context(ctx).Timeout(30*time.Second).Navigate(task.URL); err != nil {
        return err
    }
    page.MustWaitLoad()

    // 遍历列表项，把详情链接派发到下一阶段
    for _, item := range page.MustElements(".item a") {
        href, err := item.Attribute("href")
        if err != nil || href == nil {
            continue
        }
        engine.SubmitTask(&papa.Task{PID: task.ID, URL: *href, Stage: "detail"})
    }
    return nil
}

// 阶段二：详情页 —— 抓字段，写回数据库
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
    // 结果写回当前任务的 title 列（content 结构见 models/content.go）
    return engine.GetDB().Model(&papa.CrawlerTask{}).
        Where("id = ?", task.ID).
        Update("title", title).Error
}
```

#### 4. 注册阶段 + 提交起始任务（`main.go`）

`papa new` 已生成好 `main.go`，把起始 URL 填上即可：

```go
app, err := papa.New()
// ...

app.RegisterStage(&fetcher.FetchCatalog{},
    func(engine *papa.Engine) {
        engine.SubmitTask(&papa.Task{
            URL:        app.Config.Crawler.Target + "list", // 起始列表页
            Stage:      "catalog",
            Repeatable: true, // 可重复轮询
        })
    })
app.RegisterStage(&fetcher.FetchDetail{}, nil)
```

#### 5. 跑起来 + 看监控

```bash
go run .
```

- 日志写在 `logs/`。
- 监控页：`http://localhost:9090/monitor`（`server.monitor: true`），可看各阶段任务统计。
- 定时重抓 / 恢复失败：由 `config.yaml` 的 `scheduler.jobs` 驱动（`repeat` 每日重跑轮询任务，`recover` 恢复超时任务）。

### 🔧 中间件与下载器（可选）

需要代理、m3u8 视频、文件下载时，在 `RegisterStage` 之前设置：

```go
import (
    "github.com/ydtg1993/papa/pkg/middleware/m3u8"
    "github.com/ydtg1993/papa/pkg/middleware/filedown"
    "github.com/ydtg1993/papa/pkg/middleware/proxy"
)

app.Engine.SetProxy(proxy.NewManager(app.Config.Proxy.APIURL, 8*time.Minute))
app.Engine.SetM3U8(m3u8.NewDownloader(m3u8.DefaultConfig()))
app.Engine.SetFiledown(filedown.NewDownloader(filedown.DefaultConfig()))
```

fetcher 里取用：

```go
// m3u8 视频下载
res := engine.GetM3U8().Download(ctx, m3u8URL, "downloads/video", "video.ts", &m3u8.DownloadOptions{Referer: task.URL})
// 文件下载（图片/附件等）
res := engine.GetFiledown().Download(ctx, coverURL, "covers", "")
```

> 更详细的抓取技巧与数据落地见 [docs/FETCHER_WRITING_GUIDE.md](docs/FETCHER_WRITING_GUIDE.md) 和 [docs/DATA_LANDING_GUIDE.md](docs/DATA_LANDING_GUIDE.md)。

### 📝 注意事项
- 请遵守目标网站的 robots.txt 和法律法规，合理设置爬取频率。
- 若使用代理，确保代理 API 返回的代理列表可用且稳定。
- 生产环境建议开启 headless: true 以节省资源，并根据需要调整 pool_size。
- 数据库连接池参数请根据实际负载调整。
- 自动合并 MP4 需要系统安装 ffmpeg，若不使用可设置 auto_merge: false

### 本项目基于[MIT license](https://github.com/ydtg1993/papa/LICENSE.txt)开源
