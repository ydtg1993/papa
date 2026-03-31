# Papa - 高性能分布式爬虫框架

<img src="https://img.shields.io/badge/Go-1.23+-00ADD8?style=flat&logo=go">
<img src="https://img.shields.io/badge/License-MIT-blue.svg">

#### Papa 是一个基于 Go 和 chromedp 的高性能分布式爬虫框架，支持多阶段爬取、浏览器池复用、代理轮换、断点续传和任务监控。适用于需要 JavaScript 渲染的现代 Web 应用爬取。

### ✨ 核心特性
> 🚀 多阶段爬取 – 将爬取任务拆分为多个阶段（如列表页、详情页），每个阶段独立配置工作池、重试策略
> 
> 🌐 浏览器池 – 基于 chromedp 的浏览器实例池，复用浏览器进程，减少资源消耗，支持最大空闲时间自动回收
>
> 🔄 代理轮换 – 集成代理管理器，支持从 API 动态获取代理列表并轮换使用
>
> 💾 任务持久化 – 基于 GORM 将任务状态持久化到 MySQL，支持断点续传，引擎重启后自动恢复未完成任务
>
> 📊 可观测性 – 工作池提供活动事件通道，监控模块可实时统计任务执行情况（成功/失败/耗时）
>
> ⚙️ 灵活配置 – 通过 YAML 配置文件设置各阶段 worker 数量、队列大小、重试次数、浏览器参数等

### 📐 架构设计
![整体架构图](https://github.com/ydtg1993/papa/architecture.png)

#### 模块说明
<!-- 模块描述表格 -->
<table>
    <thead>
        <tr>
            <th style="text-align:center;">模块</th>
            <th style="text-align:left;">描述</th>
        </tr>
    </thead>
    <tbody>
        <tr>
            <td style="text-align:center;"><strong>Engine</strong></td>
            <td style="text-align:left;">核心引擎，管理多个爬取阶段，负责任务注册、提交、恢复和生命周期控制</td>
        </tr>
        <tr>
            <td style="text-align:center;"><strong>Stage</strong></td>
            <td style="text-align:left;">每个阶段包含一个独立的工作池（WorkerPool）和对应的抓取处理器（Handler），通过 NextStage 配置支持自动流转</td>
        </tr>
        <tr>
            <td style="text-align:center;"><strong>WorkerPool</strong></td>
            <td style="text-align:left;">泛型工作池，消费任务队列，调用 Handler 执行具体抓取逻辑，并发布活动事件供监控</td>
        </tr>
        <tr>
            <td style="text-align:center;"><strong>Handler</strong></td>
            <td style="text-align:left;">业务实现接口，每个阶段需实现 FetchHandler 方法，负责页面抓取和链接解析</td>
        </tr>
        <tr>
            <td style="text-align:center;"><strong>Browser Pool</strong></td>
            <td style="text-align:left;">管理 chromedp 浏览器实例，支持代理注入、空闲回收，提供 Get/Put 方法</td>
        </tr>
        <tr>
            <td style="text-align:center;"><strong>Proxy Manager</strong></td>
            <td style="text-align:left;">从 API 获取代理列表，轮询返回，支持定时刷新</td>
        </tr>
        <tr>
            <td style="text-align:center;"><strong>Monitor</strong></td>
            <td style="text-align:left;">消费 WorkerPool 的活动事件，统计任务执行情况（按 worker 和全局）</td>
        </tr>
        <tr>
            <td style="text-align:center;"><strong>Database</strong></td>
            <td style="text-align:left;">通过 GORM 连接 MySQL，存储任务状态（crawler_tasks）和页面数据（pages）</td>
        </tr>
    </tbody>
</table>

#### 数据流
> 1. 任务提交：main.go 调用 engine.SubmitTask("catalog", task)，任务先写入数据库（状态 pending），然后提交到对应阶段的队列。
>
> 2. 任务处理：Worker 从队列获取任务，调用 Handler 的 FetchHandler。处理前将任务状态更新为 processing，成功后更新为 success，失败则重试（最多 MaxAttempts 次），最终状态为 failed。
>
> 3. 阶段流转：Handler 在解析页面后，可通过 engine.SubmitTask 将新任务提交到下一阶段（如 detail）。
>
> 4. 恢复机制：引擎启动时调用 RecoverTasks，加载所有 pending 或超时 processing 的任务，重置状态后重新提交。
>
> 5. 监控：WorkerPool 将任务开始/结束事件发送到 Activities 通道，Monitor 消费并更新统计。

📁 目录结构

    ├── cmd/
    │   └── crawler/          # 程序入口
    │       └── main.go
    ├── configs/              # 配置文件
    │   └── config.yaml
    ├── internal/             # 内部私有代码
    │   ├── config/           # 配置加载
    │   ├── crawler/          # 引擎核心
    │   ├── fetcher/          # 抓取阶段实现
    │   ├── loggers/          # 日志封装
    │   ├── middleware/proxy/ # 代理管理
    │   └── models/           # 数据模型
    ├── pkg/                  # 公共可复用包
    │   ├── browser/          # 浏览器池
    │   ├── database/         # 数据库连接
    │   ├── monitor/          # 任务监控
    │   └── workerpool/       # 泛型工作池
    ├── logs/                 # 日志文件目录（运行时生成）
    ├── scripts/              # 辅助脚本
    ├── storage/              # 存储目录（如文件下载）
    ├── go.mod
    └── go.sum

### 🚀 快速开始
#### 环境要求
>Go 1.18+
> 
>MySQL 5.7+
> 
>Chrome 浏览器（用于 chromedp）

#### 安装
    bash
    git clone https://github.com/ydtg1993/papa.git
    cd papa
    go mod download

#### 配置
>编辑 configs/config.yaml，修改数据库连接、浏览器参数、爬虫阶段等

#### 运行
>go run cmd/crawler/main.go

### 🔧 扩展开发  添加新抓取阶段
#### 1. 在 internal/fetcher 下创建新文件，实现 crawler.Handler 接口：

    type MyStage struct{}
    
    func (m *MyStage) GetStage() string {
    return "mystage"
    }
    
    func (m *MyStage) FetchHandler(ctx context.Context, task *crawler.Task, engine *crawler.Engine) error {
    // 使用浏览器池
    browser, err := engine.BrowserPool.Get(ctx)
    if err != nil {
    return err
    }
    defer engine.BrowserPool.Put(browser)
    
        // chromedp 操作...
        // 解析出下一阶段的 URL 后，可通过 engine.SubmitTask 提交
        return nil
    }

#### 2. 在 main.go 中注册该阶段：

    myStage := &fetcher.MyStage{}
    err := engine.RegisterStage(myStage, crawler.StageConfig{
    WorkerCount: 3,
    QueueSize:   30,
    NextStage:   "", // 可选
    })

### 📝 注意事项
请遵守目标网站的 robots.txt 和法律法规，合理设置爬取频率。

若使用代理，确保代理 API 返回的代理列表可用且稳定。

生产环境建议关闭 headless: false（显示浏览器窗口）以节省资源。

数据库连接池参数请根据实际负载调整。

![MIT license](https://github.com/ydtg1993/papa/LICENSE.txt)