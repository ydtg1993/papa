# Papa - 高性能分布式爬虫框架

>Papa 是一个基于 Go 语言和 [Rod](https://github.com/go-rod/rod) 的高性能、可扩展的浏览器自动化爬虫框架。
>它内置了浏览器池、多阶段工作池、任务持久化与恢复、M3U8 视频下载（支持断点续传、自动合并）、定时任务调度、Web 监控
>等特性，适用于需要处理 JavaScript 渲染、反爬严格的网站以及流媒体资源的抓取与下载。

### 📐 架构设计
![整体架构图](https://github.com/ydtg1993/papa/blob/master/storage/architecture.png)

### ✨ 核心特性
- 🚀 **多阶段爬取** – 支持 `catalog` → `detail` 等多阶段流水线，每个阶段可独立配置并发数和队列大小。
- 🌐 **浏览器池** – 基于 Rod 封装浏览器池，支持无头/有头模式，自动管理浏览器生命周期。
- 💾 **任务持久化与恢复** – 基于 GORM 将任务状态持久化到 MySQL，支持断点续爬，引擎启动时自动恢复未完成或超时的任务。
- 📊 **可观测性** – 工作池提供活动事件通道，监控模块可实时统计各阶段任务执行情况（成功/失败/耗时），并提供 Web 界面与 JSON API。
- ⚙️ **灵活配置** – 通过 YAML 配置文件设置各阶段 worker 数量、队列大小、重试次数、浏览器参数等。
- ⏰ **定时任务调度** – 基于 Cron 表达式，支持周期执行 `catalog` 轮询（如每日检查新视频）、`recover` 恢复未完成任务等。
- 🔧 **可扩展** – 清晰的接口设计（`Handler`、`Tasker`），方便自定义爬取逻辑和下载器。
- 📦 **M3U8 下载器** – 高性能 M3U8 视频下载模块，支持：
  - 多线程并发下载
  - 断点续传（任务级 + 片段级）
  - AES-128 解密（自动处理 PKCS#7 填充）
  - 自动合并为 MP4（需 ffmpeg）
  - 多码率自适应（自动选择最高码率）
- 🔄 **代理轮换** – 集成代理管理器，支持从 API 动态获取代理列表并轮换使用。

#### 模块说明
| 模块 | 描述 |
| :--- | :--- |
| **Engine** | 核心引擎，管理多个爬取阶段，负责任务注册、提交、恢复和生命周期控制。 |
| **Stage** | 每个阶段包含一个独立的工作池（WorkerPool）和对应的抓取处理器（Handler），通过 NextStage 配置支持自动流转。 |
| **WorkerPool** | 泛型工作池，消费任务队列，调用 Handler 执行具体抓取逻辑，并发布活动事件供监控。 |
| **Handler** | 业务实现接口，每个阶段需实现 FetchHandler 方法，负责页面抓取和链接解析。 |
| **Browser Pool** | 管理 Rod 浏览器实例，支持代理注入、空闲回收，提供 Get/Put 方法。 |
| **Monitor** | 消费 WorkerPool 的活动事件，统计任务执行情况（按 worker 和全局），并通过 HTTP 服务展示。 |
| **Scheduler** | 基于 Cron 的定时任务调度器，支持周期性提交 catalog 任务或执行恢复任务。 |
| **Database** | 通过 GORM 连接 MySQL，存储任务状态（crawler_tasks）和页面数据。 |
| **Proxy Manager** | 从 API 获取代理列表，轮询返回，支持定时刷新。 |
| **M3U8 downloader** | 提供m3u8链接下载视频切片合并等。 |
| **file downloader** | 下载各类网络资源 图片，mp4，音频等 |

#### 数据流
1. **任务提交**：入口（`main.go`）调用 `engine.SubmitTask`，任务先写入数据库（状态 `pending`），然后提交到对应阶段的队列。
2. **任务处理**：Worker 从队列获取任务，调用 Handler 的 `FetchHandler`。处理前将任务状态更新为 `processing`，成功后更新为 `success`，失败则重试（最多 `MaxAttempts` 次），最终状态为 `failed`。
3. **阶段流转**：Handler 在解析页面后，可通过 `engine.SubmitTask` 将新任务提交到下一阶段（如 `detail`）。
4. **恢复机制**：引擎启动时调用 `RecoverTasks`，加载所有 `pending` 或超时 `processing` 的任务，重置状态后重新提交。
5. **监控**：WorkerPool 将任务开始/结束事件发送到 `Activities` 通道，Monitor 消费并更新统计。
6. **定时任务**：Scheduler 根据配置的 Cron 表达式，定时执行 `catalog` 提交或 `recover` 恢复，实现自动化维护。

📁 目录结构

    ├── cmd/
    │ └── crawler/ # 程序入口
    │ └── main.go
    ├── configs/ # 配置文件
    │ └── config.yaml
    ├── internal/ # 内部私有代码
    │ ├── app/ # 应用组装
    │ ├── config/ # 配置加载
    │ ├── crawler/ # 引擎核心
    │ ├── fetcher/ # 抓取阶段实现
    │ ├── models/ # 数据模型
    │ ├── scheduler/ # 定时任务调度器
    │ └── server/ # Web 监控服务
    ├── pkg/ # 公共可复用包
    │ ├── browser/ # 浏览器池
    │ ├── database/ # 数据库连接
    │ ├── middleware/ # 下载中间件（filedown, m3u8, proxy）
    │ ├── loggers/ # 日志封装（sys, engine, scheduler, db, browser...）
    │ ├── monitor/ # 任务监控
    │ └── workerpool/ # 泛型工作池
    ├── logs/ # 日志文件目录（运行时生成）
    ├── downloads/ # 默认下载目录
    ├── scripts/ # 辅助脚本
    ├── storage/ # 其他存储
    ├── go.mod
    └── go.sum

### 🚀 快速开始
#### 环境要求
- Go 1.21+
- MySQL 5.7+ 或 8.0
- Chrome/Chromium 浏览器（用于 Rod，可自动下载或指定路径）
- （可选）ffmpeg（用于自动合并 MP4）
  
#### 安装
```bash
git clone https://github.com/ydtg1993/papa.git
cd papa
go mod download
```

#### 配置
>编辑 configs/config.yaml，修改数据库连接、浏览器路径、爬虫阶段参数、定时任务等。文件中已包含详细中文注释。

#### 初始化数据库
首次运行前设置环境变量dev以自动迁移表结构
```
configs/config.yaml
# 应用基础配置
app:
  env: dev 
```

#### 运行
```
go run cmd/crawler/main.go
```

### 🔧 扩展开发  
#### 添加新抓取阶段
1. 在 internal/fetcher 下创建新文件，实现 crawler.Handler 接口：
```
    type MyStage struct{}
    
    func (m *MyStage) GetStage() string {
        return "mystage"
    }
    
    func (m *MyStage) FetchHandler(ctx context.Context, task *crawler.Task, engine *crawler.Engine) error {
        // 获取浏览器实例
        browser, err := engine.GetBrowserPool().Get(ctx)
        if err != nil {
            return err
        }
        defer engine.GetBrowserPool().Put(browser)
        
        // 使用 Rod 操作页面...
        // 解析出下一阶段的 URL 后，可通过 engine.SubmitTask 提交新任务
        return nil
    }
```

2. 在 internal/app/app.go 的 registerStages 中注册该阶段：
```
    myStage := &fetcher.MyStage{}
    if err := engine.RegisterStage(myStage, crawler.StageConfig{
        WorkerCount: 3,
        QueueSize:   30,
        NextStage:   "detail",
        Handler:     myStage.FetchHandler,
    }); err != nil {
        return err
    }
```
#### 使用 M3U8 下载器
```
    cfg := m3u8.DefaultConfig()
    cfg.EnableResume = true    // 启用断点续传
    cfg.AutoMerge = true       // 自动合并为 MP4
    cfg.MaxConcurrent = 8      // 并发下载数
    downloader := m3u8.NewDownloader(cfg)
    result := downloader.Download(context.Background(), "https://example.com/video.m3u8", "myvideo")
    if result.Error == nil {
        fmt.Println("Saved to", result.OutputFile)
    }
```

### 📝 注意事项
- 请遵守目标网站的 robots.txt 和法律法规，合理设置爬取频率。
- 若使用代理，确保代理 API 返回的代理列表可用且稳定。
- 生产环境建议开启 headless: true 以节省资源，并根据需要调整 pool_size。
- 数据库连接池参数请根据实际负载调整。
- 自动合并 MP4 需要系统安装 ffmpeg，若不使用可设置 auto_merge: false

### 本项目基于[MIT license](https://github.com/ydtg1993/papa/LICENSE.txt)开源
