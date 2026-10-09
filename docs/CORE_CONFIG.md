# Papa 爬虫核心配置手册

> 面向：配置 `configs/config.yaml` 时，每个字段是什么意思、怎么填、哪些能热更。
> 配套：本文是「配置字典」；各模块的行为语义见 [SCHEDULER.md](./SCHEDULER.md)、[ERROR_QUEUE.md](./ERROR_QUEUE.md)、[RECOVER_QUEUE.md](./RECOVER_QUEUE.md)、[MONITOR.md](./MONITOR.md)。

---

## 0. 配置文件与加载

- 主配置：`configs/config.yaml`（`papa new` 生成，含中文注释）。
- 加载路径回退链：`papa.WithConfigPath(...)` 指定的路径 → 环境变量 `PAPA_CONFIG` → `configs/config.yaml`。
- 运行期覆盖：`configs/runtime.yaml`（与 config.yaml 同目录）。OA 后台改配置只写内存，**关停时**把被改字段落盘到这里，重启后叠加生效；运行期**不碰 config.yaml**。
- `PUT /api/config` 收的是**增量**：只改 body 里提到的字段，没提到的保持原样（映射给 `{}` 表示清空那组覆盖）。详见 [MONITOR.md](./MONITOR.md) 第 6 节。

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

| 键 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `dedup_cache_size` | int | `0` | 内存去重表最大条目数；`0`=不限（旧行为），`>0` 用 LRU 限界，淘汰条目由 DB 唯一索引兜底 |
| `queue_watermark` | float | `0.75` | 队列高水位比例（0-1），达到后新任务溢出到 DB 待回灌，避免满队列丢任务 |
| `drain_interval` | duration | `2s` | 溢出任务回灌队列的间隔 |
| `stop_timeout` | duration | `5s` | 优雅退出时等各阶段 worker 把队列跑完的上限（各阶段**并发**等，总等待约一个该值）。超时则打错误日志，并**跳过**关库与关浏览器池 —— 此时 worker goroutine 还活着，关了只会让在途写入全部失败。与 `server.shutdown_timeout` 不是一回事：那个等的是在途 HTTP 请求（如日志打包下载） |
| `trace.enabled` | bool | `false` | 单任务步骤追踪：开启后 handler 可用 `task.Trace.Step/Fail` 上报步骤，写入 `crawler_task_trace` 表。关闭时 `task.Trace` 为 `nil`，调用是安全 no-op |
| `trace.retention` | duration | `168h` | 步骤记录保留期（后台按批清理）；填**负数**表示永久保留、不自动清理 |
| `breaker.enabled` | bool | `false` | 熔断闸门总开关。开启后窗口内**终态失败**数达阈值就把**所有阶段**的 worker 一起闸住 |
| `breaker.window` | duration | `5m` | 统计窗口。窗口是滑动切片（60 个等宽桶），粒度 = `window/60` |
| `breaker.threshold` | int | — | 窗口内终态失败数达到它即暂停。**`enabled: true` 时必填且必须 > 0** —— 没写、写 `0` 或负数都会在**启动时 panic**（判据在 `breaker.New`，调用点是 `engine.NewEngine`）。它没有默认值：`threshold <= 0` 时「多少条才算熔断」没有答案，补一个数只会让人以为开着、数的却是另一回事 |
| `stages.<name>.worker_count` | int | — | 该阶段 worker 并发数 |
| `stages.<name>.queue_size` | int | — | 该阶段任务队列缓冲大小 |
| `stages.<name>.delay` | 时长/区间 | — | 任务间隔，固定 `"5m"` 或随机区间 `"10s-30s"` |
| `stages.<name>.retry.max_attempts` | int | `3` | 最大尝试次数（含首次） |
| `stages.<name>.retry.backoff` | duration | `1s` | 重试退避基数（指数递增） |

### browser —— 浏览器池（Rod）

| 键 | 类型 | 热更 | 说明 |
| --- | --- | --- | --- |
| `enable` | bool | ❌ | 是否启用浏览器池 |
| `pool_size` | int | ❌ | 代理浏览器**并发上限**（按需创建，非常驻实例数） |
| `direct_pool_size` | int | ❌ | 强制直连浏览器并发上限（不走代理） |
| `max_idle_time` | duration | ✅ | 空闲回收阈值，`0`=不回收 |
| `headless` | bool | ❌ | 无头模式 |
| `no_sandbox` | bool | ❌ | 关闭 Chromium sandbox |
| `leakless` | bool | ❌ | leakless 进程守护（Windows 上其 exe 易被杀软误报，谨慎开启） |
| `browser_path` | string | ❌ | Chrome 可执行文件路径，空则用默认 Chromium |
| `headers` | map | ✅ | 默认请求头（与内置默认头合并，同名覆盖） |

### html —— 静态 HTML 客户端

