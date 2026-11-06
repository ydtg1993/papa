# Papa - 高性能爬虫框架

> 基于 Go 的爬虫框架，内置**浏览器自动化（Rod）**与**静态 HTML 抓取（htmlfetch）**双引擎：浏览器池、多阶段工作池、任务持久化与恢复、M3U8/文件下载、失败重投/启动恢复/周期轮询治理队列、Web 监控。框架不再内置通用 cron 调度器。
> **运行观测（obs）**：框架级结构化事件流（`logs/events.jsonl`）+ 人读报告 / 机读 JSON / AI 排查细则（`papa report`），见 [OBSERVABILITY.md](./OBSERVABILITY.md)。

## 快速开始

```bash
go install github.com/ydtg1993/papa/v3/cmd/papa@latest
papa new mycrawler --replace /path/to/papa   # 或外部 go run github.com/ydtg1993/papa/v3/cmd/papa@latest new mycrawler
cd mycrawler && go mod tidy
```

1. 把 `configs/config.yaml` 的 `db.dsn` 指向你的 MySQL（`configs/config.yaml.example` 是进版本库的范本，
   照它抄一份自己的 `config.yaml`）。
2. **`papa migrate` 建表** —— 在项目目录下跑（它自己读 `configs/config.yaml`）。检测到这是业务
   项目时，它会转交项目自己的迁移入口，框架表与 `models` 包里的业务模型一起建。
   之后每新开一个带表的开关（`server.operation_log` / `crawler.trace.enabled` / `rules.enabled`）都要**再跑一次**，对应表跟着开关走。
3. 写 fetcher（实现 `papa.Fetcher`）；**站点与阶段声明写在 `internal/sites/<站名>.go`**（并发/队列/间隔/重试/
   入口开关都在那儿一份写全，各文件在自己的 `init()` 里登记），`main.go` 里只有 `app.RegisterSites(papa.Sites()...)`
   一行 —— 加一个站只加文件。起始任务由 fetcher 自己实现 `SubmitEntries(engine, site)` 提交
   （见 [FETCHER_WRITING_GUIDE.md](./FETCHER_WRITING_GUIDE.md) 1.1）。
4. 加数据模型写`internal/models/` 包（`internal/models/models.go` 里登记建表清单）；后台（`/monitor`）
   要加表格页 / 自定义页 / 自己的接口，改 `internal/monitor/` 包 ——
   `main.go` 只有 `monitor.Register(app)` 一行（分层与用法见 [MONITOR.md](./MONITOR.md) 第 5 节）。
5. `go run .`（配置路径默认 `configs/config.yaml`，要用别处就 `go run . --config=./config.yaml`）。

启动时**不会**自动建表 —— 迁移是显式的一步，所以本地和生产跑的是同一条命令。
底层使用 GORM `AutoMigrate`，主要用于补表、列和索引，不负责删除列或表达全部破坏性变更。重复执行通常是幂等的，但生产环境仍应先备份、审查生成 SQL，并为改名、改类型等变更自己写迁移 SQL。

> **在项目目录里 `papa migrate` 与 `make migrate` 等价**：CLI 是独立编译的进程，看不到你在
> `main.go` / `models.Models()` 里注册的模型，所以它在业务项目里会把迁移**转交**给项目自己
> （`go run . -migrate` → `App.Migrate()`），两边都建。不在业务项目里跑时它只建框架自带的表。
> 脚手架刚生成的项目没有业务表（框架任务表不存采集结果，抓到的结果要在 `internal/models/` 里建业务表来落），
> 所以起步阶段两条命令等价。判据与细则见 [CORE_CONFIG.md](./CORE_CONFIG.md) 第 2 节。

新项目根入口只转交 `internal/app.Run`；应用导航在 `docs/README.md`，本套框架手册复制到 `docs/framework/`。旧项目的根级包与平铺文档继续受 `check` / `upgrade` 支持。

## 阅读路径

先按「你要做的事」定位手册，再进对应手册查字段 / 接口 / 示例：

