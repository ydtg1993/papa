# Papa 爬虫核心配置手册

> 面向：配置 `configs/config.yaml` 时，每个字段是什么意思、怎么填。文件和 Go 声明改动需重启；运行治理可在 OA 保存后即时生效，见 [RUNTIME_CONTROL.md](../runtime/RUNTIME_CONTROL.md)。
> 配套：本文是「配置字典」；各治理队列与后台行为见 [ERROR_QUEUE.md](../runtime/ERROR_QUEUE.md)、[RECOVER_QUEUE.md](../runtime/RECOVER_QUEUE.md)、[REPEAT_QUEUE.md](../runtime/REPEAT_QUEUE.md)、[MONITOR.md](../operations/MONITOR.md)。框架已移除通用 cron 调度器。

---

## 0. 配置文件与加载

- 主配置：`configs/config.yaml`（`papa new` 生成，含中文注释）。
- 加载路径回退链：`papa.WithConfigPath(...)` 指定的路径 → 环境变量 `PAPA_CONFIG` → `configs/config.yaml`。
- **文件与运行覆盖分开**：改 `config.yaml` 或 Go 站点声明后需要重启；`runtime` 支持 OA 持久覆盖，保存后限制新工作并跨重启恢复。三个治理队列仍是全局启动配置，不能按站点覆盖；站点/阶段拓扑、物理 worker/队列容量和凭据来源不热改。任务自己的 repeat 周期仍可通过后台或 API 动态调整。
  运行覆盖保存在数据库中，不读写 `configs/runtime.yaml`。

公共浏览器的 `pool_size`、`direct_pool_size`、`max_idle_time` 与完整 breaker 策略均由数据库独占，修改通过版本化 API 即时发布；没有 YAML/Go fallback。迁移及初始化见 [EXECUTION_POLICY.md](../runtime/EXECUTION_POLICY.md)。

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
> 2. **一族「开着才必填」**：`html.max_body_size` —— 关掉对应开关时它们不参与校验（不许一个用不着的东西把人卡在启动上）。
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
| `stop_timeout` | duration | `5s` | 优雅退出时 worker 与动态 ticker 共用从 Stop 入口开始的一份等待预算，不各等一个完整 timeout；不保证任意阻塞锁或外部调用的硬实时上限。排空失败时保留数据库；最终退出仍调用浏览器池 `Shutdown(ctx)`，另给一份该时长的清理预算，取消创建并回收借出、预留和空闲实例。清理超时明确记录剩余状态，不依赖 leakless 兜底。认领或终态写入未确认的任务也计入未完成，即使其 worker 已因取消退出，也不会报告已排空。与 `server.shutdown_timeout` 不是一回事：那个等的是在途 HTTP 请求（如日志打包下载） |
| `stage_idle_timeout` | duration | `0`（不回收） | 阶段工作池连续空闲多久就回收（回收 = Stop 并撤出活池表，下次投递按需重建）。`0`/负数 = 常驻不回收。多站 / 长跑建议配，如 `10m` |
| `trace.mode` | string | `errors` | 同时控制步骤与原始快照：off=关闭；errors=保留 Fail/Warn、实际失败或 panic 的异常尝试；all=每次尝试。关闭时 `task.Trace` 为 nil，方法调用安全 no-op。详见下方 Trace 一节 |
| `trace.retention` | duration | `168h` | 步骤记录保留期（后台按批清理）；填**负数**表示永久保留、不自动清理 |

熔断完整策略已移入数据库 `execution_policy.breaker`，不再有 `crawler.breaker` YAML 键或 Go 站点覆盖。首次启动前必须显式初始化；全局默认及完整站点策略均在「系统设置 → 熔断与浏览器」调整，见 [EXECUTION_POLICY.md](../runtime/EXECUTION_POLICY.md)。`SiteSpec.StartPaused` 仍是独立启动暂停声明。

### browser —— 浏览器池（Rod）

稳定身份档案不存放在 `config.yaml`；通过「浏览器诊断」页或公开 API 创建版本、绑定主体并复验生效，见 [BROWSER_IDENTITY.md](../accounts/BROWSER_IDENTITY.md)。没有绑定/显式 Profile 时保留旧行为。启用身份后，不允许 Device、UA/language 启动参数或普通请求头与身份字段冲突；二进制与启用开关按本节管理；公共池容量和空闲回收时长仅从数据库执行策略读取。

