# Papa 爬虫核心配置手册

> 面向：配置 `configs/config.yaml` 时，每个字段是什么意思、怎么填。**全部改完要重启**（没有热更）。
> 配套：本文是「配置字典」；各模块的行为语义见 [SCHEDULER.md](./SCHEDULER.md)、[ERROR_QUEUE.md](./ERROR_QUEUE.md)、[RECOVER_QUEUE.md](./RECOVER_QUEUE.md)、[MONITOR.md](./MONITOR.md)。

---

## 0. 配置文件与加载

- 主配置：`configs/config.yaml`（`papa new` 生成，含中文注释）。
- 加载路径回退链：`papa.WithConfigPath(...)` 指定的路径 → 环境变量 `PAPA_CONFIG` → `configs/config.yaml`。
- **没有运行期热更**：改配置 = 改 `config.yaml` / 站点声明 + 重启。队列（三段）与 `crawler` 的那四项
  还能按站点在声明里覆盖（见各节与 [REPEAT_QUEUE.md](./REPEAT_QUEUE.md) 第 5 节）。
  老项目里的 `configs/runtime.yaml` 已经没人读写了，可以直接删。

## 1. 完整配置项参考

> **配置校验层**：启动时（`config.Load`）校验两件事，**不符合直接 panic** —— 不返回错误、也不静默降级。
>
> | 校验 | 行为 |
> | --- | --- |
> | **键名**：文件里有、结构体里没有的键 | **一律拒绝启动**，并尝试给出「是不是想写 X」。**没有降级开关** —— 静默忽略未知键是最贵的一种配置错误（它伪装成「配置生效了」） |
> | **值域**：已声明字段的合法范围 | 越界即 panic，报错点名键路径与为什么 |
>
> 判据表在 `config/validate.go` 的 `rules`，一条一行；`TestRuleKeysExistInStruct` 钉住每个键都真实存在于结构体（防止表里写一个永远不会触发的假键）。**这一层只管 `config.yaml`** —— 各库自己收到的 Go 结构体零值（`filedown.ChunkSize`、`htmlfetch.MaxBodySize` 那类）不在这里，判据归各自的构造函数。
>
> 每条规则都区分三态：**没写**（用默认值，合法）／写了且合法／写了但越界。唯一的例外是 `log.dir` —— 它没有「没写也合法」那一态（空串会去写文件系统根），见下。
>
> 唯一的"不校验"的键是 **`business`**（见下）：框架不认它的语义，写什么键都不管 —— 那是留给业务自己的配置位，不是给框架配置的。

### app —— 环境

| 键 | 类型 | 说明 |
| --- | --- | --- |
| `env` | string | `dev` / `prod`。只影响 SQL 日志（dev 打 info 且带参数值；见 `db.log_level`），不再影响建表 |

### crawler —— 爬虫核心

> **阶段参数不在这个文件里**：`worker_count` / `queue_size` / `delay` / `retry` / 入口开关 /
> 站点归属都在 `configs/sites/<站名>.go` 里一份写全（各文件在 init 里自登记，`App.RegisterSites(papa.Sites()...)`）——
> 一个阶段的存在本来就离不开代码（必须有个 fetcher），参数再放另一份文件等于加一个阶段要改两处。
> 配置里留着的 `crawler.stages` 段会被"未知配置键"拦下（**故意的**，见下面的校验层说明）。

