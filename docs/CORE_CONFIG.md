# Papa 爬虫核心配置手册

> 面向：配置 `configs/config.yaml` 时，每个字段是什么意思、怎么填。文件和 Go 声明改动需重启；运行治理可在 OA 保存后即时生效，见 [RUNTIME_CONTROL.md](RUNTIME_CONTROL.md)。
> 配套：本文是「配置字典」；各治理队列与后台行为见 [ERROR_QUEUE.md](./ERROR_QUEUE.md)、[RECOVER_QUEUE.md](./RECOVER_QUEUE.md)、[REPEAT_QUEUE.md](./REPEAT_QUEUE.md)、[MONITOR.md](./MONITOR.md)。框架已移除通用 cron 调度器。

---

## 0. 配置文件与加载

- 主配置：`configs/config.yaml`（`papa new` 生成，含中文注释）。
- 加载路径回退链：`papa.WithConfigPath(...)` 指定的路径 → 环境变量 `PAPA_CONFIG` → `configs/config.yaml`。
- **文件与运行覆盖分开**：改 `config.yaml` 或 Go 站点声明后需要重启；`runtime` 支持 OA 持久覆盖，保存后限制新工作并跨重启恢复。三个治理队列仍是全局启动配置，不能按站点覆盖；站点/阶段拓扑、物理 worker/队列容量和凭据来源不热改。任务自己的 repeat 周期仍可通过后台或 API 动态调整。
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
> 每条规则都区分三态：**没写**（用默认值，合法）／写了且合法／写了但越界。两类例外：
>
> 1. **三个必填键**（**没有**「没写也合法」那一态）：`log.dir`（空串会去写文件系统根）、
>    `db.max_idle_conns` / `db.max_open_conns`（0 在 `database/sql` 里是「不限」，与"漏写"是同一个数、分不开）。
> 2. **一族「开着才必填」**：`crawler.breaker.threshold`、`html.max_body_size`、`browser.pool_size`、
>    `crawler.archive.dir` —— 关掉对应开关时它们不参与校验（不许一个用不着的东西把人卡在启动上）。
>
> 各键见下面对应小节。
>
> 唯一的"不校验"的键是 **`business`**（见下）：框架不认它的语义，写什么键都不管 —— 那是留给业务自己的配置位，不是给框架配置的。

### network —— 采集出口

| 键 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `private_origins` | string[] | `[]` | 仅允许公网 HTTP(S)；内网按完整协议/主机/端口显式授权。不能带路径、查询、凭据或通配符，重启生效。完整用法与代理限制见文末「统一网络出口」 |
| `allowed_origins` | string[] | `[]` | 非空时所有出口进一步限制为这些精确 origin；仍需 `private_origins` 才能访问私网。空值不增加限制，重启生效。身份诊断只给独立诊断实例授予本次回环 origin，不改生产配置 |

### app —— 环境

| 键 | 类型 | 说明 |
| --- | --- | --- |
| `env` | string | `dev` / `prod`。只影响 SQL 日志（dev 打 info 且带参数值；见 `db.log_level`），不再影响建表 |

`papa new` 的 DSN 使用 `user:CHANGE_ME` 占位，需在本机替换后才能迁移/运行。`dev` SQL 日志可能含参数原文；诊断脱敏不保证识别任意 SQL 字面量，真实数据环境应限制日志访问并按需改用 `prod`。

### crawler —— 爬虫核心

> **阶段结构和默认值在 Go 声明里**：`WorkerCount` / `QueueSize` / `Delay` / `Retry` / 入口开关 /
> 站点归属写在 `internal/sites/<站名>.go`（各文件在 init 里自登记，`App.RegisterSites(papa.Sites()...)`）。
> `runtime.sites.<site>.stages.<stage>` 仅覆盖已有阶段的有效并发、固定任务间隔和账号池，不能新增阶段或扩大物理容量。
> 配置里留着的 `crawler.stages` 段会被"未知配置键"拦下（**故意的**，见下面的校验层说明）。