| 键 | 类型 | 热更 | 说明 |
| --- | --- | --- | --- |
| `enable` | bool | ❌ | 是否启用静态 HTML 客户端 |
| `timeout` | duration | ✅ | 请求超时 |
| `max_body_size` | int64 | ✅ | 响应体大小上限（字节）。**`enable: true` 时必填**，`102400..67108864`（100KB..64MB）。下界挡的是**单位混淆** —— 隔壁 `log.max_size` 的单位是 MB，这里写 `10` 意思是 10 字节，于是每次抓取都报「页面太大」；上界 64MB 在模板值（10MB）之上，只防笔误。热更时同样校验，越界回 400 |
| `headers` | map | ✅ | 额外请求头 |

### proxy —— 代理管理器

| 键 | 类型 | 说明 |
| --- | --- | --- |
| `api_url` | string | 代理服务 API 地址 |
| `refresh_interval` | duration | 代理列表刷新间隔 |

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
| （无密钥字段） | — | 后台凭据是 `crawler_access_token` 表里的多条**访问令牌**（每条属于一个操作人），不再用配置里的单密钥；用 `papa token add --operator <名字>` 创建 |
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

| 键 | 类型 | 说明 |
| --- | --- | --- |
| `enabled` | bool | 是否启用 |
| `worker_count` | int | 并发重新投递数 |
| `interval` | duration | 自动轮询间隔；`0`=仅手动 |
| `max_retry` | int | 单个任务最多再处理代数；`0`=不限 |
| `batch_size` | int | 每批查询处理的任务数；`0`=默认 1000（分页流式） |

### recover_queue —— 启动恢复（详见 [RECOVER_QUEUE.md](./RECOVER_QUEUE.md)）

| 键 | 类型 | 说明 |
| --- | --- | --- |
| `enabled` | bool | 是否启用；启用后**启动时**恢复一次 |
| `worker_count` | int | 并发重新入队的数量 |
| `batch_size` | int | 每批查询处理的任务数；`0`=默认 1000（分页流式） |

### repeat_queue —— 周期轮询队列（详见 [REPEAT_QUEUE.md](./REPEAT_QUEUE.md)）

| 键 | 类型 | 说明 |
| --- | --- | --- |
| `enabled` | bool | 是否启用周期轮询 repeatable 任务 |
| `worker_count` | int | 并发重新投递 repeatable 任务的数量 |
| `interval` | duration | 轮询间隔；`0`=不自动轮询，仅手动触发 |
| `batch_size` | int | 每批查询处理的任务数；`0`=默认 1000（分页流式） |

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
- **只作用于 `config.yaml`**：热更覆盖层（`runtime.yaml`）里写 `business` 不生效，改它要重启。这一段的键也**不会**出现在后台「配置」页上。

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

## 2. 时长格式

- 标准 Go 时长字符串：`"500ms"` / `"5s"` / `"10m"` / `"6h"`。
- 阶段 `delay` 额外支持区间：`"10s-30s"` 表示在该区间内随机取一个间隔（反爬更隐蔽）。

## 3. 运行期热更（OA 后台）

- 可热更字段（`PUT /api/config`，改后即时生效）：
  - 浏览器/HTML：`browser.max_idle_time` / `headers`，`html.timeout` / `max_body_size` / `headers`。
  - 两个队列（`error_queue` / `repeat_queue`）的**全部字段**：`enabled` / `interval` / `worker_count` / `batch_size`，外加 `error_queue.max_retry`；`recover_queue` 只剩 `enabled` / `worker_count` / `batch_size`（它只在启动跑一次，没有 `interval` / `timeout` 可调）。
> 热更的值**同样要过校验**（`config.ValidateRuntime`）—— 这条路绕过 `config.Load`，
> 不单独校验的话，一个 `html.max_body_size: 0` 能在不重启的情况下让每一次抓取都失败，
> 而配置文件的校验完全看不见它。越界回 **400**（不是 500：那是调用方的输入错），且不下发。

- 需重启字段：`browser.enable` / `headless` / `no_sandbox` / `leakless` / `browser_path` / `pool_size` / `direct_pool_size`、`proxy.*`、`crawler.stages.*`、`crawler.dedup_cache_size`、`crawler.queue_watermark`、`crawler.drain_interval`、`crawler.stop_timeout`、`crawler.trace.*`、`crawler.breaker.*`（`RuntimeConfig` 里没有 trace / breaker，改只能重启）。

> **熔断的「暂停」状态也不跨重启**：进程重启即恢复运行 —— 起进程本身就是一次人工介入。
> 所以没有任何"暂停"字段需要持久化，`configs/runtime.yaml` 里也不会出现它。
- 持久化：热更只写内存；关停时把「被改字段」写成 `configs/runtime.yaml` 覆盖层，下次启动叠加回 `config.yaml`。