| 键 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `dedup_cache_size` | int | `0` | 内存去重表最大条目数；`0`=不限，`>0` 用 LRU 限界，淘汰条目由 DB 唯一索引兜底 |
| `queue_watermark` | float | `0.75` | 队列高水位比例（0-1），达到后新任务溢出到 DB 待回灌，避免满队列丢任务 |
| `drain_interval` | duration | `2s` | 溢出任务回灌队列的间隔 |
| `stop_timeout` | duration | `5s` | 优雅退出时等各阶段 worker 把队列跑完的上限（各阶段**并发**等，总等待约一个该值）。超时则打错误日志，并**跳过**关库与关浏览器池 —— 此时 worker goroutine 还活着，关了只会让在途写入全部失败。与 `server.shutdown_timeout` 不是一回事：那个等的是在途 HTTP 请求（如日志打包下载） |
| `trace.enabled` | bool | `false` | 单任务步骤追踪：handler 用 `task.Trace.Step/Fail/Warn` 上报（`Warn` 是非致命档：任务成功但要留痕，如封面没下下来），写入 `crawler_task_trace` 表。引擎自己也会补两条：「任务失败」（失败尝试的**原因**，带分类与消息）与「归档页面」（带文件名，抽屉里可直接下载）。关闭时 `task.Trace` 为 `nil`，调用是安全 no-op |
| `trace.retention` | duration | `168h` | 步骤记录保留期（后台按批清理）；填**负数**表示永久保留、不自动清理 |
| `breaker.enabled` | bool | `false` | 熔断闸门总开关。开启后窗口内**终态失败**数达阈值就把**所有阶段**的 worker 一起闸住 |
| `breaker.window` | duration | `5m` | 统计窗口。窗口是滑动切片（60 个等宽桶），粒度 = `window/60` |
| `breaker.threshold` | int | — | 窗口内终态失败数达到它即暂停。**`enabled: true` 时必填且必须 > 0** —— 没写、写 `0` 或负数都会在**启动时 panic**（判据在 `breaker.New`，调用点是 `engine.NewEngine`）。它没有默认值：`threshold <= 0` 时「多少条才算熔断」没有答案，补一个数只会让人以为开着、数的却是另一回事 |

> **熔断按站点分组**：这一节配的是**默认值**（默认 scope = 未归属站点的阶段）；每个站点可以在
> `configs/sites/<站名>.go` 里用 `Breaker: &papa.BreakerSpec{...}` 覆盖（含 `Enabled: false`
> 单独关掉某一站）。多站时站点 A 被墙不会把站点 B 一起闸住。

### browser —— 浏览器池（Rod）

| 键 | 类型 | 说明 |
| --- | --- | --- |
| `enable` | bool | 是否启用浏览器池 |
| `pool_size` | int | 代理浏览器**并发上限**（按需创建，非常驻实例数） |
| `direct_pool_size` | int | 强制直连浏览器并发上限（不走代理） |
| `max_idle_time` | duration | 空闲回收阈值，`0`=不回收 |
| `headless` | bool | 无头模式 |
| `no_sandbox` | bool | 关闭 Chromium sandbox |
| `leakless` | bool | leakless 进程守护（Windows 上其 exe 易被杀软误报，谨慎开启） |
| `browser_path` | string | Chrome 可执行文件路径，空则用默认 Chromium |
| `headers` | map | **全局**默认请求头（与内置默认头合并，同名覆盖）。**通常不写这里**：与站点相关的头写在 `configs/sites/<站名>.go` 的 `Headers`（站点级）—— 它可被该层与**逐请求**（`papa.WithHeaders(ctx, …)`）覆盖，站点级写空值表示删掉那个头；这一层留给"与站点无关、所有站共用"的头 |

### html —— 静态 HTML 客户端

| 键 | 类型 | 说明 |
| --- | --- | --- |
| `enable` | bool | 是否启用静态 HTML 客户端 |
| `timeout` | duration | 请求超时 |
| `max_body_size` | int64 | 响应体大小上限（字节）。**`enable: true` 时必填**，`102400..67108864`（100KB..64MB）。下界挡的是**单位混淆** —— 隔壁 `log.max_size` 的单位是 MB，这里写 `10` 意思是 10 字节，于是每次抓取都报「页面太大」；上界 64MB 在模板值（10MB）之上，只防笔误。启动时校验，越界直接报错 |
| `headers` | map | 额外请求头，同上：**通常写在站点级 `Headers`**（抓取与下载都认），可被 `papa.WithHeaders` 覆盖（站点级写空值 = 删）。留空时静态抓取的 UA 是框架默认的 `PapaStaticHTML/1.0` |

### proxy —— 代理管理器

| 键 | 类型 | 说明 |
| --- | --- | --- |
| `api_url` | string | 代理服务 API 地址 |
| `refresh_interval` | duration | 代理列表刷新间隔 |

> 这一节配的是**代理池**（`api_url` 拉列表 + 定时刷新）。**用不用、用哪一个**由 fetcher 决定：
> `engine.NextProxy()` 取一个，`papa.WithProxyURL(ctx, addr)` 传给一次静态抓取，
> `filedown.DownloadOptions{Proxy: addr}` / `m3u8.DownloadOptions{Proxy: addr}` 传给一次下载。
> 若这次下载就该跟着任务的抓取出口走，用 `filedown.OptionsFromRequest(ctx, referer)` /
> `m3u8.OptionsFromRequest(ctx, referer)` 一步拿到（它会把 ctx 上的显式代理与站点级/逐请求头一起填好）。
> 浏览器渲染那条**不支持**逐请求地址（Chrome 限制），只有"走池 / 直连"两档。