> **默认不用它**：能用静态客户端拿到的页面一律用 `html`（`engine.FetchHTML`），rod 只在
> 静态 HTML 里拿不到目标数据时才上（判据与理由见 FETCHER_WRITING_GUIDE.md §3）。
> 打开 `enable` 不等于"该用它"—— 它只是把这条路允许起来。

| 键 | 类型 | 说明 |
| --- | --- | --- |
| `enable` | bool | 是否启用浏览器池 |
| `headless` | bool | 无头模式 |
| `no_sandbox` | bool | 关闭 Chromium sandbox |
| `leakless` | bool | leakless 进程守护（Windows 上其 exe 易被杀软误报，谨慎开启） |
| `binary` | string | 浏览器二进制从哪来：`rod`（默认）= **Rod 管理、缺了自动下载**的 Chromium，服务器上不用手动装；`system` = 系统已安装的 Chrome / Chromium / Edge（建池时探测，找不到就启动报错，不下载）；`path` = 用 `browser_path` 指定。留空 = 看 `browser_path`：给了就按 `path`，没给就按 `rod`。**`rod` / `system` 时不能再给 `browser_path`**（来源冲突，配置加载即报错） |
| `temp_dir` | string | 临时用户数据根目录，空值沿用 Rod 默认系统临时路径。Linux 可设 `/dev/shm/crawler`；创建并检查可写，不可用时报错，不回退。每实例独立子目录，由实例关闭流程清理；配置根目录始终保留，可供多个池或诊断实例共享，显式配置不代表独占所有权。池关闭不替代宿主对在途使用的排空，借出实例归还时再清理。不能与 `flags.user-data-dir` / `flags.disk-cache-dir` 同配；修改后重启。Chromium 子进程 TMPDIR 同步使用该根目录。 |
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
| `headers` | map | 额外请求头，同上：**通常写在站点级 `Headers`**，可被 `papa.WithHeaders` 覆盖（站点级写空值 = 删）。下载器须由调用方通过 `OptionsFromRequest(ctx, referer)` 显式传递，不自动继承抓取头。留空时静态抓取的 UA 是框架默认的 `PapaStaticHTML/1.0` |

### proxy —— 代理管理器

| 键 | 类型 | 说明 |
| --- | --- | --- |
| `api_url` | string | 代理服务 API 地址 |
| `refresh_interval` | duration | 代理列表刷新间隔 |

> 这一节配的是**代理池**（`api_url` 拉列表 + 定时刷新）。健康策略另见 `runtime.proxy_health`。
> 自行选择时使用 `engine.GetProxy().NextChecked(ctx)` 并处理耗尽错误；不提供吞掉代理耗尽错误的字符串入口。
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

YAML 时长写字符串；运行 API JSON 的时长使用整数纳秒。策略持久化到 `crawler_config.runtime_policy`，revision 以表列为权威；读取失败不回退更宽松默认。只读页面不触发账号验证或代理探测。流程、请求覆盖与浏览器边界见 [RUNTIME_CONTROL.md](../runtime/RUNTIME_CONTROL.md)。

账号健康政策、使用预算与备用预验证不是 `runtime` YAML 的另一份覆盖：前者按账号保存到管理状态，后者按池保存，可由 OA 动态调整。默认缓冲 10 秒、两次间隔确认、未知结果退避；使用额度与自动恢复默认关闭。正式字段、默认值及跨池共享语义见 [ACCOUNT_RUNTIME.md](../accounts/ACCOUNT_RUNTIME.md)。账号接入需由业务显式注册 Provider 和外部密钥支持的 Secrets 存储，不在 YAML 或任务 Meta 中保存明文密码，见 [ACCOUNT_ONBOARDING.md](../accounts/ACCOUNT_ONBOARDING.md)。

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
> 框架负责事件流（写入 + 契约）与报告引擎（`papa report generate`，产物 4 个文件）；使用姿势、报告怎么读、业务怎么扩展见 [OBSERVABILITY.md](../operations/OBSERVABILITY.md)。