| 键 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `dedup_cache_size` | int | `0` | 内存去重表最大条目数；`0`=不限，`>0` 用 LRU 限界，淘汰条目由 DB 唯一索引兜底。启动预热每页最多 256 行，有限模式只取容量内最近活跃记录；`0` 仅查询暂存按页有界，总缓存仍不限。脚手架配置为 10000 |
| `queue_watermark` | float | `0.75` | 队列高水位比例（0-1），达到后新任务溢出到 DB 待回灌，避免满队列丢任务 |
| `drain_interval` | duration | `2s` | 溢出任务回灌队列的间隔 |
| `stop_timeout` | duration | `5s` | 优雅退出时 worker 与动态 ticker 共用从 Stop 入口开始的一份等待预算，不各等一个完整 timeout；不保证任意阻塞锁或外部调用的硬实时上限。超时则打错误日志，并**跳过**关库与关浏览器池，避免破坏仍在运行的 worker。认领或终态写入未确认的任务也计入未完成，即使其 worker 已因取消退出，也不会报告已排空。与 `server.shutdown_timeout` 不是一回事：那个等的是在途 HTTP 请求（如日志打包下载） |
| `stage_idle_timeout` | duration | `0`（不回收） | 阶段工作池连续空闲多久就回收（回收 = Stop 并撤出活池表，下次投递按需重建）。`0`/负数 = 常驻不回收。多站 / 长跑建议配，如 `10m` |
| `trace.enabled` | bool | `false` | 单任务步骤追踪：handler 用 `task.Trace.Step/Fail/Warn` 上报（`Warn` 是非致命档：任务成功但要留痕，如封面没下下来），写入 `crawler_task_trace` 表。引擎自己也会补几条：「任务失败」（失败尝试的**原因**，带分类与消息）、「归档页面」（带文件名，抽屉里可直接下载），以及浏览器捕获/等待失败时的「捕获网络请求」（候选请求样本）与「等待渲染」（卡住的 selector / js / request-idle）。关闭时 `task.Trace` 为 `nil`，调用是安全 no-op |
| `trace.retention` | duration | `168h` | 步骤记录保留期（后台按批清理）；填**负数**表示永久保留、不自动清理 |
| `breaker.enabled` | bool | `false` | **每个站点没单配阈值时用的默认值**：计数与阈值**每站一份**（一个站被墙不会把别的站一起闸住），阈值取这里这份；某个站要自己的阈值就写 `SiteSpec.Breaker`（这一项也会成为它的默认）。站点是必填维度，没有「未归属」那一档 |
| `breaker.window` | duration | `5m` | 常规统计窗口；精确记录窗口内终态失败任务轮次。成功不抵消失败，同一任务轮次重复上报只计一次 |
| `breaker.threshold` | int | — | 窗口内终态失败数达到它即暂停。**`enabled: true` 时必填且必须 > 0** —— 没写、写 `0` 或负数都会在配置加载校验时 panic；`breaker.NewDetector` 还保留构造层防线。它没有默认值：`threshold <= 0` 时「多少条才算熔断」没有答案，补一个数只会让人以为开着、数的却是另一回事 |
| `breaker.policy.short_window` | duration | `30s` | 短时爆发检测窗口 |
| `breaker.policy.fast_threshold` | int | `5` | 同阶段、同分类、同错误指纹的不同任务数达到此值，立即保护整站；阶段分类只用于定位和报告 |
| `breaker.policy.mixed_threshold` | int | `8` | 短窗口内失败的不同任务数达到此值、未命中快速或硬保护时，进入有期限的混合错误观察 |
| `breaker.policy.observe_for` | duration | `30s` | 最后一次失败后的混合观察时长；所有已注册阶段至少两个本轮不同任务成功且没有新失败，排空在途任务后退出观察；覆盖不足则探测。新增失败重计观察时间，不延长总恢复预算，不越过硬阈值 |
| `breaker.policy.cooldown` | duration | `30s` | 自动保护后的冷却时间 |
| `breaker.policy.probe_timeout` | duration | `10s` | 单次试探超时 |
| `breaker.policy.max_probes` | int | `3` | 一次自动恢复最多进行的整站试探轮数；每轮逐阶段验证全部阶段 |
| `breaker.policy.probe_interval` | duration | `30s` | 试探间隔；明确的 Retry-After 仍是下限 |
| `breaker.policy.max_recovery` | duration | `5m` | 本轮自动恢复时间预算，耗尽后保持暂停并追加本轮结果，一小时后重新复查 |
| `breaker.policy.ramp_for` | duration | `30s` | 试探通过后的低速恢复观察时长；到期且每个阶段都有本轮验证成功（混合错误为三个失败样本轮次）后停止继续放行，等待全站在途任务全部结束再确认恢复并清零 |
| `breaker.policy.ramp_concurrency` | int | `1` | 低速恢复阶段的并发上限 |
| `breaker.policy.interval` | duration | `1s` | 观察和低速恢复阶段的任务启动间隔下限 |
| `breaker.policy.capacity` | int | `4096` | 每站检测器保留的失败轮次上限；成功样本使用独立同容量空间。失败超限直接保护整站并标明证据受限，不通过丢弃失败降低计数 |

> **熔断按站点分组**：这一节配的是**默认值**；每个站点可以在
> `internal/sites/<站名>.go` 里用 `Breaker: &papa.BreakerSpec{...}` 覆盖（哪怕 `Enabled: false`
> 只关掉某一站）。每站一份计数是硬要求 —— 共用一份的话站点 A 被墙会把站点 B 一起闸住。
>
> **想"启动先别跑"不在这一节**：那是声明期的一道整站闸门 `SiteSpec.StartPaused: true`
> （入口任务照投、任务寄放在溢出表，后台点「恢复整站」才放开），见 [MONITOR.md](./MONITOR.md) §10。
> `policy` 的零值采用上表默认；负值不合法，时长不超过 24h，整数不超过 100000；`capacity` 必须覆盖短窗口两条阈值，`max_recovery` 至少覆盖冷却、单次试探和低速恢复时长。`BreakerSpec.Policy == nil` 继承配置文件策略；非 nil 用该站策略整体替换，未填字段采用框架默认。默认值是通用起点，应结合本站流量与报告调整。
>
> 这一节配置**判据与恢复预算**；"停"这个动作与暂停状态在引擎的暂停信号里（见 [MONITOR.md](./MONITOR.md) §9），
> 人工暂停写的是同一张表、来源标成 manual。

### browser —— 浏览器池（Rod）

稳定身份档案不存放在 `config.yaml`；通过「浏览器诊断」页或公开 API 创建版本、绑定主体并复验生效，见 [BROWSER_IDENTITY.md](BROWSER_IDENTITY.md)。没有绑定/显式 Profile 时保留旧行为。启用身份后，不允许 Device、UA/language 启动参数或普通请求头与身份字段冲突；物理池配置仍按本节管理。

> **默认不用它**：能用静态客户端拿到的页面一律用 `html`（`engine.FetchHTML`），rod 只在
> 静态 HTML 里拿不到目标数据时才上（判据与理由见 FETCHER_WRITING_GUIDE.md §3）。
> 打开 `enable` 不等于"该用它"—— 它只是把这条路允许起来。