### db —— 数据库

| 键 | 类型 | 说明 |
| --- | --- | --- |
| `driver` | string | **只实现了 `mysql`**；其它值启动即报 `unsupported driver` |
| `dsn` | string | 数据源名称 |
| `max_idle_conns` | int | 空闲连接数上限。**必填**，`1..100`；且**不得大于 `max_open_conns`**（Go 会把超出的静默压到 `max_open_conns`，你写的那个数不生效） |
| `max_open_conns` | int | 最大连接数上限。**必填**，`1..100`。**不写或写 0 会被拒**：`database/sql` 把 0 当成「不限」，而配置的零值也是 0 —— 两者是同一个数、分不开，不拦的话漏配就是静默打满 MySQL。上界 100 是政策值（模板给的就是 10 / 100），要开更多改 `config/validate.go` 的 `dbPoolMax` |
| `conn_max_lifetime` / `conn_max_idle_time` | duration | 连接最大生命周期 / 空闲最大存活 |
| `log_level` | string | SQL 日志级别：`silent`/`error`/`warn`/`info`；**留空按 `app.env` 推**（`dev`=info，其它=warn）。非 dev 还会隐掉日志里的参数值（渲染成 `?`）—— 每条 SQL 连着参数值进日志会随「日志导出」外泄；调试要看值就把 `env` 设成 `dev` |

### log —— 日志

| 键 | 类型 | 说明 |
| --- | --- | --- |
| `dir` | string | 日志目录。**必填** —— 空串会拼出**文件系统根 / 当前盘根**下的 `sys.log`：要么因为没权限而静默一条日志都没有，要么把日志散在盘根 |
| `max_size` | int | 单文件大小上限（MB） |
| `max_days` / `max_backups` | int | 保留天数 / 备份数 |
| `compress` / `local_time` | bool | 是否压缩 / 本地时间 |

### server —— 监控 HTTP 服务（详见 [MONITOR.md](./MONITOR.md)）

| 键 | 类型 | 说明 |
| --- | --- | --- |
| `enabled` | bool | 是否开启统一 HTTP 服务。开则挂载整个监控后台（`/monitor` + `/api/*` + 表格页 + 自定义页 + `UseRouter` 注册的路由）；关则**服务完全不监听**，上面这些一律不挂载。原来还有一个 `monitor` 子开关，但它唯一的效果是「起了 HTTP 服务却什么都不挂」（所有路径 404），已移除 |
| `port` | int | 监听端口 |
| `whitelist` / `whitelist_file` | []string | 来源 IP/CIDR 白名单；文件优先 |
| `monitor_dirs` | map | 监控页展示的业务目录占用 `name: path` |
| `queue_sample_interval` | duration | 三个治理队列「待处理」积压数的采样间隔，默认 `1m`。监控页刷新只读内存快照，仅采样时查库；调大可降低 DB 压力 |
| （无密钥字段） | — | 后台凭据是 `crawler_access_token` 表里的多条**访问令牌**（每条属于一个操作人）；用 `papa token add --operator <名字>` 创建 |
| `operation_log` | bool | 操作日志开关，默认 `false`。开启后后台所有增删改操作写入 `crawler_operation_log` 表（含失败，并记下**操作人**——来自访问令牌），侧边栏 General 分组多出一项「操作日志」（在「访问令牌」上方，只读表格页）；关闭时不建表、不写库，菜单项也不出现 |
| `read_header_timeout` | duration | `10s` | 只发请求头不发送体的慢连接会被掐掉 |
| `read_timeout` | duration | `30s` | 读完整请求（含 body）的上限 |
| `write_timeout` | duration | `0`（不限） | **默认不限**：日志打包下载可能传很久，设上限等于掐断在途下载；要限制再显式配 |
| `idle_timeout` | duration | `60s` | keep-alive 空闲连接的上限 |
| `shutdown_timeout` | duration | `10s` | 优雅退出时等在途请求（如日志下载）跑完的上限，超时才强制断开 |

### scheduler —— 定时任务（详见 [SCHEDULER.md](./SCHEDULER.md)）