| 键 | 类型 | 说明 |
| --- | --- | --- |
| `events.enabled` | bool | 是否启用在线信号聚合与事件归档，默认 `false`；离线 CLI 可独立执行 |
| `events.file` | string | 文件名（相对 `log.dir`），默认 `events.jsonl`；轮转沿用 `log.max_size` / `max_days` / `max_backups` / `compress` / `local_time` |
| `events.buffer` | int | 归档异步队列长度，默认及上限 8192；满则丢弃。未知 Emitter 由固定消费者隔离，执行路径不等待其回调 |
| `events.snapshot_interval` | duration | `stage_snapshot`（阶段/队列周期快照）的间隔，如 `30s`；`0` / 不写 = 不发 |
| `report.on_exit` | bool | 尽力生成退出信号快照，默认 `false`；需 events.enabled。与归档合计最多等待 2 秒，超时不影响业务资源关闭条件 |
| `report.interval` | duration | 在线快照周期，例如 `30m`；`0` / 不写关闭定时生成。需 events.enabled，与 on_exit 独立；只写当前本地日，合并待处理请求 |
| `report.mode` | string | 仅离线分析模式：auto（默认）= snapshot，acceptance 显式验收；CLI --snapshot / --acceptance 互斥且优先。在线始终 snapshot，不启用验收；非法值仍拒绝配置 |
| `report.dir` | string | 报告产物目录，默认 `./reports`。产物按**本地日历日**拆分：`<dir>/<2006-01-02>/`（一份 = 一天的日报；`--run` 时 `<dir>/<run_id>/`），四份 `report.md` / `report.json` / `ai-brief.md` / `ai-brief.json`；后台「obs报告」页也读它 |
| `report.retention` | duration | 默认 168h（7 天），0 同默认，负数永久保留。在线持有目录游标，每轮最多检查 128 个目录项并在下轮续扫，只删过期日期目录的四份已知产物，保留无关文件；离线无筛选 CLI 按 report.Prune 清理有 report.md 的过期目录，显式 since/until/run 不清理 |
| `report.timeout` | duration | 离线生成的协作超时，默认 `120s`；不能强杀任意分析回调。在线报告与事件归档退出等待固定共享最长 2 秒，不使用此值扩展期限 |
| `report.thresholds.stuck_after` | duration | 仅离线分析：判「卡住」的阈值（processing 超过它未更新），默认 `10m`。**报告口径**，不是启动恢复判据 |
| `report.thresholds.crawl_fail_rate_max` | float | 仅离线分析：抓取终态失败率上限，默认 `0.05`（未校准） |
| `report.thresholds.event_max_gap` | duration | 仅离线分析：事件时间断层阈值，默认 `10m`：事件流里相邻事件间隔超过它即算一次断层（丢事件 / 被硬杀）。**已用于 `event_coverage` 检查**：超过阈值、或窗口起点后超过阈值才有首条事件，该检查判未通过（warn），并向 `degraded` 写「事件存在时间断层，事件口径可能不完整」。**例外**：断层若正好是一段 `run_stopped` → `run_started` 的停机区间（起止秒级吻合），视为引擎没在跑而不是丢事件 —— 检查判通过、message 写「引擎停机 …，不是事件丢失」，且不写 degraded。`snapshot` 模式下引擎暂停/空闲时该检查标不适用。`0` / 不写 = 默认，负数启动报错。吞吐/ETA 的时长另按各 run 首末可见事件区间合并计算（含暂停），不按整日减停机；无正的事件跨度不判零吞吐异常 |

**事件契约（v1，只增不改）**：一行一个 JSON 对象，字段 `v`（schema 版本）、`seq`（本 run 内单调递增）、`ts`、`run_id`、`type`，以及按类型可选的 `site` / `stage` / `task_id` / `attempt` / `duration_ms` / `queue` / `counts` / `backlog` / `attrs` / `stages` / `queues` 等。其中 `attrs` 是**业务自定义键值对**（框架不解释语义），业务事件需要携带框架不认识的维度时放这里。读者必须忽略未知键；`seq` 出现缺口 = 丢弃或丢失。

事件类型（常量见 `pkg/events/event.go`）：

| 类别 | 事件 |
| --- | --- |
| 运行 | `run_started` / `run_stopped` |
| 任务 | `task_claimed` / `task_finished` / `task_failed` / `task_retry_scheduled` |
| 队列 | `queue_round` / `task_requeued` / `backlog_sampled` |
| 运行时 | `stage_snapshot` / `stage_pool` / `breaker_tripped` / `pause_resumed` |

业务侧可 `app.Events().Emit(events.Event{...})` 复用同一个 sink（同一 `run_id` / `seq` 序列）；业务事件类型由业务自己约定，框架只负责承载与落盘。

业务扩展通过业务自己的离线命令调用 `obs.Generate` 并提供 Sections/Patterns；App 的 UseReportSection / UseReportPattern 已移除。在线报告不接收 DB/业务回调，不扫描日志或 JSONL，只输出收到的信号和完整性说明；固定 snapshot / unknown，不把计数当权威任务总量。四份报告产物格式保留。

