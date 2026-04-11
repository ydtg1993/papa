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
- 🔧 **可扩展** – 清晰的接口设计（`Handler`、`Tasker`），方便自定义爬取逻辑和下载器。
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
| **Stage** | 每个阶段包含一个独立的工作池（WorkerPool）和对应的抓取处理器（Handler），用户需在 FetchHandler 中调用 SubmitTask 实现阶段跳转。 |
| **WorkerPool** | 泛型工作池，消费任务队列，调用 Handler 执行具体抓取逻辑，并发布活动事件供监控。 |
| **Handler** | 业务实现接口，每个阶段需实现 FetchHandler 方法，负责页面抓取和链接解析。 |
| **Browser Pool** | 管理 Rod 浏览器实例，支持代理注入、空闲回收，提供 Get/Put 方法。 |
| **Monitor** | 消费 WorkerPool 的活动事件，统计任务执行情况（按 worker 和全局），并通过 HTTP 服务展示。 |
| **Scheduler** | 基于 Cron 的定时任务调度器，支持周期性提交 catalog 任务或执行恢复任务。 |
| **Database** | 通过 GORM 连接 MySQL，存储任务状态（crawler_tasks）和页面数据。 |
| **Proxy Manager** | 从 API 获取代理列表，轮询返回，支持定时刷新。 |
| **M3U8 downloader** | 下载 M3U8 视频流，支持切片合并、解密、断点续传。 |
| **File Downloader** | 下载普通文件（图片、音频、MP4 等），支持分片并发和断点续传。 |

#### 数据流
1. **任务提交**：入口（`main.go`）调用 `engine.SubmitTask`，任务先写入数据库（状态 `pending`），然后提交到对应阶段的队列。
2. **任务处理**：Worker 从队列获取任务，调用 Handler 的 `FetchHandler`。处理前将任务状态更新为 `processing`，成功后更新为 `success`，失败则重试（最多 `MaxAttempts` 次），最终状态为 `failed`。
3. **阶段流转**：Handler 在解析页面后，可通过 `engine.SubmitTask` 将新任务提交到下一阶段（如 `detail`）。
4. **恢复机制**：引擎启动时调用 `RecoverTasks`，加载所有 `pending` 或超时 `processing` 的任务，重置状态后重新提交。
5. **监控**：WorkerPool 将任务开始/结束事件发送到 `Activities` 通道，Monitor 消费并更新统计。
6. **定时任务**：Scheduler 根据配置的 Cron 表达式，定时执行 `catalog` 提交或 `recover` 恢复，实现自动化维护。

📁 目录结构

      ├── cmd/
      │   └── crawler/            # 程序入口
      │       └── main.go
      ├── configs/
      │   └── config.yaml         # 配置文件
      ├── internal/               # 内部私有代码
      │   ├── app/                # 应用组装（依赖注入、启动）
      │   ├── config/             # 配置加载（viper）
      │   ├── crawler/            # 引擎核心（Engine, Task）
      │   ├── fetcher/            # 抓取阶段实现（catalog, detail）
      │   ├── models/             # 数据模型（CrawlerTask）
      │   ├── scheduler/          # 定时任务调度器（cron jobs）
      │   └── server/             # Web 监控服务（HTML + JSON API）
      ├── pkg/                    # 公共可复用包
      │   ├── browser/            # 浏览器池（基于 rod）
      │   ├── database/           # 数据库连接（GORM）
      │   ├── loggers/            # 日志封装（lumberjack + logrus）
      │   ├── middleware/         # 下载中间件
      │   │   ├── filedown/       # 文件下载器
      │   │   ├── m3u8/           # M3U8 视频下载器
      │   │   └── proxy/          # 代理管理器
      │   ├── queue.go            # 通用消息队列（错误/活动）
      │   ├── track/              # 监控统计（StatsQueue）
      │   └── workerpool/         # 泛型工作池
      ├── logs/                   # 日志文件目录（运行时生成）
      ├── downloads/              # 默认下载目录
      ├── scripts/                # 辅助脚本（Docker、数据库迁移等）
      ├── storage/                # 其他存储（架构图等）
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
yaml

# configs/config.yaml
app:
  env: dev   # loc/dev/prod，dev 时自动迁移表结构