| 键 | 类型 | 说明 |
| --- | --- | --- |
| `timezone` | string | cron 时区（默认 `Asia/Shanghai`） |

### error_queue —— 失败任务错误队列（详见 [ERROR_QUEUE.md](./ERROR_QUEUE.md)）

**不在 config.yaml 里**：三个治理队列都按站点写在 `configs/sites/<站名>.go` 的 `SiteSpec` 上
（yaml 里再写 `error_queue:` 这类段会以「未知配置键」拒绝启动，并指路到这里）。

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `Enabled` | `*bool` | **必写**（`nil` = 漏写 → 启动 panic）；`&false` = 本站不跑这条队列，其余字段可以不写 |
| `WorkerCount` | int | 并发重新投递数；开着时必填且 `> 0` |
| `Interval` | string | 自动轮询间隔（如 `"4h"`）；`"0"`=不自动，仅后台手动触发 |
| `MaxRetry` | int | 单个任务最多再处理代数；`0`=不限 |
| `BatchSize` | int | 每批查询处理的任务数；开着时必填且 `> 0`（分页流式） |

```go
	// configs/sites/<站名>.go（脚手架生成物里就是这几行）
	ErrorQueue: &papa.ErrorQueueSpec{
		Enabled:     &on,   // on := true —— Go 没有字面量取址
		WorkerCount: 1,
		Interval:    "4h",
		MaxRetry:    3,
		BatchSize:   100,
	},
	// 这一站不跑错误队列：只写一行
	// ErrorQueue: &papa.ErrorQueueSpec{Enabled: &off},
```

> 错误队列**按站点跑**（后台一行一个站）：命名站点是 `error_queue:<站点>`；默认 scope 那条仍叫
> `error_queue`，但它**不跑** —— 未归属（`site` 为空）的任务没有声明可挂（见 [DEVELOPMENT.md](./DEVELOPMENT.md) §7）。
> 各站的重投并发、累计计数与运行快照互不影响，后台与 `/api/errorqueue/process?site=` 都能单独触发。
>
> `Enabled: &true` 时**每个字段都得写**，缺一项 / 值越界启动就报（与 `crawler.breaker` 的
> 「enabled=true 时 threshold 必填」同一档）。

### archive —— 页面归档（失败时留下那一页）

```yaml
crawler:
  archive:
    enabled: true
    dir: "./logs/fetcher-html"
    mode: failure        # failure（默认）/ always
    retention: "168h"    # 默认 7 天；负数 = 永久保留
    max_file_mb: 8       # 默认 8
```

| 键 | 说明 |
| --- | --- |
| `enabled` | 是否开启；默认 `false` |
| `dir` | 归档根目录。**开启时必填**（空串会拼成"文件系统根"下的 `{stage}/…`，见校验层） |
| `mode` | `failure`（默认）= 只在**失败**的尝试落盘，写入量按失败率走（与 trace「只在失败尝试写 data」同构）；`always` = 每次尝试都落（排查期开，一天几万个文件很正常） |
| `retention` | 保留期；`0`/未写 = 默认 7 天（与 trace 对齐），**负数 = 永久保留** |
| `max_file_mb` | 单页上限（**MB**）；`0`/未写 = 8；超了不归档并记一条 WARN |

**它做什么**：把「失败那一刻抓到的那一页」原样落到

```
{dir}/{stage}/task-{id}-try-{retry}-{urlhash8}.html   ← 原始字节，可直接用浏览器打开
{dir}/{stage}/task-{id}-try-{retry}-{urlhash8}.json   ← 状态码 / 最终 URL / 字节数 / 时间
```

- **不用写任何代码**：`engine.FetchHTML` 抓到的页面自动登记进"本次尝试"的缓冲，尝试结束（**含 panic**）时按结局决定落不落盘。业务只管照常 `FetchHTML`。
- **必须是原始字节**：goquery 把文档重新序列化一遍会丢掉 `<noscript>` 里的回退标签 —— 延迟渲染的封面正藏在里面，而那恰恰是选择器坏掉时最该看的东西。所以落的是 `page.HTML`，不是 `doc.Html()`。
- **文件名带 `try`**（= `crawler_tasks.retry`）：同一任务重试 3 次不会互相覆盖，"第一次是登录墙、第三次正常"这种关键信息才留得住。
- **trace 里连得起来**：归档后引擎会补一条 `归档页面` 步骤，data 里就是文件名 —— 后台「追踪」抽屉里"失败"与"那一页"是同一条线索。
  （`always` 模式下**成功的**尝试里，这条步骤名还在但 data 会被 trace 的既有策略剥掉 —— 只有失败的尝试保留 data。那次尝试的文件照样在，按 `{dir}/{stage}/task-{id}-try-{retry}-*` 在目录里找即可。）