| 键 | 类型 | 说明 |
| --- | --- | --- |
| `enable` | bool | 是否启用浏览器池 |
| `pool_size` | int | 代理浏览器**并发上限**（按需创建，非常驻实例数）。**`enable: true` 时必填且 > 0**，漏写、写 0 或负数会在配置加载时被拒绝；直接构造零容量 `Pool` 时，`Get` 也会立即返回 `browser pool not configured` |
| `direct_pool_size` | int | 强制直连浏览器并发上限（不走代理）。**留 0 是合法的** = 不用直连池 —— 真调到 `GetDirect` 时会明确报 `direct browsers not configured` |
| `max_idle_time` | duration | 空闲回收阈值，`0`=不回收 |
| `headless` | bool | 无头模式 |
| `no_sandbox` | bool | 关闭 Chromium sandbox |
| `leakless` | bool | leakless 进程守护（Windows 上其 exe 易被杀软误报，谨慎开启） |
| `binary` | string | 浏览器二进制从哪来：`rod`（默认）= **Rod 管理、缺了自动下载**的 Chromium，服务器上不用手动装；`system` = 系统已安装的 Chrome / Chromium / Edge（建池时探测，找不到就启动报错，不下载）；`path` = 用 `browser_path` 指定。留空 = 看 `browser_path`：给了就按 `path`，没给就按 `rod`。**`rod` / `system` 时不能再给 `browser_path`**（来源冲突，配置加载即报错） |
| `temp_dir` | string | 临时用户数据根目录，空值沿用 Rod 默认系统临时路径。Linux 可设 `/dev/shm/crawler`；创建并检查可写，不可用时报错，不回退。每实例独立子目录，关闭后删除子目录。不能与 `flags.user-data-dir` / `flags.disk-cache-dir` 同配；修改后重启。Chromium 子进程 TMPDIR 同步使用该根目录。 |
| `browser_path` | string | 可执行文件路径，仅在 `binary: path`（或 `binary` 留空）时生效 |
| `flags` | map | 额外 Chromium 启动参数（launcher flags）：键是开关名（前导 `--` 可省），值为空 = 布尔开关（`--name`），否则拼成 `--name=value`。**值一律按字符串解码**，数字/布尔要加引号（`renderer-process-limit: "2"`）；不带值写 `disable-gpu:` 表示布尔开关。例：`disable-gpu:`、`blink-settings: imagesEnabled=false`。**只放 Chromium 自己的开关**：`headless` / `no_sandbox` / `leakless` / 代理各有配置键，flags 最后应用、会覆盖它们；`rod-` 前缀是 Rod 内部参数，写进来会被拒绝 |
| `self_check` | bool | 启动自检，默认 `false`：建池时真实启动一次浏览器再关掉，起不来就启动失败。`true` 时 `rod` 模式会在**启动阶段**触发 Chromium 下载（Linux 下落在 `~/.cache/rod/browser`，Windows 是 `%APPDATA%\rod\browser`）—— 服务器部署建议打开，把「下载失败 / 缺系统库 / 路径写错」暴露在启动时，而不是第一次抓取时。注意它只验证浏览器二进制本身：Linux 上缺 `libnss3` 这类**系统共享库**仍要自己装（Rod 只下载 Chromium，不装 apt 包） |
| `headers` | map | **全局**默认请求头（与内置默认头合并，同名覆盖）。**通常不写这里**：与站点相关的头写在 `internal/sites/<站名>.go` 的 `Headers`（站点级）—— 它可被该层与**逐请求**（`papa.WithHeaders(ctx, …)`）覆盖，站点级写空值表示删掉那个头；这一层留给"与站点无关、所有站共用"的头 |

### html —— 静态 HTML 客户端

> **这是默认路径**：站点的页面只要 HTML 里翻得到目标节点就用它 —— 页面上有 JS、有交互也不例外
> （见 FETCHER_WRITING_GUIDE.md §3 那条规则）。

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

> 这一节配的是**代理池**（`api_url` 拉列表 + 定时刷新）。健康策略另见 `runtime.proxy_health`。
> 自行选择时使用 `engine.GetProxy().NextChecked(ctx)` 并处理耗尽错误；`Next()` / `NextProxy()` 仅保留旧字符串兼容。
> `papa.WithProxyURL(ctx, addr)` 传给一次静态抓取，
> `filedown.DownloadOptions{Proxy: addr}` / `m3u8.DownloadOptions{Proxy: addr}` 传给一次下载。
> 若这次下载就该跟着任务的抓取出口走，用 `filedown.OptionsFromRequest(ctx, referer)` /
> `m3u8.OptionsFromRequest(ctx, referer)` 一步拿到（它会把 ctx 上的显式代理与站点级/逐请求头一起填好）。
> 浏览器渲染**不支持**逐请求改代理地址（Chrome 限制）；出口在实例创建时固定，账号专用池也不在途换出口。

### runtime —— 运行治理（默认关闭）

优先级：框架默认 → Go 声明 → YAML 显式字段 → OA 持久覆盖。`defaults` 是站点默认策略；`sites.<已注册站点键>` 支持同样字段，站点内字段覆盖同来源默认值。仅提供的字段参与合并，`false` / `0` / 空池名均有明确覆盖意义；省略或在 OA 清除覆盖才表示继承。

下面的请求键位于 `runtime.defaults.request` 或 `runtime.sites.<site>.request`：

| 键 | 默认 | 说明 |
| --- | --- | --- |
| `enabled` | `false` | 开启该站请求硬限制与自动调节；跨阶段、跨账号共用。关闭也遵守 429/503 的有效 Retry-After |
| `max_inflight` | `0` | 同站实际请求在途上限；0 不设此上限，非负 |
| `requests_per_second` / `burst` | `0` / `1` | 速率 0 不限；正速率要求 burst ≥ 1 |
| `manual_delay` | `"0s"` | 手动请求间隔；与阶段任务间隔独立 |
| `retry_after_default` / `retry_after_max` | `"30s"` / `"1h"` | 请求控制与 AutoThrottle 都开启时，429 缺少有效头的默认冷却；最大自动等待预算。超过预算需确认，但不截短服务端期限；关闭请求控制且预算未设正数时用默认 1h |
| `autothrottle.enabled` | `false` | 请求控制开启后自适应延迟；限流时先检查开关，开启才降频，绝不自动开启。关闭后保持固定间隔和已启用硬限制，不额外添加无头 429 默认冷却 |
| `autothrottle.min_delay` / `max_delay` | `"100ms"` / `"30s"` | 自动间隔范围，最大值不得小于最小值 |
| `autothrottle.target_concurrency` / `smoothing` | `1` / `0.5` | 启用时目标并发 > 0，平滑系数在 (0,1] |

