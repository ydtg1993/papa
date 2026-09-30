# Papa - 高性能分布式爬虫框架

> Papa 是一个基于 Go 语言和 [Rod](https://github.com/go-rod/rod) 的高性能、可扩展的浏览器自动化爬虫框架。
> 它内置了浏览器池、多阶段工作池、任务持久化与恢复、M3U8 视频下载（支持断点续传、自动合并）、文件下载、定时任务调度、Web 监控等特性，
> 适用于需要处理 JavaScript 渲染、反爬严格的网站以及流媒体资源的抓取与下载。

### ✨ 核心特性
- 🚀 **多阶段爬取** – 支持 `catalog` → `detail` 等多阶段流水线，每个阶段可独立配置并发数和队列大小。
- 🌐 **浏览器池** – 基于 Rod 封装浏览器池，支持无头/有头模式，自动管理浏览器生命周期。
- 💾 **任务持久化与恢复** – 基于 GORM 将任务状态持久化到 MySQL，支持断点续爬，引擎启动时自动恢复未完成或超时的任务。
- 📊 **可观测性** – 工作池提供活动事件通道，监控模块可实时统计各阶段任务执行情况（成功/失败/耗时），并提供 Web 界面与 JSON API。
- ⚙️ **灵活配置** – 通过 YAML 配置文件设置各阶段 worker 数量、队列大小、重试次数、浏览器参数等。
- ⏱️ **延迟投递与随机间隔** – 任务支持 `NotBefore`/`Delay` 延迟投递（到点才入队，不空占 worker）；阶段 `delay` 支持 `"10s-30s"` 随机区间，反爬更隐蔽。
- 🛡️ **错误分类与告警** – 区分「可重试 / 不可重试」错误，失败时结构化记录 `stage/task_id/url/retry/kind` 并触发告警 hook（webhook/钉钉）。
- 🔁 **失败任务错误队列** – 自动或手动把 `failed` 任务重新投递回各自阶段，带并发数与「再处理代数」上限，防止永久坏任务无限重试。
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
4. **恢复机制**：调度器按配置定时执行 `recover` 任务，把 `pending` 或超时 `processing` 的任务重置状态后重新提交。
5. **监控**：WorkerPool 将任务开始/结束事件发送到 `Activities` 通道，Monitor 消费并更新统计。
6. **定时任务**：Scheduler 根据配置的 Cron 表达式，定时执行 `catalog` 提交或 `recover` 恢复，实现自动化维护。

📁 目录结构

      ├── papa.go                 # 门面包：papa.New() + 类型别名（App/Fetcher/Task/Engine/Config）
      ├── config/                 # 配置加载（viper）
      ├── crawler/                # 引擎核心（Engine, Task, Fetcher）
      ├── models/                 # 数据模型（CrawlerTask）
      ├── docs/                   # 使用手册（脚手架复制到业务项目）
      ├── internal/               # 私有实现（不对外暴露，仅门面/内部引用）
      │   ├── app/                # 应用组装（依赖注入、启动、选项）
      │   ├── dataadmin/          # 通用数据浏览注册表
      │   ├── database/           # 数据库连接（GORM）
      │   ├── metrics/            # 业务自定义监控数据
      │   ├── msgqueue/           # 通用消息队列（错误/活动）
      │   ├── scheduler/          # 定时任务调度器（cron jobs）
      │   ├── server/             # Web 监控服务（HTML + JSON API）
      │   ├── sysinfo/            # 系统指标采集
      │   ├── track/              # 监控统计（StatsQueue）
      │   └── workerpool/         # 泛型工作池
      ├── pkg/                    # 公开可复用库（引擎 API 会暴露其类型）
      │   ├── browser/            # 浏览器池（基于 rod）
      │   ├── htmlfetch/          # 静态 HTML 抓取（goquery）
      │   ├── loggers/            # 日志封装（lumberjack + logrus）
      │   ├── notify/             # 告警通知器（webhook，可接钉钉）
      │   └── middleware/         # 下载中间件
      │       ├── filedown/       # 文件下载器
      │       ├── m3u8/           # M3U8 视频下载器
      │       └── proxy/          # 代理管理器
      ├── cmd/papa/               # 脚手架 CLI：papa new <name> 生成新爬虫项目
      ├── scripts/                # 辅助脚本（Makefile）
      ├── go.mod
      └── go.sum


### 🚀 快速开始

Papa 是框架包，你在**自己的项目里 `import "github.com/ydtg1993/papa/v2"`** 使用它，不需要 clone 这个仓库。

#### 环境要求
- Go 1.25+
- MySQL 5.7+ 或 8.0
- Chrome/Chromium 浏览器（用于 Rod，可自动下载或指定路径）
- （可选）ffmpeg（用于自动合并 MP4）

#### 方式一：用脚手架生成新项目（推荐）

**框架作者本地验证（无需发布版本）** —— 先安装脚手架到本机，再用 `--replace` 指向本地仓库：

```bash
go install ./cmd/papa        # 一次性安装，得到 papa 命令

cd /some/where
papa new mycrawler --replace /path/to/papa   # 生成的 go.mod 直接指向本地 papa 仓库
cd mycrawler && go mod tidy && go run .
```

> 不想安装就直接 `go run ./cmd/papa new mycrawler --replace ..`（在 papa 仓库根目录下运行，`..` 指回 papa 仓库）。

**外部用户（框架发布后）** —— 从 GitHub 拉取脚手架：

```bash
go run github.com/ydtg1993/papa/v2/cmd/papa@latest new mycrawler
cd mycrawler
go mod tidy && go run .
```

完整流程见下方「具体怎么用」一节。

#### 方式二：在已有项目里手动引入

```bash
cd your-project
go get github.com/ydtg1993/papa/v2@latest
```

然后在 `main.go` 里 `import "github.com/ydtg1993/papa/v2"`，用 `papa.New()` 创建应用、注册阶段即可。

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
# 或外部拉取：go run github.com/ydtg1993/papa/v2/cmd/papa@latest new mycrawler

cd mycrawler
go mod tidy
```

生成结构：

```
mycrawler/
├── main.go                   # 入口：papa.New() + RegisterStage
├── configs/config.yaml       # 配置（预置 catalog 一个阶段）
├── fetcher/fetch_catalog.go  # 一个 fetcher 伪代码（catalog）
├── models/content.go         # 业务内容结构（极简起点）
├── docker/                   # Dockerfile + docker-compose.yml（开发容器）
├── docs/                     # 使用手册（写 fetcher / model 的指南）
├── Makefile                  # build / run / docker-up 等快捷命令
└── logs/                     # 日志目录
```

#### 2. 改配置 `configs/config.yaml`

改数据库连接；`crawler.stages` 已预置 `catalog` 一个阶段，可按需调并发/队列/延迟/重试：

```yaml
crawler:
  target: "https://example.com/"   # 起始目标站（main.go 里拼起始 URL 用）
  stages:
    catalog:
      worker_count: 1
      queue_size: 20
      delay: "10s-30s"             # 任务间隔：固定 "5m" 或随机区间 "10s-30s"
      retry: { max_attempts: 3, backoff: "30s" }
db:
  dsn: "root:123456@tcp(127.0.0.1:3306)/crawler?charset=utf8mb4&parseTime=True&loc=Local"
```

#### 3. 写 fetcher 逻辑（`fetcher/fetch_catalog.go`）

每个 fetcher 实现 `papa.Fetcher`：`GetStage()` 返回阶段名，`FetchHandler` 写抓取逻辑。

```go
package fetcher

import (
    "context"
    "time"

    "github.com/ydtg1993/papa/v2"
)

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

    // 提取字段
    title := page.MustElement("h1").MustText()

    // 结果写回当前任务（title 列 + content 列）
    return engine.SaveResult(task.ID, title, map[string]any{"title": title})
}
```

要抓多个阶段（列表 → 详情 → 视频），就再写一个 fetcher、在 `config.yaml` 加一个 stage、在 `main.go` 再 `RegisterStage` 一次，并在 catalog 里 `engine.SubmitTask(&papa.Task{PID: task.ID, URL: detailURL, Stage: "detail"})` 派发子任务（完整多阶段示例见 [FETCHER_WRITING_GUIDE.md](FETCHER_WRITING_GUIDE.md)）。

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
```