- **写盘失败不影响任务**：归档是排查辅助，写不进去只记 WARN（磁盘满、目录只读都不该让任务失败）。
- **两条抓取路径都覆盖**：`FetchHTML`（静态）与 `FetchRendered`（浏览器渲染）。后者的原始 HTML 本来就是 `page.HTML()` 读出来的（解析文档必须付的代价），登记不额外花 CDP 调用；它拿不到状态码与 Content-Type，元信息里留 0/空。
- 保留期清理每小时巡检一次（与 trace 的清理同频），按文件修改时间删，`ctx` 取消即收工。

> **后台能直接把那一页拿走**：「追踪」抽屉里带「下载这一页」按钮，走 `GET /api/task/page?file=<相对路径>` ——
> 服务端按 `archive.dir` 解析、拒绝 `..` 与绝对路径，回的是**文件本身**（`Content-Disposition: attachment`）
> 而不是内联渲染，拿去本地对着真实页面改选择器最省事。要按自己的路由暴露归档目录，用 `monitor/router.go` 的 `Routes(app)`。

### recover_queue —— 启动恢复（详见 [RECOVER_QUEUE.md](./RECOVER_QUEUE.md)）

**不在 config.yaml 里**，写法同 `ErrorQueue`（`RecoverQueue: &papa.RecoverQueueSpec{...}`）：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `Enabled` | `*bool` | **必写**；`&false` = 这一站重启后不捡回中断的任务 |
| `WorkerCount` | int | 并发重新入队的数量；开着时必填且 `> 0` |
| `BatchSize` | int | 每批查询处理的任务数；开着时必填且 `> 0` |

> 它**只在启动跑一次**（没有周期、没有后台手动入口），所以没有 `Interval` / `MaxRetry` ——
> 这两个字段在这个类型上**根本不存在**（写错是编译期错误，不是启动报错）。

### repeat_queue —— 周期轮询队列（详见 [REPEAT_QUEUE.md](./REPEAT_QUEUE.md)）

**不在 config.yaml 里**，写法同 `ErrorQueue`（`RepeatQueue: &papa.RepeatQueueSpec{...}`）：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `Enabled` | `*bool` | **必写**；`&false` = 本站没有轮询队列 |
| `WorkerCount` | int | 并发重新投递的数量；开着时必填且 `> 0` |
| `Interval` | string | **最粗兜底**的扫描间隔（如 `"2h"`）；开着时必填且 `> 0` |
| `BatchSize` | int | 每批查询处理的任务数；开着时必填且 `> 0` |

> 轮询队列**按站点跑**（后台一行一个站）。它有两层开关，**是「与」关系**：
> `RepeatQueue.Enabled` 是**声明层**的"这一站有没有这条队列"（改了要重启）；
> `SiteSpec.AutoRepeat` 是**运营层**的"要不要自动跑"（落库 `crawler_sites.auto_repeat`，库为事实，
> 后台「站点」页能随时开停、重启按库走）。见 [REPEAT_QUEUE.md](./REPEAT_QUEUE.md) 第 5 节。
>
> `Interval` 是"多久扫一次"的兜底；"**哪条任务**要轮询、**多久一次**"是任务行上的
> `repeatable` / `repeat_interval` 两列 —— 提交时用 `papa.Task{Repeatable: true, RepeatInterval: 10*time.Minute}`
> 播种（`repeat_interval` 为 0 = 用本站的 `Interval`），之后在后台任务表上用「开轮询」/「停轮询」/「设轮询周期」
> 或代码里 `engine.SetTaskRepeatable(id, on)` / `engine.SetTaskRepeatInterval(id, was, seconds)` 随时改
>（见 [REPEAT_QUEUE.md](./REPEAT_QUEUE.md) 第 3 节）。

### crawler_sites —— 站点表（框架自建，不是配置）

站点声明与运行统计的落库形态：一行一个站点（空 `key` 那行是**默认 scope**）。
启动时按声明播种（行不存在才插入；已存在只刷新 `base_url` / `stage_count`，**不动 `auto_repeat`** ——
库为事实），之后由引擎在"每站轮询跑完"与"熔断暂停/恢复"时按列更新统计。
它是后台站点 Tab 与站点概要与 `papa migrate` 的产物；**不落 Headers**（里面有 UA/Cookie）。