有效 `Retry-After` 支持秒数与 HTTP 日期，HTTP、浏览器普通访问和恢复试探均须等待站点期限；较短的新期限不能缩短已有冷却。开启 AutoThrottle 时，429 或带有效头的 503 会增加间隔（通常翻倍、零间隔至少增加 1ms，最大不超过 `max_delay`），即使拒绝响应很快也不加速。配置关闭不会阻止错误计数、熔断分析或有效远端等待。

站点级其他键位于 `runtime.defaults` 或 `runtime.sites.<site>`：

| 键 | 默认 | 说明 |
| --- | --- | --- |
| `weight` / `min_weight` | `1` / `1` | 0..10000；资源可用时分配服务机会，不承诺故障或紧急内存保护时强制保留名额 |
| `pool` | 空 | 新任务默认账号池名；空不选池，已有任务保存的归属不被重写 |
| `stages.<stage>.max_concurrent` | Go `WorkerCount` | 已注册阶段的有效执行并发，不超过物理 worker 数；0 表示沿用物理上限 |
| `stages.<stage>.delay` | Go `Delay` | 显式覆盖为固定时长；清除覆盖恢复声明的固定/随机范围 |
| `stages.<stage>.pool` | 继承站点池 | 非空阶段池优先；空阶段值仍使用站点池 |

全局资源键位于 `runtime.resources`：

| 键 | 默认 | 说明 |
| --- | --- | --- |
| `enabled` / `sample_interval` | `false` / `"2s"` | 独立于后台页面的采样与放行控制 |
| `cpu_high` / `cpu_recover` / `high_windows` | `85` / `65` / `3` | CPU 百分比及连续高负载窗口；启用时高阈值大于恢复阈值 |
| `memory_high` / `memory_recover` | `90` / `75` | 内存百分比滞回阈值 |
| `minimum_available_bytes` | `0` | 最低可用内存字节数；0 不加此项门槛 |
| `task_max` / `browser_max` | `32` / `4` | 任务执行、浏览器借用的全局许可上限；启用时均 > 0，物理容量仍是上限 |
| `shrink_factor` | `0.5` | 降额后保留比例；启用时在 (0,1) |
| `recovery_step` / `recovery_interval` | `1` / `"30s"` | 每次恢复许可数和最小恢复间隔；采样无效时不自动上调 |

代理健康键位于 `runtime.proxy_health`，不放在 `proxy.api_url` 同级字段中：

| 键 | 默认 | 说明 |
| --- | --- | --- |
| `enabled` | `false` | 开启代理反馈、冷却及检查式选择；耗尽不回退直连 |
| `failure_threshold` | `3` | 已确认代理自身故障的连续失败阈值 |
| `initial_cooldown` / `max_cooldown` | `"30s"` / `"10m"` | 失败探测指数退避；正数、顺序合法，最大不超过 24h |
| `max_probes` / `recovery_successes` | `1` / `2` | 全管理器同时探测数、恢复所需成功数；计数项范围 1..10000 |

YAML 时长写字符串；运行 API JSON 的时长使用整数纳秒。策略持久化到 `crawler_config.runtime_policy`，revision 以表列为权威；读取失败不回退更宽松默认。只读页面不触发账号验证或代理探测。流程、请求覆盖与浏览器边界见 [RUNTIME_CONTROL.md](RUNTIME_CONTROL.md)。

账号健康政策、使用预算与备用预验证不是 `runtime` YAML 的另一份覆盖：前者按账号保存到管理状态，后者按池保存，可由 OA 动态调整。默认缓冲 10 秒、两次间隔确认、未知结果退避；使用额度与自动恢复默认关闭。正式字段、默认值及跨池共享语义见 [ACCOUNT_RUNTIME.md](ACCOUNT_RUNTIME.md)。账号接入需由业务显式注册 Provider 和外部密钥支持的 Secrets 存储，不在 YAML 或任务 Meta 中保存明文密码，见 [ACCOUNT_ONBOARDING.md](ACCOUNT_ONBOARDING.md)。

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
| `max_size` | int | 单文件大小上限（MB），默认 100 |
| `max_days` / `max_backups` | int | 保留天数 / 备份数；**0（或没配）= 默认 7 天 / 3 份**（不是"永久保留"，要留久就给大数） |
| `compress` / `local_time` | bool | 是否压缩 / 本地时间 |
| `level` | string | 日志级别：`debug` / `info` / `warn` / `warning` / `error`（`warning` 是 `warn` 别名）；留空 = `info`。非法值启动即报错。**只影响人类日志**；机器消费走 `obs.events` |

### obs —— 运行事件流与报告产物（默认关）

> 面向自动化作业：让「一次运行结束后出报告」有机器可读的数据源。**默认全关** —— 不写这段时行为与没有观测体系时一致。
> 框架负责事件流（写入 + 契约）与报告引擎（`papa report generate`，产物 4 个文件）；使用姿势、报告怎么读、业务怎么扩展见 [OBSERVABILITY.md](./OBSERVABILITY.md)。