```

#### 运行
```
go run cmd/crawler/main.go
```

### 🔧 扩展开发  
#### 添加新抓取阶段
1. 在 main.go 中注册阶段配置代理,下载器：
```
    **使用下载器,代理,等中间件 用于fetcher中资源落地**
    appInstance.Engine.SetProxy(proxy.NewManager(appInstance.Config.Proxy.APIURL, 8*time.Minute))
    appInstance.Engine.SetFiledown(filedown.NewDownloader(filedown.DefaultConfig()))  //可按照filedown.DefaultConfig config自定义修改
    appInstance.Engine.SetM3U8(m3u8.NewDownloader(m3u8.DefaultConfig()))              //可按照m3u8.DefaultConfig config自定义修改


    //=========================注册业务逻辑所需要的爬虫流程阶段=========================//
	// 参数： 传入对应的爬虫业务层fetcher, 回调方法用于初始任务手动提交
	// 阶段一: 抓取分类目录页(需要做一次手动提交将目录页url传入任务) 采集:url 标题 分类信息
	// 注:fetcher.FetchFirst{}中GetStage()返回字串需要与配置保持一直 配置:crawler.stages.first
    appInstance.RegisterStage(&fetcher.FetchFirstStage{},
	func(engine *crawler.Engine) {
		// 手动提交起始任务 任务需标注下一个阶段为流水作业需要 一般用于初期目录阶段后续任务分发均由fetcher步骤里具体实现
		if err := engine.SubmitTask(&crawler.Task{
			PID:        0, //标明初级任务
			URL:        "目录主页url用于爬取单任务所需内容",
			Stage:      "FirstStage", //配置中提前设定好的stage标识 对应任务stage将交由对应阶段fetcher处理
			Repeatable: true, //可重复抓取，为后期轮询标识
		}); err != nil {
			appInstance.Logger.Engine.Errorf("submit initial task: %s", err.Error())
		}
	})
    // 阶段二: 抓取详情目录页内容
	// 注:fetcher.FetchSecond{} GetStage()返回字串 second 同配置 crawler.stages.second
    app.RegisterStage(&fetcher.FetchSecond{}, nil)
```

2. 在 internal/fetcher 下创建新文件，实现 crawler.Handler 接口：
```
    type FirstStage struct{}
    
    func (m *FirstStage) GetStage() string {
        return "FirstStage"  //需要与配置stage相同
    }
    
    func (m *FirstStage) FetchHandler(ctx context.Context, task *crawler.Task, engine *crawler.Engine) error {
        // 获取浏览器实例
        browser, err := engine.GetBrowserPool().Get(ctx)
        if err != nil {
            return err
        }
        defer engine.GetBrowserPool().Put(browser)
        
        // 使用 Rod 操作页面...
        // 解析出下一阶段的 URL 后，可通过 engine.SubmitTask 提交新任务

		// 在 fetcher 中使用
		result := engine.GetM3U8().Download(ctx, m3u8URL, "downloads", "myvideo", nil)
		if result.Error != nil {
		    return result.Error
		}

		//分发提交子任务到下个阶段
        engine.SubmitTask(&crawler.Task{
        PID:   task.ID,
        URL:   videoURL,
        Stage: "second",  //交由已注册好的second阶段处理
    })
        return nil
    }
```

#### fetcher中使用 M3U8 下载器 示例
```
	    cfg := m3u8.DefaultConfig()
		cfg.EnableResume = true
		cfg.AutoMerge = true
		downloader := m3u8.NewDownloader(cfg)
		result := downloader.Download(context.Background(), 
		    "https://example.com/video.m3u8", 
		    "downloads",    // 输出子目录
		    "myvideo",      // 输出文件名（不含扩展名，会自动加 .mp4）
		    nil)
```

#### fetcher中使用下载器 示例
```
    // 下载封面图，输出到 "covers" 子目录，自动生成文件名
	res := engine.GetFiledown().Download(ctx, coverURL, "covers", "")
	if res.Error != nil {
	    return res.Error
	}
```

### 📝 注意事项
- 请遵守目标网站的 robots.txt 和法律法规，合理设置爬取频率。
- 若使用代理，确保代理 API 返回的代理列表可用且稳定。
- 生产环境建议开启 headless: true 以节省资源，并根据需要调整 pool_size。
- 数据库连接池参数请根据实际负载调整。
- 自动合并 MP4 需要系统安装 ffmpeg，若不使用可设置 auto_merge: false

### 本项目基于[MIT license](https://github.com/ydtg1993/papa/LICENSE.txt)开源