| 我要… | 手册 | 手册在讲什么 |
| --- | --- | --- |
| 从零写抓取逻辑（HTML / Rod 策略、场景示例） | [FETCHER_WRITING_GUIDE.md](./FETCHER_WRITING_GUIDE.md) | Fetcher 契约、两种抓取策略、常见坑 |
| 网络录制 / 链接与订阅源 / 分页 / 增量 / 数据质量 | [CRAWL_TOOLKIT.md](./CRAWL_TOOLKIT.md) | 有界工具、调用方职责与测试边界 |
| 按账号登录、Token/CSRF、过期恢复、浏览器状态与加密保存 | [ACCOUNT_RUNTIME.md](./ACCOUNT_RUNTIME.md) | 统一账号入口、版本控制、暂停注销与凭据边界 |
| 编写 HTML/JSON 规则并用样本验收、激活与回滚 | [CRAWL_RULES.md](./CRAWL_RULES.md) | 声明式离线提取、校验与版本注册 |
| 持久保存规则/样本，发布整组阶段版本并回滚 | [RULE_STORE.md](./RULE_STORE.md) | MySQL 元数据＋文件样本、显式验收、发布竞争、Catalog 恢复与影子评估 |
| 把规则接成列表、详情与 URL/POST 游标分页采集 | [RULE_FETCHER.md](./RULE_FETCHER.md) | 通用执行器、版本固定、账号接线、幂等保存和完整示例 |
| 把抓到的数据落库 | [DATA_LANDING_GUIDE.md](./DATA_LANDING_GUIDE.md) | 单表+JSON、三条落地路径、API 速查 |
| 查 / 改某个配置项 | [CORE_CONFIG.md](./CORE_CONFIG.md) | 全部配置项字典、时长格式、站点/阶段声明，以及全局三个治理队列 |
| 失败任务要重跑 | [ERROR_QUEUE.md](./ERROR_QUEUE.md) | error_queue 配置、自动/手动触发、再处理上限 |
| 重启后捡回中断的任务 | [RECOVER_QUEUE.md](./RECOVER_QUEUE.md) | recover_queue、启动时重投本进程启动前遗留的未终态任务 |
| 周期轮询 repeatable 任务 | [REPEAT_QUEUE.md](./REPEAT_QUEUE.md) | repeat_queue、到点才重投（周期可逐条定、后台可改） |
| **加一个站 / 管多个站点**（站点维度都有什么） | [FETCHER_WRITING_GUIDE.md](./FETCHER_WRITING_GUIDE.md) §7、[DEVELOPMENT.md](./DEVELOPMENT.md) §7 | 一站一个文件（自登记）、站点键必填、阶段名按站点作用域（两个站可以同名）、站点级请求头/熔断/下载器；三个治理队列有意留在全局 |
| 用监控后台 / 调它的 API / 挂自己的接口 | [MONITOR.md](./MONITOR.md) | Dashboard、设置/表格页/自定义页/自定义路由与中间件/站点 Tab 与队列治理/暂停铃铛/obs 报告/日志 API |
| **查一轮跑得怎么样 / 出运行报告 / 给报告加业务段** | [OBSERVABILITY.md](./OBSERVABILITY.md) | 事件契约、`obs` 配置、`papa report` 4 产物、内置模式库、业务扩展点、局限与注意 |
| 排查「页面抓到了啥」/ 生成新项目 / 升级已有项目 / 建令牌 | [CLI.md](./CLI.md) | `papa new` 脚手架、`html`/`rod`/`diff`/`select` 调试命令、`upgrade`（刷新框架副本+检查）、`check`（不用起库验声明）、`token add` |
| 构建 / 测试 / 竞态检测（Makefile） | [DEVELOPMENT.md](./DEVELOPMENT.md) | make 目标、Windows 怎么跑、race 前置条件 |

> 新手上手顺序：本页 → [FETCHER_WRITING_GUIDE.md](./FETCHER_WRITING_GUIDE.md) → [DATA_LANDING_GUIDE.md](./DATA_LANDING_GUIDE.md)，其余按需查。

## 注意事项

- 遵守目标网站 robots.txt 与法律法规，合理设置抓取频率。
- 生产环境建议 `headless: true`，按负载调 `pool_size`（浏览器**全局**并发上限，`browser.enable: true` 时必填且 > 0；改需重启）/ 数据库连接池。
- 自动合并 MP4 需系统安装 ffmpeg。

## License

基于 [MIT License](https://github.com/ydtg1993/papa/LICENSE.txt) 开源。