### business —— 业务自己的配置段（框架不解析）

爬虫之外的配置（封面目录、归档模式、站点特有开关……）写在 `business` 下面，框架**不解析、不校验**这一段的键名，原样透出给 App：

```yaml
business:
  covers:
    dir: ./covers
    size: 300
    interval: "5m"
  archive: true
```

```go
type CoversConfig struct {
    Dir      string        `mapstructure:"dir"`
    Size     int           `mapstructure:"size"`
    Interval time.Duration `mapstructure:"interval"`
}

var covers CoversConfig
// 解码走与框架配置同一套 hook：`"5m"` → time.Duration、`"a,b"` → []string
if err := app.Config.BusinessSection("covers", &covers); err != nil {
    panic(err) // 段没配、键名拼错、类型不对都在这里报出来
}
```

- **为什么要有这一段**：「未知键一律拒绝启动」那条规则是对的（拼错的键名不该静默失效），但它的副作用是业务没法在同一个 `config.yaml` 里放自己的配置 —— `covers.dir` 这类写进去就是启动 panic，只能另开一个文件自己解析、塞环境变量或写死成常量。
- **`BusinessSection` 仍然拒收业务段内部的未知键**（`ErrorUnused`）：框架不认业务键的语义，但"拼错了当没写"这件事不该发生在业务段里 —— `covers.dr` 会当场报错而不是留个零值。要完全自由的结构（键名由业务动态决定）就直接读 `app.Config.Business` 那个 map。
- **只作用于 `config.yaml`**：业务段就得写在这个文件里（本来就是给业务放的），改完重启生效。框架的校验管不到它，后台也不展示。

## 2. 建表 / 迁移

**只有一条路：显式跑一次。启动时不会自动迁移。**

- 业务项目（脚手架生成的，或任何 `go.mod` 依赖 papa 的项目）：`papa migrate` 会自动**转交**
  项目自己的迁移入口（`go run . -migrate` → `App.Migrate()`），建**框架自带的表 +
  你用 `papa.WithModels` 注册的业务模型**。Makefile 里的 `make migrate` 是同一件事。
- 不在业务项目里（或就在 papa 仓库里）：`papa migrate [-c configs/config.yaml]` 只建框架
  自带的表，会先打印将要确保存在的表。

**为什么业务项目要转交**：CLI 是独立编译的进程，业务注册的模型是业务模块里的 Go 类型，
它拿不到 —— 只有项目自己的进程里有。判据是当前目录的 `go.mod` 依赖了 papa（业务项目必然
直接 import 它，或本地 `replace` 它）。

**转交的前提是项目自己的 main 认识 `-migrate`**：脚手架生成的 `main.go` 把 `App.Migrate()`
接在这个参数上（Makefile 里是 `make migrate`）。自己写 main 的话记得照做 —— 否则
`papa migrate` 转交过去，就等于把你的爬虫直接启动了。

**`AutoMigrate` 只增不减**：加表、加列、加索引，不删列也不改类型，重复跑是幂等的 —— 生产上执行是安全的。
真正的破坏性变更（改名、改类型）gorm 不会替你猜，那得手工写迁移。

框架自带的表：`crawler_tasks`、`crawler_access_token` 总是建；
`crawler_operation_log`（`server.operation_log`）与 `crawler_task_trace`（`crawler.trace.enabled`）
跟着开关走 —— **先开开关再跑一次 `papa migrate`**，否则那张表不会建出来。

**业务自己的表由项目自己的进程建**：那些模型是通过 `papa.WithModels`（脚手架里是根 `models`
包的 `Models()`）在业务进程里注册的，所以 `papa migrate` 在业务项目里会转交给它。
用脚手架就别去记两条命令 —— `papa migrate` 与 `make migrate` 等价。

> 启动时会检查"该有的表在不在"，缺了打醒目的错误日志 —— 免得出现「开关看着是开的、实际什么都没写进去」。

## 3. 时长格式

- 标准 Go 时长字符串：`"500ms"` / `"5s"` / `"10m"` / `"6h"`。
- 阶段 `delay` 额外支持区间：`"10s-30s"` 表示在该区间内随机取一个间隔（反爬更隐蔽）。