| 键 | 类型 | 说明 |
| --- | --- | --- |
| `events.enabled` | bool | 是否写运行事件流，默认 `false` |
| `events.file` | string | 文件名（相对 `log.dir`），默认 `events.jsonl`；轮转沿用 `log.max_size` / `max_days` / `max_backups` / `compress` / `local_time` |
| `events.buffer` | int | 异步队列长度，默认 8192。**满了丢弃并计数，绝不阻塞 worker** |
| `events.snapshot_interval` | duration | `stage_snapshot`（阶段/队列周期快照）的间隔，如 `30s`；`0` / 不写 = 不发 |
| `report.on_exit` | bool | 进程退出时生成报告，默认 `false`。开启后退出流程在关库前生成一份报告（失败只记日志，不阻塞退出） |
| `report.interval` | duration | **定时生成报告**：如 `30m` = Run 期间每 30 分钟刷新一次当天日报；`0` / 不写 = 不自动生成（默认关）。与 `on_exit` 独立，两者都开时写同一天目录、后写覆盖前写（当天最新快照）；定时器停机时先停、再出退出报告 |
| `report.mode` | string | 判定语义：`auto`（默认）在定时、退出和 CLI 中均使用 `snapshot` 运行报告，回答运行状态与异常；`acceptance` 为显式批次验收，检查本轮是否通过、期末是否排空。配置写 `acceptance` 仍生效。CLI 的 `--snapshot` / `--acceptance` 优先于本键且互斥。CLI 默认逐个有事件的本地日生成日报，仅显式 `--snapshot` 限为最后一天；进程内自动报告只写当天。其它值启动即报错 |
| `report.dir` | string | 报告产物目录，默认 `./reports`。产物按**本地日历日**拆分：`<dir>/<2006-01-02>/`（一份 = 一天的日报；`--run` 时 `<dir>/<run_id>/`），四份 `report.md` / `report.json` / `ai-brief.md` / `ai-brief.json`；后台「obs报告」页也读它 |
| `report.retention` | duration | 报告保留期，默认 `168h`（7 天）；`0` / 不写同默认；负数 = 永久保留、不自动清理。**每次生成报告后**（`papa report generate` / `report.interval` / `report.on_exit`）清理 `<dir>` 下过期的报告目录：只删有 `report.md` 的直接子目录；日报目录按名字里的日期算（保留最近 N 个自然日），旧命名（`run_id` / `window-*`）按目录修改时间算 |
| `report.timeout` | duration | 单次生成报告的超时，默认 `120s`（`report.interval` 与 `report.on_exit` 共用） |
| `report.thresholds.stuck_after` | duration | 判「卡住」的阈值（processing 超过它未更新），默认 `10m`。**报告口径**，不是启动恢复判据 |
| `report.thresholds.crawl_fail_rate_max` | float | 抓取终态失败率上限，默认 `0.05`（未校准） |
| `report.thresholds.event_max_gap` | duration | 事件时间断层阈值，默认 `10m`：事件流里相邻事件间隔超过它即算一次断层（丢事件 / 被硬杀）。**已用于 `event_coverage` 检查**：超过阈值、或窗口起点后超过阈值才有首条事件，该检查判未通过（warn），并向 `degraded` 写「事件存在时间断层，事件口径可能不完整」。**例外**：断层若正好是一段 `run_stopped` → `run_started` 的停机区间（起止秒级吻合），视为引擎没在跑而不是丢事件 —— 检查判通过、message 写「引擎停机 …，不是事件丢失」，且不写 degraded。`snapshot` 模式下引擎暂停/空闲时该检查标不适用。`0` / 不写 = 默认，负数启动报错。吞吐/ETA 的时长另按各 run 首末可见事件区间合并计算（含暂停），不按整日减停机；无正的事件跨度不判零吞吐异常 |

**事件契约（v1，只增不改）**：一行一个 JSON 对象，字段 `v`（schema 版本）、`seq`（本 run 内单调递增）、`ts`、`run_id`、`type`，以及按类型可选的 `site` / `stage` / `task_id` / `attempt` / `duration_ms` / `queue` / `counts` / `backlog` / `attrs` / `stages` / `queues` 等。其中 `attrs` 是**业务自定义键值对**（框架不解释语义），业务事件需要携带框架不认识的维度时放这里。读者必须忽略未知键；`seq` 出现缺口 = 丢弃或丢失。

事件类型（常量见 `pkg/events/event.go`）：

| 类别 | 事件 |
| --- | --- |
| 运行 | `run_started` / `run_stopped` |
| 任务 | `task_claimed` / `task_finished` / `task_failed` / `task_retry_scheduled` |
| 队列 | `queue_round` / `task_requeued` / `backlog_sampled` |
| 运行时 | `stage_snapshot` / `stage_pool` / `spill_changed` / `breaker_tripped` / `pause_resumed` |

业务侧可 `app.Events().Emit(events.Event{...})` 复用同一个 sink（同一 `run_id` / `seq` 序列）；业务事件类型由业务自己约定，框架只负责承载与落盘。

报告扩展点：`app.UseReportSection(section...)` / `app.UseReportPattern(pattern...)` 在 `Run` 之前注册业务报告段与业务模式；不注册时报告只有框架通用段。报告产物默认写两份文档：`report.md`（人读）与 `ai-brief.md`（交给另一个 AI 的排查细则，含边界/证据/假设/验证/验收），各带一份同构 JSON。

**已知限制（这一版）**：`queue_wait_ms` 不发（需要在任务上记录提交时刻）；`queue_round` 只带本轮处理量，不带每轮失败数。

### server —— 监控 HTTP 服务（详见 [MONITOR.md](./MONITOR.md)）

| 键 | 类型 | 说明 |
| --- | --- | --- |
| `enabled` | bool | 是否开启统一 HTTP 服务。开则挂载整个监控后台（`/monitor` + `/api/*` + 表格页 + 自定义页 + `UseRouter` 注册的路由）；关则**服务完全不监听**，上面这些一律不挂载。原来还有一个 `monitor` 子开关，但它唯一的效果是「起了 HTTP 服务却什么都不挂」（所有路径 404），已移除 |
| `port` | int | 监听端口 |
| `monitor_dirs` | map | 监控页展示的业务目录占用 `name: path` |
| `queue_sample_interval` | duration | 三个治理队列「待处理」积压数的采样间隔，默认 `1m`。监控页刷新只读内存快照，仅采样时查库；调大可降低 DB 压力 |
| （无密钥字段） | — | 后台凭据是 `crawler_access_token` 表里的多条**访问令牌**（每条属于一个操作人）；管理接口始终要求有效令牌，空表也拒绝访问；首把或恢复令牌在服务器本机用 `papa token add --operator <名字>` 创建 |
| `operation_log` | bool | 操作日志开关，默认 `false`。开启后后台所有增删改操作写入 `crawler_operation_log` 表（含失败，并记下**操作人**——来自访问令牌；表格动作、访问令牌页、队列「立即执行」、熔断放行四类都记），「系统管理」显示「操作日志」只读表格页；关闭时不建表、不写库，菜单项也不出现 |
| `read_header_timeout` | duration | 默认 `10s`；读取请求头的时间上限 |
| `read_timeout` | duration | 默认 `30s`；读完整请求（含 body）的上限 |
| `write_timeout` | duration | 默认 `0`（不限）；日志打包下载、浏览器诊断可能持续较久，显式设限时应覆盖这些请求的执行时间，否则连接可能提前结束 |
| `idle_timeout` | duration | 默认 `60s`；keep-alive 空闲连接的上限 |
| `shutdown_timeout` | duration | 默认 `10s`；优雅退出时等在途请求（如日志下载）跑完的上限，超时才强制断开 |