#### 5. 跑起来 + 看监控

```bash
go run .
```

- 日志写在 `logs/`。
- 监控页：`http://localhost:9090/monitor`（`server.monitor: true`），OA 后台布局，Dashboard 看机器 CPU/内存/磁盘、业务目录（downloads/logs）占用、任务队列概览。
  - `server.auth_key` / `server.auth_key_file`：访问密钥，`papa new` 已自动生成 `configs/secret` 密钥文件；密钥文件优先于内联 `auth_key`，两者都空则不校验。
  - `server.whitelist` / `server.whitelist_file`：来源 IP/CIDR 白名单。`whitelist_file`（`papa new` 生成 `configs/whitelist`）优先于内联 `whitelist`，设置页动态改白名单会写回该文件、重启后仍生效。
  - `server.monitor_dirs`：监控页「业务目录占用」要统计的目录（`name: path`）。
  - 业务自定义数据：fetcher 里调 `engine.RecordMetric("key", value)`，监控页「自定义数据」模块实时展示。
  - 「设置」模块：动态改白名单、重新生成登录密钥、优雅退出爬虫、导出日志。
- 定时重抓 / 恢复失败：由 `config.yaml` 的 `scheduler.jobs` 驱动（`repeat` 每日重跑轮询任务，`recover` 恢复超时任务）。