**已知限制（这一版）**：`queue_wait_ms` 不发（需要在任务上记录提交时刻）；`queue_round` 只带本轮处理量，不带每轮失败数。

### server —— 监控 HTTP 服务（详见 [MONITOR.md](../operations/MONITOR.md)）

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

### error_queue —— 失败任务错误队列（详见 [ERROR_QUEUE.md](../runtime/ERROR_QUEUE.md)）

三条治理队列始终运行，参数为全局配置，修改后重启生效。

```yaml
error_queue:
  worker_count: 1             # 0 = 默认 1
  interval: "4h"              # 0 / 不写 = 默认 4h
  max_retry: 3                # 自动重试次数上限；0 = 不限
  batch_size: 100             # 0 = 默认 1000，不能为负
```

`max_retry` 使用独立 `auto_retry_count`；`reprocess` 记录自动和人工再处理次数。NoRetry 持久禁止自动重试；人工重投保留预算和禁令直到成功。历史 failed 的禁令未知时阻止自动重试；升级须显式迁移，普通启动不迁移。

### Trace —— 步骤与原始快照统一保留

```yaml
crawler:
  trace:
    mode: errors          # off / errors / all
    retention: "168h"     # 默认 7 天；负数不自动清理
  trace_files:
    dir: "./traces"
    max_file_bytes: 4194304
    max_total_bytes: 268435456
    max_files: 10000
```

`trace.mode` 同时控制步骤与页面/响应快照：`off` 两者都关闭；`errors` 保留有 `Fail`、`Warn`、实际执行失败或 panic 的异常尝试；`all` 保留每次尝试。任务最终成功也可能有异常尝试。没有独立快照开关、目录或保留期。

普通 `FetchHTML`、渲染/捕获入口已有的 HTML，以及非账号 `FetchHTTP`、`FetchIncremental`、`CaptureResponse` 获取的文本 HTML/JSON 响应体，会登记在当前尝试。快照保存抓取入口得到的原始字节，未经 goquery 重新序列化；不重新请求页面，也不下载或镜像 CSS、图片等外部资源。非账号 `CaptureRequest` 在 Trace 开启时自动启用 `IncludeHTML`，成功时读取当前 DOM，取证不重新导航。浏览器快照是当时获得的 HTML，并不保证还原完整离线页面。账号认证 HTTP/HTML 路径继续排除原始快照。

Trace 新记录按 `traces/<site>/task-<id>/<execution-hash>-<sequence>.json` 分站点保存（根目录由 `trace_files.dir` 指定），文件名使用执行标识的 SHA-256 和 20 位递增序号；任务身份仍保留在记录元数据中；内部缺少站点的记录归入 `_unknown`。只支持当前目录及 v2 记录；旧目录、旧记录和旧快照令牌不再支持，使用前需清空旧 trace 目录。站点键必须是 1–128 位 ASCII 字母、数字、点、下划线或连字符，不允许路径分隔符、尾随点、`.`、`..` 或 Windows 保留设备名。

快照以内嵌数据保存在同一有界 Trace 记录内，后台通过 Trace API 下载原始内容，不生成独立 HTML 或 JSON 归档文件。`trace_files.max_file_bytes` 限制整个序列化记录，包含快照及其编码开销；超限时丢弃快照并标记截断，优先保留步骤。记录共同受 `max_total_bytes`、`max_files` 与 `trace.retention` 管理；写盘失败或容量拒绝会告警，不改变任务结算。

**迁移**：删除旧 `crawler.archive` 整段；旧 `trace.enabled` 改用 `trace.mode`。这些旧键会拒绝启动并给出迁移提示，没有兼容分支。旧归档文件不读取、不迁移、不删除，需由使用者自行管理。

### recover_queue —— 启动恢复（详见 [RECOVER_QUEUE.md](../runtime/RECOVER_QUEUE.md)）

三条治理队列始终运行，参数为全局配置，修改后重启生效。

```yaml
recover_queue:
  batch_size: 100             # 0 = 默认 1000，不能为负
```

启动时按进程边界重置 processing 孤儿，运行期事件唤醒、按阶段空位持续补充 pending。有界 channel 提供背压；不再使用溢出表或定时 drain。仅保留 batch_size，旧 enabled / worker_count / batch_pause 配置会拒绝启动。