### error_queue —— 失败任务错误队列（详见 [ERROR_QUEUE.md](./ERROR_QUEUE.md)）

写在 `config.yaml` 顶层的全局段（**不再按站点拆**）：

```yaml
error_queue:
  enabled: true               # 失败任务重投；false / 不写 = 不跑
  worker_count: 1             # 并发重新投递数；开启时必填 > 0
  interval: "4h"              # 自动轮询间隔；"0" = 不自动，仅后台手动触发
  max_retry: 3                # 单个任务最多再处理代数；0 = 不限
  batch_size: 100             # 每批查询处理的任务数；0 = 默认 1000（分页流式，不能为负）
```

> 错误队列是**全局一条**：扫所有站点的 failed 任务重投回各自的阶段，后台一行。
> 开着（`enabled: true`）时 `worker_count` 必填 > 0、`batch_size`（0 = 默认 1000）与 `max_retry` 不能为负、
> `interval` 不能为负 —— 缺一项 / 越界启动就报（与 `crawler.breaker` 的
> 「enabled=true 时 threshold 必填」同一档）。参数启动时读一次，改完要重启。

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
| `dir` | 归档根目录。**开启时必填**（空串会拼成"文件系统根"下的 `{site}/{stage}/…`，见校验层） |
| `mode` | `failure`（默认）= 只在**失败**的尝试落盘；`always` = 成功尝试也落盘 |
| `retention` | 保留期；`0`/未写 = 默认 7 天（与 trace 对齐），**负数 = 永久保留** |
| `max_file_mb` | 单页上限（**MB**）；`0`/未写 = 8。原始 HTML 归档超限时跳过并记 WARN |

**原始 HTML 归档**：`FetchHTML` 等既有入口把「失败那一刻抓到的那一页」原样落到

```
{dir}/{site}/{stage}/task-{id}-try-{retry}-{urlhash8}.html   ← 原始字节，可直接用浏览器打开
{dir}/{site}/{stage}/task-{id}-try-{retry}-{urlhash8}.json   ← 状态码 / 最终 URL / 字节数 / 时间
```

- **不用写任何代码**：`engine.FetchHTML` 抓到的页面自动登记进"本次尝试"的缓冲，尝试结束（**含 panic**）时按结局决定落不落盘。业务只管照常 `FetchHTML`。
- **必须是原始字节**：goquery 把文档重新序列化一遍会丢掉 `<noscript>` 里的回退标签 —— 延迟渲染的封面正藏在里面，而那恰恰是选择器坏掉时最该看的东西。所以落的是 `page.HTML`，不是 `doc.Html()`。
- **文件名带 `try`**（= `crawler_tasks.retry`）：同一任务重试 3 次不会互相覆盖，"第一次是登录墙、第三次正常"这种关键信息才留得住。
- **trace 里连得起来**：归档后引擎会补一条 `归档页面` 步骤，data 里就是文件名 —— 后台「追踪」抽屉里"失败"与"那一页"是同一条线索。
  （`always` 模式下**成功的**尝试里，这条步骤名还在但 data 会被 trace 的既有策略剥掉 —— 只有失败的尝试保留 data。那次尝试的文件照样在，按 `{dir}/{site}/{stage}/task-{id}-try-{retry}-*` 在目录里找即可。）
- **写盘失败不影响任务**：归档是排查辅助，写不进去只记 WARN（磁盘满、目录只读都不该让任务失败）。
- **几条抓取路径都覆盖**：`FetchHTML`（静态）、`FetchRendered` / `FetchRenderedPage(With)`（渲染）与
  `engine.CaptureRequest`（捕获）。渲染/捕获的原始 HTML 本来就已读出来，登记不额外花 CDP 调用；
  等待条件失败时由 `browser.WaitError.HTML` 带出、捕获失败时由 `CaptureResult.HTML` 带出，
  引擎层统一登记。它们拿不到状态码与 Content-Type，元信息里留 0/空。
- 保留期清理每小时巡检一次（与 trace 的清理同频），按文件修改时间删，`ctx` 取消即收工。

> **后台能直接把那一页拿走**：「追踪」抽屉里带「下载这一页」按钮，走 `GET /api/task/page?file=<相对路径>` ——
> 服务端按 `archive.dir` 解析、拒绝 `..` 与绝对路径，回的是**文件本身**（`Content-Disposition: attachment`）
> 而不是内联渲染，拿去本地对着真实页面改选择器最省事。要按自己的路由暴露归档目录，用 `monitor/router.go` 的 `Routes(app)`。

### recover_queue —— 启动恢复（详见 [RECOVER_QUEUE.md](./RECOVER_QUEUE.md)）

写在 `config.yaml` 顶层的全局段（**不再按站点拆**）：

```yaml
recover_queue:
  enabled: true               # 启动恢复；false / 不写 = 不跑
  worker_count: 1             # 并发重新入队的数量；开启时必填 > 0
  batch_size: 100             # 每批查询处理的任务数；0 = 默认 1000（不能为负）
  batch_pause: "30m"          # 批间停顿；不写/0 = 默认 30m，要连续跑完写很小的正数
```

