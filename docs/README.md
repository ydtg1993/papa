# Papa - 高性能爬虫框架

> 基于 Go 的爬虫框架，内置**浏览器自动化（Rod）**与**静态 HTML 抓取（htmlfetch）**双引擎：浏览器池、多阶段工作池、任务持久化与恢复、M3U8/文件下载、定时任务、Web 监控。

## 快速开始

```bash
go install github.com/ydtg1993/papa/v2/cmd/papa@latest
papa new mycrawler --replace /path/to/papa   # 或外部 go run github.com/ydtg1993/papa/v2/cmd/papa@latest new mycrawler
cd mycrawler && go mod tidy
```

1. 把 `configs/config.yaml` 的 `db.dsn` 指向你的 MySQL。
2. **`papa migrate` 建表** —— 在项目目录下跑（它自己读 `configs/config.yaml`）。检测到这是业务
   项目时，它会转交项目自己的迁移入口，框架表与 `models` 包里的业务模型一起建。
   之后每新开一个带表的开关（`server.operation_log` / `crawler.trace.enabled`）都要**再跑一次**，那两张表跟着开关走。
3. 写 fetcher（实现 `papa.Fetcher`），在 `main.go` 里注册阶段、提交起始任务、调 `Run`。
4. 加数据模型写根目录 `models/` 包（`models/models.go` 里登记建表清单）；后台（`/monitor`）
   要加表格页 / 自定义页 / 自己的接口，改 `monitor/` 包 ——
   `main.go` 只有 `monitor.Register(app)` 一行（分层与用法见 [MONITOR.md](./MONITOR.md) 第 5 节）。
5. `go run .`

启动时**不会**自动建表 —— 迁移是显式的一步，所以本地和生产跑的是同一条命令。
`AutoMigrate` 只增不减（加表 / 加列 / 加索引，不删列也不改类型），重复跑幂等，随便跑。

> **在项目目录里 `papa migrate` 与 `make migrate` 等价**：CLI 是独立编译的进程，看不到你在
> `main.go` / `models.Models()` 里注册的模型，所以它在业务项目里会把迁移**转交**给项目自己
> （`go run . -migrate` → `App.Migrate()`），两边都建。不在业务项目里跑时它只建框架自带的表。
> 脚手架刚生成的项目没有业务表（内容默认写进 `crawler_tasks.content` 这个 JSON 列，不是表），
> 所以起步阶段两条命令等价。判据与细则见 [CORE_CONFIG.md](./CORE_CONFIG.md) 第 2 节。

## 阅读路径

先按「你要做的事」定位手册，再进对应手册查字段 / 接口 / 示例：

| 我要… | 手册 | 手册在讲什么 |
| --- | --- | --- |
| 从零写抓取逻辑（HTML / Rod 策略、场景示例） | [FETCHER_WRITING_GUIDE.md](./FETCHER_WRITING_GUIDE.md) | Fetcher 契约、两种抓取策略、常见坑 |
| 把抓到的数据落库 | [DATA_LANDING_GUIDE.md](./DATA_LANDING_GUIDE.md) | 单表+JSON、三条落地路径、API 速查 |
| 查 / 改某个配置项 | [CORE_CONFIG.md](./CORE_CONFIG.md) | 全部配置项字典、时长格式、热更字段 |
| 注册定时任务 / 写 cron | [SCHEDULER.md](./SCHEDULER.md) | RegisterCronJob、6 段 cron、RepollRepeatableTasks |
| 失败任务要重跑 | [ERROR_QUEUE.md](./ERROR_QUEUE.md) | error_queue 配置、自动/手动触发、再处理上限 |
| 重启后捡回中断的任务 | [RECOVER_QUEUE.md](./RECOVER_QUEUE.md) | recover_queue、启动时把未到终态的任务全部重新入队 | 
| 周期轮询 repeatable 任务 | [REPEAT_QUEUE.md](./REPEAT_QUEUE.md) | repeat_queue、自动重投已完成的轮询任务 |
| 用监控后台 / 调它的 API / 挂自己的接口 | [MONITOR.md](./MONITOR.md) | Dashboard、设置/表格页/自定义页/自定义路由与中间件/动态配置/队列/日志 API |
| 排查「页面抓到了啥」/ 生成新项目 / 建令牌 | [CLI.md](./CLI.md) | `papa new` 脚手架、`html`/`rod`/`diff`/`select` 调试命令、`token add` |
| 构建 / 测试 / 竞态检测（Makefile） | [DEVELOPMENT.md](./DEVELOPMENT.md) | make 目标、Windows 怎么跑、race 前置条件 |

> 新手上手顺序：本页 → [FETCHER_WRITING_GUIDE.md](./FETCHER_WRITING_GUIDE.md) → [DATA_LANDING_GUIDE.md](./DATA_LANDING_GUIDE.md)，其余按需查。

## 注意事项

- 遵守目标网站 robots.txt 与法律法规，合理设置抓取频率。
- 生产环境建议 `headless: true`，按负载调 `pool_size`（浏览器并发上限，改需重启）/ 数据库连接池。
- 自动合并 MP4 需系统安装 ffmpeg。

## License

基于 [MIT License](https://github.com/ydtg1993/papa/LICENSE.txt) 开源。