### 监控后台设置 API

所有接口都在密钥 + 白名单校验之后（密钥见上）。写接口均为 `POST`，请求头带 `Authorization: Bearer <key>`（或 `X-Auth-Key`，或 `?key=`）。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/settings` | 返回当前白名单、密钥/白名单文件路径与是否存在、日志目录 |
| POST | `/api/settings/whitelist` | 更新白名单并持久化到 `whitelist_file`；body `{"whitelist": ["127.0.0.1","10.0.0.0/8"]}` |
| POST | `/api/settings/secret` | 重新生成密钥并写回 `auth_key_file`；返回 `{"key": "<新密钥>"}` |
| POST | `/api/settings/shutdown` | 触发优雅退出 |
| POST | `/api/errorqueue/process` | 手动触发失败任务错误队列处理；返回 `{"status":"ok","processed":N}` |
| GET | `/api/logs` | 列出日志目录文件 |
| GET | `/api/logs/download` | 下载日志；`?file=name` 下载单个，缺省打包全部为 zip |

> 白名单文件为纯文本：每行一个 IP/CIDR，`#` 开头为注释，留空 = 不限制。动态改动会写回该文件，重启后仍生效。

### 数据浏览 API（通用 model 浏览）

把业务 model 登记进 OA，即可在「数据浏览」模块分页/搜索/筛选/排序查看。注册在代码层完成：

```go
app, err := papa.New(
    papa.WithBrowsableModels(
        papa.ModelDef{Key: "episode", Label: "剧集", Model: &models.Episode{}},
    ),
)
```

#### ModelDef 字段

| 字段 | 说明 |
| --- | --- |
| `Key` | URL 安全标识（字母/数字/下划线/连字符），是 `/api/data/{key}` 的路径段，**别用中文或空格** |
| `Label` | 后台「数据浏览」下拉框里的展示名，留空则回退用 `Key` |
| `Model` | **GORM 表模型的指针**（对应一张表）。注意：像 `DetailContent` 那种序列化进 `content` 列的 JSON 结构不是表模型，不能注册 |

#### 列能力自动判定

框架用 GORM schema 内省 `Model` 的字段，自动决定每列的能力：

| 字段类型 | 模糊搜索（`search`） | 排序（`sort`） | 等值筛选（`filter`） |
| --- | --- | --- | --- |
| 字符串 | ✅ LIKE | ✅ | ✅ |
| 数字 / bool / 时间 | ❌ | ✅ | ✅ |
| JSON / 其它 | ❌ | ❌ | ❌（只读展示） |

#### 建表与 `WithModels` 的关系

`WithBrowsableModels` 的模型在 `app.env: dev` 时会被自动迁移建表，**无需再写一次 `WithModels`**。`WithModels` 只用于「要建表但不想在后台浏览」的模型。

#### 后台怎么看

监控后台「数据浏览」模块：下拉框选模型 → 搜索框模糊搜 → 列筛选 → 点表头排序 → 翻页。框架默认已登记 `task`（任务表 `crawler_tasks`）。

#### 接口