> 它**只在启动跑一次**（没有周期、没有后台手动入口），所以没有 `interval` / `max_retry`。
> 恢复是**分批**投递的：每批 `batch_size` 条投完停 `batch_pause`（默认 30m）再继续，
> 避免重启瞬间把几千条积压一次灌进各阶段队列；停机信号可立即打断停顿。

### repeat_queue —— 周期轮询队列（详见 [REPEAT_QUEUE.md](./REPEAT_QUEUE.md)）

写在 `config.yaml` 顶层的全局段（**不再按站点拆**）：

```yaml
repeat_queue:
  enabled: true               # 周期轮询；false / 不写 = 不跑
  worker_count: 1             # 并发重新投递的数量；开启时必填 > 0
  interval: "2h"              # 最粗兜底的扫描间隔；开启时必填 > 0
  batch_size: 100             # 每批查询处理的任务数；0 = 默认 1000（不能为负）
```

> 轮询队列是**全局一条**（后台一行）。"要不要自动跑"只有一层：`enabled`（false 就是仅手动触发）。
> `interval` 是"多久扫一次"的兜底；"**哪条任务**要轮询、**多久一次**"是任务行上的
> `repeatable` / `repeat_interval` 两列 —— 提交时用 `papa.Task{Repeatable: true, RepeatInterval: 10*time.Minute}`
> 播种（`repeat_interval` 为 0 = 用全局 `interval`），之后在后台任务表上用「开轮询」/「停轮询」/「设轮询周期」
> 或代码里 `engine.SetTaskRepeatable(id, on)` / `engine.SetTaskRepeatInterval(id, was, seconds)` 随时改
>（见 [REPEAT_QUEUE.md](./REPEAT_QUEUE.md) 第 3 节）。

### crawler_config 的 `sites` 键 —— 站点快照（**内存是源，库是投影**）

站点信息不再单独一张表：它落在 `crawler_config` 的 `cfg_key = sites` 一行上，值是站点数组
（`key` / `base_url` / `stage_count` / `breaker_paused` / `breaker_paused_at`）。

| 谁 | 什么时候动它 |
| --- | --- |
| 引擎启动 | 读这一行：**没有就按声明生成**；有就载入内存。按当前声明对齐站点集合 / `base_url` / `stage_count`；暂停投影以运行闸门为准：先恢复 `crawler_breaker_incidents` 的活动事件，再应用 `StartPaused`。人工暂停不从此投影恢复；完全接管自动保护时结束自动事件，自动保护自身的待人工处理状态仍按持久事件恢复。整份快照一次写回，内容一致就不回写 |
| 后台点「暂停整站 / 恢复整站」 | 改内存那份 + 整块回写这一行（在读写口那把写锁里做读-改-写） |
| 后台「站点」页 `/api/monitor` 的 `sites` | **读内存**（读写口的内存缓存），不查库 |

**不要手改这一行**：它是投影，下一次启动重建就把你的改动覆盖掉；`base_url` / `stage_count` 也是
按声明抄的（改声明要重启，直接改库不生效）。**不落 Headers**（里面有 UA/Cookie）。
从更早版本升上来的库，`crawler_sites` 那张表已经没有代码再用它了，手工 `DROP TABLE crawler_sites` 即可。

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

**转交的前提是项目自己的 main 认识 `-migrate`**：脚手架生成的 `internal/app/run.go` 把 `App.Migrate()`
接在这个参数上（Makefile 里是 `make migrate`）。自己写 main 的话记得照做 —— 否则
`papa migrate` 转交过去，就等于把你的爬虫直接启动了。

底层使用 GORM `AutoMigrate` 补齐框架表与注册模型。它不会替业务推断删列、改名等破坏性意图；不同数据库对类型和约束迁移的能力也不完全相同。重复运行通常是幂等的，但生产环境不要把它理解成“无条件安全”：先备份、审查生成 SQL，并为改名、改类型、数据回填等操作自己写迁移 SQL。

**框架不为更早版本的表结构负责**：`papa migrate` 只做 `AutoMigrate`（建表 / 补列 / 补索引），
不再带任何"给旧库补课"的显式迁移（删旧索引、改列类型、回填、删死列那些都删掉了）。
从更早版本升级时，应核对已有 schema 和数据。`AutoMigrate` 仍可能调整其支持的类型或约束，但不会执行上述历史专项修复，也不会代替业务的数据迁移脚本。

框架自带的表：`crawler_tasks`、`crawler_breaker_incidents`、`crawler_access_token`、`crawler_config`、`crawler_runtime_policy_history`、`crawler_browser_profiles`、`crawler_browser_identity_bindings`、`crawler_account_pools`、`crawler_account_management`、`crawler_account_profiles` 总是建；
`crawler_operation_log`（`server.operation_log`）、`crawler_task_trace`（`crawler.trace.enabled`）、`crawler_task_flows`（`task_flow.enabled`）
跟着开关走 —— **先开开关再跑一次 `papa migrate`**，否则对应表不会建出来。

运行治理升级补 `crawler_config.revision` 与 `crawler_tasks.account_pool`；本轮新增任务失败类别、补偿条件/时间/原因/CAS 版本、最近账号与选择版本、最近 8 次尝试 JSON。账号池表保存成员、当前选择和有界切换记录；管理表保存健康政策、核验进度、预算与有界事件，不保存 Cookie/Token。账号资料表只保存资料与加密存储引用，策略历史表保留最近 100 个覆盖版本。普通启动只检查结构并加载状态，不自动加表/列；即使默认未启用控制器，也应完成显式迁移。

**业务自己的表由项目自己的进程建**：那些模型是通过 `papa.WithModels`（脚手架里是 `internal/models`
包的 `Models()`）在业务进程里注册的，所以 `papa migrate` 在业务项目里会转交给它。
用脚手架就别去记两条命令 —— `papa migrate` 与 `make migrate` 等价。