### repeat_queue —— 周期轮询队列（详见 [REPEAT_QUEUE.md](../runtime/REPEAT_QUEUE.md)）

三条治理队列始终运行，参数为全局配置，修改后重启生效。

```yaml
repeat_queue:
  worker_count: 1             # 0 = 默认 1
  interval: "12h"              # 0 / 不写 = 默认 12h
  batch_size: 100             # 0 = 默认 1000，不能为负
```

只轮询到期的 success 任务，强制轮询也不处理 failed。任务上的 repeatable / repeat_interval 控制是否轮询及周期；repeat_interval 为 0 使用全局周期。旧 enabled 配置会拒绝启动。

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
当前模型不包含 `crawler_sites`；v4 使用独立数据库，不要求删除旧版数据库中的表或数据。

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

**v4 使用独立数据库，不提供旧版任务表原地升级**：启动和迁移先校验已有任务身份结构与数据，拒绝旧格式。校验通过后，`papa migrate` 才以 `AutoMigrate` 创建/补齐当前模型；没有历史专项回填、删旧索引或旧列转换。AutoMigrate 仍可能调整其支持的类型和约束，不能代替业务的数据迁移脚本。

框架自带的表：`crawler_tasks`、`crawler_breaker_incidents`、`crawler_breaker_history`、`crawler_access_token`、`crawler_config`、`crawler_config_operations`、`crawler_runtime_policy_history`、`crawler_browser_profiles`、`crawler_browser_identity_bindings`、`crawler_account_pools`、`crawler_account_management`、`crawler_account_profiles` 总是建；
`crawler_operation_log`（`server.operation_log`）、`crawler_task_flows`（`task_flow.enabled`）
跟着开关走 —— **先开开关再跑一次 `papa migrate`**，否则对应表不会建出来。

当前模型包括 `crawler_config.revision`、`crawler_tasks.account_pool`、任务失败类别、补偿条件/时间/原因/CAS 版本、最近账号与选择版本、最近 8 次认领关联 JSON。账号池表保存成员、当前选择和有界切换记录；管理表保存健康政策、核验进度、预算与有界事件，不保存 Cookie/Token。账号资料表只保存资料与加密存储引用，策略历史表保留最近 100 个覆盖版本。普通启动只检查结构并加载状态，不自动加表/列；即使默认未启用控制器，也应完成显式迁移。

**业务自己的表由项目自己的进程建**：那些模型是通过 `papa.WithModels`（脚手架里是 `internal/models`
包的 `Models()`）在业务进程里注册的，所以 `papa migrate` 在业务项目里会转交给它。
用脚手架就别去记两条命令 —— `papa migrate` 与 `make migrate` 等价。

> 启动时会检查"该有的表在不在"，缺了打醒目的错误日志 —— 免得出现「开关看着是开的、实际什么都没写进去」。

## 3. 时长格式

- 标准 Go 时长字符串：`"500ms"` / `"5s"` / `"10m"` / `"6h"`。
- Go `StageSpec.Delay` 额外支持区间：`"10s-30s"` 表示在该区间内随机取间隔；`runtime` 的阶段 `delay` 覆盖值只接受单一固定时长。OA 展示 Go 声明范围，清除覆盖恢复该范围。
- `runtime_policy` 等直接使用 Go `time.Duration` 的 HTTP JSON 字段为整数纳秒，OA 表单按秒换算；`execution_policy` 使用独立 Duration 类型，JSON 必须为 `"5s"` 这样的带单位字符串，不能传整数。以各 API 的类型契约为准。

### 阶段轮询次数上限

轮询上限属于业务站点声明，不是 YAML 配置项：在 `papa.StageSpec` 上设置 `RepeatLimit: 30`。0 表示不限；达到数据库 `repeat` 计数上限后，该站该阶段的任务不再被自动或强制轮询，恢复及错误重投不受此限，不关闭任务的 `repeatable`。已有 `repeat` 列继续使用，不需要迁移。完整计数含义见 [REPEAT_QUEUE.md](../runtime/REPEAT_QUEUE.md)。

## 动态调度开关

`task_flow.enabled` 默认 false；启用后显式 `papa migrate` 创建运行策略表。crawler_tasks 的四个调度列属于共同 v4 schema，关闭开关也包含它们，普通启动不迁移。用法、限制与动态切换见 [动态阶段调度](../runtime/TASK_FLOW.md)。


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

### v4 独立任务与恢复格式