（同样走密钥 + 白名单）：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/data/models` | 列出可浏览模型及列元数据 |
| GET | `/api/data/:model` | 分页列表；query：`page`(1 起)、`size`(默认 20，上限 200)、`search`(字符串列 LIKE)、`sort`(`col` / `-col`)、`filter[col]=val`(可重复，等值) |

示例：`/api/data/episode?page=1&size=20&search=火影&sort=-id&filter[downloaded]=true`

> 只读，无写端点。`sort`/`filter`/`search` 的列名均走白名单校验，非法列会被忽略；未登记 model 返回 404。

### 🔧 中间件与下载器（可选）

需要代理、m3u8 视频、文件下载时，在 `RegisterStage` 之前设置：

```go
import (
    "github.com/ydtg1993/papa/v2/pkg/middleware/m3u8"
    "github.com/ydtg1993/papa/v2/pkg/middleware/filedown"
    "github.com/ydtg1993/papa/v2/pkg/middleware/proxy"
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

### 🛡️ 错误分类、延迟投递与告警

- **不可重试错误**：fetcher 里 `return papa.WrapNoRetryKind("structure", err)`（或 `papa.WrapNoRetry(err)`），引擎**不重试**，直接把任务标 `failed` 并触发告警——适合结构错误 / 404 / 访问受限这类重试无意义的失败；普通 `error` 仍按配置自动重试。
- **业务键解耦**：`Task.Meta map[string]string` 承载 `series_id`/`episode_id` 等业务键，不再塞进 URL；`Task.IdempotencyKey` 可自定义去重键（默认 `stage|url`）。
- **延迟投递**：`Task.NotBefore` / `Task.Delay` 让任务到点才入队（episode 反爬要 10–30s 随机间隔时，直接设 `Delay`，别在 handler 里空等）。
- **告警 hook**：`engine.AddNotifier(notify.NewWebhook("https://..."))`，任务最终失败时自动推送 `AlertEvent`（含 `stage/task_id/url/retry/kind/message`）。
- **便捷 API**：`engine.FetchRendered(ctx, url, waitSelector)` 一次包好借浏览器→导航→等 DOM→解析→最终 URL；`engine.Upsert(record, conflictCols, updateCols)` 冲突更新并回填主键；`engine.SubmitTasks([]*Task)` 批量投递（事务化入库）。

### 🔁 失败任务错误队列（自动 / 手动再处理）

任务重试耗尽后进入 `status = failed`，持久化在 `crawler_tasks` 表里不会丢。框架内置一个「错误队列」把它重新投递回各自阶段，受 `error_queue` 配置控制：

```yaml
error_queue:
  enabled: false      # 是否启用错误队列处理
  worker_count: 2     # 并发重新投递失败任务的数量
  interval: "10m"     # 自动轮询间隔；0 = 不自动轮询，仅手动触发
  max_retry: 3        # 单个失败任务最多再处理代数；0 = 不限
```

- **自动轮询**：`enabled: true` 且 `interval` 非 0 时，后台按间隔自动把失败任务重置为 `pending` 并重新入队。
- **手动触发**：`interval: 0`（或任何时刻）可在 OA 后台「设置 → 错误队列处理」点「手动处理失败任务」，或直接 `POST /api/errorqueue/process`。
- **再处理代数上限**：每次重新投递会把该任务的 `reprocess + 1`；超过 `max_retry` 的任务不再投递，避免「结构错误 / 404」这类永久坏任务无限空转。配合 #错误分类的 `WrapNoRetryKind` 使用更精准（如只重试 `retryable`/`protected`，跳过 `structure`/`not-found`）。
- **业务自定义**：`engine.ProcessErrorQueue()` 已公开，可在你自己的 cron / 调度里直接调用，按需加过滤条件（如按阶段、按错误 kind）。

### 📝 注意事项
- 请遵守目标网站的 robots.txt 和法律法规，合理设置爬取频率。
- 若使用代理，确保代理 API 返回的代理列表可用且稳定。
- 生产环境建议开启 headless: true 以节省资源，并根据需要调整 pool_size。
- 数据库连接池参数请根据实际负载调整。
- 自动合并 MP4 需要系统安装 ffmpeg，若不使用可设置 auto_merge: false

### 本项目基于[MIT license](https://github.com/ydtg1993/papa/LICENSE.txt)开源