> 启动时会检查"该有的表在不在"，缺了打醒目的错误日志 —— 免得出现「开关看着是开的、实际什么都没写进去」。

## 3. 时长格式

- 标准 Go 时长字符串：`"500ms"` / `"5s"` / `"10m"` / `"6h"`。
- Go `StageSpec.Delay` 额外支持区间：`"10s-30s"` 表示在该区间内随机取间隔；`runtime` 的阶段 `delay` 覆盖值只接受单一固定时长。OA 展示 Go 声明范围，清除覆盖恢复该范围。
- HTTP JSON 中的 duration 是整数纳秒，`"5s"` 只用于 YAML/Go 声明字符串；OA 表单按秒换算。

### 阶段轮询次数上限

轮询上限属于业务站点声明，不是 YAML 配置项：在 `papa.StageSpec` 上设置 `RepeatLimit: 30`。0 表示不限；达到数据库 `repeat` 计数上限后，该站该阶段的任务不再被自动或强制轮询，恢复及错误重投不受此限，不关闭任务的 `repeatable`。已有 `repeat` 列继续使用，不需要迁移。完整计数含义见 [REPEAT_QUEUE.md](./REPEAT_QUEUE.md)。

## 动态调度开关

`task_flow.enabled` 默认 false；启用后显式 `papa migrate` 创建运行策略表。升级本版本须迁移 crawler_tasks 的四个调度列（关闭开关也需要），普通启动不迁移。用法、限制与动态切换见 [动态阶段调度](TASK_FLOW.md)。


## 统一网络出口（network）

`network.private_origins` 默认 `[]`：框架采集出口只允许公网 HTTP(S)，拒绝回环、私网、链路本地、保留地址和非 HTTP(S) URL。内网采集必须显式配置完整 origin，不能带路径、查询、凭据或通配符：

```yaml
network:
  private_origins:
    - "https://intranet.example:8443"
    - "http://127.0.0.1:8123"
```

授权仅匹配指定协议、主机和端口，`localhost` 与 `127.0.0.1` 是不同 origin；省略默认端口与显式 `:80`/`:443` 等价。它不自动授权子域或其他端口，也不限制路径；不要把整条任务 URL 填入。DNS 返回的全部 IP 都须通过检查，实际连接使用核准的 IP。入口获准不代表重定向、分片、密钥或页面子资源的其他 origin 获准。

此配置下发给 Engine 的 HTML/FetchHTTP、浏览器池和两个内置下载器，也用于 `papa html`、`papa rod`、`papa diff`；重启生效，不支持 OA 热更或逐任务放宽。底层构造器的 `Network` 字段使用同一 `egress.Config`（门面别名 `papa.NetworkConfig`）。自行创建的下载器/客户端不会自动读取应用配置，宿主必须显式传入。构造时复制授权；下载器的 Config 副本、With 与 SetClient 不改变原授权。

Webhook 使用 `Webhook.Network`；代理列表 API 使用 `proxy.NewManagerWithNetwork`。它们不会自动继承页面给出的授权。

兼容性：原先隐式访问内网的业务需补授权；标准自定义 `http.Client` 保留超时、Jar、重定向及 TLS 配置，但拨号与协议处理受框架控制，不接受自定义 RoundTripper。默认不继承环境代理。显式 HTTP/SOCKS5 上游代理须支持固定 IP 隧道（HTTP 代理必须支持 CONNECT，包括 HTTP 目标端口）；当前不支持 HTTPS 上游代理，失败不会转直连。经上游代理的请求暂不复用连接，存在额外握手成本。

主机名使用 ASCII；国际化域名请先转换为 Punycode。拒绝尾点、旧式数字 IP、带作用域的 IPv6 等歧义形式。上游代理地址属于宿主显式配置的基础设施，不受目标站私网白名单限制；不能把页面、模型输出或未经验证的任务字段直接作为代理地址。

M3U8 合并调用 FFmpeg 时显式使用 `-protocol_whitelist file`，仅处理已经下载、解密的本地片段；不会因媒体中的嵌套清单再次联网。业务自行调用外部工具不在此约束内。

浏览器安全参数覆盖冲突的用户启动参数，禁用直接 DNS、QUIC、非代理 WebRTC、扩展与相关旁路；仅为本机网关排除回环地址的 DNS 禁止规则，页面回环流量仍经过网关。每实例网关随浏览器关闭，普通 HTTP 升级（`ws://`）暂不支持，`wss://` 经过受控 CONNECT。用户自行使用原生 Rod/CDP、操作系统网络、外部程序或自定义业务 HTTP 客户端属于可信扩展，框架出口策略不是系统级沙箱，也不管理数据库等基础设施连接或浏览器二进制下载。

### 标准任务身份升级

本次升级必须停止所有 worker 后显式执行 `papa migrate`。迁移先增加可空的身份哈希列及 `(site, stage, identity_hash)` 唯一索引，再分批回填已有任务；旧 IdempotencyKey 映射为 ID 类型，否则使用规范 URL。回填成功后才删除 `idx_site_stage_url_hash` 和 `idx_idempotency_hash`。不删除任何任务或业务数据。

规范 URL 可能让原来不同的 URL 合并为同一身份（例如默认端口、fragment）。遇到这种冲突或非法历史 URL，迁移报错并保留旧索引，不自动合并；运维核对任务后显式解决，再重跑迁移。失败前已经完成的回填可保留，重复迁移会跳过，不要求整批DDL事务回滚。迁移未成功完成之前不要启动新版本；新列允许 NULL 仅为升级过渡，所有正常创建入口都会生成身份。大表建唯一索引应预留维护窗口并先备份。


熔断升级须显式迁移创建 `crawler_breaker_history`，用于 obs 当日过程与重启历史；现有 `crawler_breaker_incidents` 保留控制快照。长期保护固定每小时复查，遵守 Retry-After，人工暂停不自动解除。取样和历史规则见 [BREAKER.md](BREAKER.md)。