v4 任务使用 ResourceID（留空时规范 URL）及 `(site, stage, identity_hash)` 唯一约束。创建入口统一生成 identity_type/value/hash；不再声明旧 URLHash、IdempotencyKey 或 IdempotencyHash。普通启动和显式 `papa migrate` 在创建 Engine/迁移写入前检查已有任务表的 v4 身份列、残留旧列/索引及数据；旧格式明确拒绝，不回填、不改写旧索引、不删除业务数据。使用独立 v4 数据库，不支持原地升级旧版本。 身份数据校验发现第一条非法记录即拒绝，不统计全部坏行；正常数据仍需扫描全表，启动耗时随数据量和存储性能增长。校验不缓存跨进程的通过结论。

熔断持久化使用当前控制版本 1 和精确窗口快照；缺失/不支持的版本阻止恢复，不从展示报告推导控制状态。正常 v4 重启恢复和追加历史保持有效；普通启动不迁移。

### Trace 文件存储

Trace 不再使用数据库；常规迁移与启动就绪检查不再包含 `crawler_task_trace`。旧表不删除，也不读取或自动导出；旧 SQL 历史不出现在当前后台。页面/响应快照内嵌在 Trace 文件中，下载接口按快照标识读取原始字节。

`crawler.trace.mode` 可选 `off`（关闭）、`errors`（默认，仅保留失败、panic、显式 Fail/Warn）、`all`（所有尝试，含成功）。错误模式下干净成功不落盘，同轮失败后成功重试仍保留失败尝试。正常关停取消本身不触发异常保留。全量模式也保存正常取消的尝试；没有步骤的尝试补一个完成标记。后台展示最新**有保留证据**的执行，不代表当前任务状态。

| `crawler.trace_files` 配置 | 默认 | 作用 |
| --- | --- | --- |
| `dir` | `./traces` | Trace 专用目录，单个 Store 拥有，不与其它产物共用 |
| `max_file_bytes` | 4194304 | 整个尝试记录（含快照与编码开销）的字节上限；超限丢弃快照并标记截断，优先保留步骤 |
| `max_total_bytes` | 268435456 | 文件总字节预算 |
| `max_files` | 10000 | 文件数量预算 |

预算 0 使用默认值，负数非法；达到容量拒绝新证据并告警，不改任务结算。`crawler.trace.retention` 仍默认 168h，负数关闭过期清理。按记录保存时间清理，不以读取时间续期。

手动追踪 API 已删除，不维护手动选择状态，也不提供主动追踪按钮。需要完整记录时设置 `crawler.trace.mode: all`。

`ListTraceExecution` 是唯一追踪查询入口，读取最新已发布执行的末 500 步；详细接口首步 `truncated=true` 表示存在截断。写入以每次尝试为单位暂存、同步、关闭并重命名发布；执行目录汇聚各尝试，不将多个执行混合。没有记录不等于任务未执行。容量和故障边界见 [RESOURCE_BUDGETS.md](RESOURCE_BUDGETS.md)。

单实例调度、在线信号、离线报告输入和下载预算的装配 API、有限默认值及超限处理见 [资源预算与背压](RESOURCE_BUDGETS.md)。这些预算与站点/阶段的业务并发配置分开，通过 Go 装配接口设置，不是 YAML 键。

#### Trace 模式配置

模式在启动时确定，修改后重启生效。省略 mode 默认 errors；Go 零值同样默认 errors。关闭用 `config.TraceConfig{Mode: config.TraceOff}`（`config` 为 `github.com/ydtg1993/papa/v4/config`）。旧 enabled 键已删除，和无效模式一样在启动校验时拒绝，不再进行映射。

## Handler 与下载退避

阶段重试仍通过 Go 声明 `RetrySpec` 配置；下载仍用 Config/Overlay 的 `RetryInterval` 和 `MaxRetries`。Handler 指数退避值上限 60 秒、下载上限 10 秒，均在安全封顶后执行 Equal Jitter（半值至全值）；不增加 `retry.max_backoff` 或 `retry.jitter` YAML 键。首次/末次尝试、取消与计数约定见 [Fetcher 手册](../guides/FETCHER_WRITING_GUIDE.md)。本地退避不缩短服务端 Retry-After。

配置迁移：删除 `crawler.queue_watermark`、`crawler.drain_interval` 以及三队列的 `enabled`。删除 `recover_queue.worker_count`、`recover_queue.batch_pause`。所有已删除键均拒绝启动，不会静默忽略。
