# Papa - 高性能分布式爬虫框架

> 基于 Go + [Rod](https://github.com/go-rod/rod) 的浏览器自动化爬虫框架：浏览器池、多阶段工作池、任务持久化与恢复、M3U8/文件下载、定时任务、Web 监控。

## 快速开始

```bash
go install ./cmd/papa
papa new mycrawler --replace /path/to/papa   # 或外部 go run github.com/ydtg1993/papa/v2/cmd/papa@latest new mycrawler
cd mycrawler && go mod tidy && go run .
```

三步：改 `configs/config.yaml`（`app.env: dev` 自动建表）→ 写 fetcher（实现 `papa.Fetcher`）→ `main.go` 注册阶段 + 提交起始任务 + `Run`。

## 阅读路径

先按「你要做的事」定位手册，再进对应手册查字段 / 接口 / 示例：

| 我要… | 手册 | 手册在讲什么 |
| --- | --- | --- |
| 从零写抓取逻辑（HTML / Rod 策略、场景示例） | [FETCHER_WRITING_GUIDE.md](./FETCHER_WRITING_GUIDE.md) | Fetcher 契约、两种抓取策略、常见坑 |
| 把抓到的数据落库 | [DATA_LANDING_GUIDE.md](./DATA_LANDING_GUIDE.md) | 单表+JSON、三条落地路径、API 速查 |
| 查 / 改某个配置项 | [CORE_CONFIG.md](./CORE_CONFIG.md) | 全部配置项字典、时长格式、热更字段 |
| 注册定时任务 / 写 cron | [SCHEDULER.md](./SCHEDULER.md) | RegisterCronJob、6 段 cron、RepollRepeatableTasks |
| 失败任务要重跑 | [ERROR_QUEUE.md](./ERROR_QUEUE.md) | error_queue 配置、自动/手动触发、再处理上限 |
| 重启后捡回卡死任务 | [RECOVER_QUEUE.md](./RECOVER_QUEUE.md) | recover_queue、启动立即恢复、卡死判定 |
| 周期轮询 repeatable 任务 | [REPEAT_QUEUE.md](./REPEAT_QUEUE.md) | repeat_queue、自动重投已完成的轮询任务 |
| 用监控后台 / 调它的 API | [MONITOR.md](./MONITOR.md) | Dashboard、设置/数据浏览/动态配置/队列/日志 API |
| 排查「页面抓到了啥」/ 生成新项目 | [CLI.md](./CLI.md) | `papa new` 脚手架 + `html`/`rod`/`diff`/`select` 调试命令 |
| 构建 / 测试 / 竞态检测（Makefile） | [DEVELOPMENT.md](./DEVELOPMENT.md) | make 目标、Windows 怎么跑、race 前置条件 |

> 新手上手顺序：本页 → [FETCHER_WRITING_GUIDE.md](./FETCHER_WRITING_GUIDE.md) → [DATA_LANDING_GUIDE.md](./DATA_LANDING_GUIDE.md)，其余按需查。

## 注意事项

- 遵守目标网站 robots.txt 与法律法规，合理设置抓取频率。
- 生产环境建议 `headless: true`，按负载调 `pool_size` / 数据库连接池。
- 自动合并 MP4 需系统安装 ffmpeg。

## License

基于 [MIT License](https://github.com/ydtg1993/papa/LICENSE.txt) 开源。
