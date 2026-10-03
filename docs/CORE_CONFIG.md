# Papa 爬虫核心配置手册

> 面向：配置 `configs/config.yaml` 时，每个字段是什么意思、怎么填、哪些能热更。
> 配套：本文是「配置字典」；各模块的行为语义见 [SCHEDULER.md](./SCHEDULER.md)、[ERROR_QUEUE.md](./ERROR_QUEUE.md)、[RECOVER_QUEUE.md](./RECOVER_QUEUE.md)、[MONITOR.md](./MONITOR.md)。

---

## 0. 配置文件与加载

- 主配置：`configs/config.yaml`（`papa new` 生成，含中文注释）。
- 加载路径回退链：`papa.WithConfigPath(...)` 指定的路径 → 环境变量 `PAPA_CONFIG` → `configs/config.yaml`。
- 运行期覆盖：`configs/runtime.yaml`（与 config.yaml 同目录）。OA 后台改配置只写内存，**关停时**把被改字段落盘到这里，重启后叠加生效；运行期**不碰 config.yaml**。

## 1. 完整配置项参考

### app —— 环境

| 键 | 类型 | 说明 |
| --- | --- | --- |
| `env` | string | `dev`（自动迁移表结构）/ `prod` |

### crawler —— 爬虫核心

| 键 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `target` | string | — | 起始目标站域（main.go 里拼起始 URL 用） |
| `dedup_cache_size` | int | `0` | 内存去重表最大条目数；`0`=不限（旧行为），`>0` 用 LRU 限界，淘汰条目由 DB 唯一索引兜底 |
| `queue_watermark` | float | `0.75` | 队列高水位比例（0-1），达到后新任务溢出到 DB 待回灌，避免满队列丢任务 |
| `drain_interval` | duration | `2s` | 溢出任务回灌队列的间隔 |
| `trace.enabled` | bool | `false` | 单任务步骤追踪：开启后 handler 可用 `task.Trace.Step/Fail` 上报步骤，写入 `crawler_task_trace` 表。关闭时 `task.Trace` 为 `nil`，调用是安全 no-op |
| `trace.retention` | duration | `168h` | 步骤记录保留期（后台按批清理）；填**负数**表示永久保留、不自动清理 |
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
| `max_body_size` | int64 | ✅ | 响应体大小上限（字节） |
| `headers` | map | ✅ | 额外请求头 |

### proxy —— 代理管理器

| 键 | 类型 | 说明 |
| --- | --- | --- |
| `api_url` | string | 代理服务 API 地址 |
| `refresh_interval` | duration | 代理列表刷新间隔 |

### db —— 数据库

| 键 | 类型 | 说明 |
| --- | --- | --- |
| `driver` | string | `mysql` 等 |
| `dsn` | string | 数据源名称 |
| `max_idle_conns` / `max_open_conns` | int | 空闲/最大连接数 |
| `conn_max_lifetime` / `conn_max_idle_time` | duration | 连接最大生命周期 / 空闲最大存活 |

### log —— 日志

| 键 | 类型 | 说明 |
| --- | --- | --- |
| `dir` | string | 日志目录 |
| `max_size` | int | 单文件大小上限（MB） |
| `max_days` / `max_backups` | int | 保留天数 / 备份数 |
| `compress` / `local_time` | bool | 是否压缩 / 本地时间 |

### server —— 监控 HTTP 服务（详见 [MONITOR.md](./MONITOR.md)）

| 键 | 类型 | 说明 |
| --- | --- | --- |
| `enabled` | bool | 是否开启 HTTP 服务 |
| `port` | int | 监听端口 |
| `monitor` | bool | 是否挂载监控页/API |
| `whitelist` / `whitelist_file` | []string | 来源 IP/CIDR 白名单；文件优先 |
| `monitor_dirs` | map | 监控页展示的业务目录占用 `name: path` |
| `queue_sample_interval` | duration | 三个治理队列「待处理」积压数的采样间隔，默认 `1m`。监控页刷新只读内存快照，仅采样时查库；调大可降低 DB 压力 |
| （无密钥字段） | — | 后台凭据是 `crawler_access_token` 表里的多条**访问令牌**（每条属于一个操作人），不再用配置里的单密钥；用 `papa token add --operator <名字>` 创建 |
| `operation_log` | bool | 操作日志开关，默认 `false`。开启后后台所有增删改操作写入 `crawler_operation_log` 表（含失败，并记下**操作人**——来自访问令牌），侧边栏 General 分组多出一项「操作日志」（在「访问令牌」上方，只读表格页）；关闭时不建表、不写库，菜单项也不出现 |

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

### recover_queue —— 中断恢复队列（详见 [RECOVER_QUEUE.md](./RECOVER_QUEUE.md)）

| 键 | 类型 | 说明 |
| --- | --- | --- |
| `enabled` | bool | 是否启用；启用后启动即恢复一次 |
| `worker_count` | int | 并发恢复数 |
| `interval` | duration | 自动轮询间隔；`0`=仅启动时+手动 |
| `timeout` | duration | 任务卡住多久算卡死 |
| `batch_size` | int | 每批查询处理的任务数；`0`=默认 1000（分页流式） |

### repeat_queue —— 周期轮询队列（详见 [REPEAT_QUEUE.md](./REPEAT_QUEUE.md)）

| 键 | 类型 | 说明 |
| --- | --- | --- |
| `enabled` | bool | 是否启用周期轮询 repeatable 任务 |
| `worker_count` | int | 并发重新投递 repeatable 任务的数量 |
| `interval` | duration | 轮询间隔；`0`=不自动轮询，仅手动触发 |
| `batch_size` | int | 每批查询处理的任务数；`0`=默认 1000（分页流式） |

## 2. 时长格式

- 标准 Go 时长字符串：`"500ms"` / `"5s"` / `"10m"` / `"6h"`。
- 阶段 `delay` 额外支持区间：`"10s-30s"` 表示在该区间内随机取一个间隔（反爬更隐蔽）。

## 3. 运行期热更（OA 后台）

- 可热更字段（`PUT /api/config`，改后即时生效）：
  - 浏览器/HTML：`browser.max_idle_time` / `headers`，`html.timeout` / `max_body_size` / `headers`。
  - 三个队列（`error_queue` / `recover_queue` / `repeat_queue`）的**全部字段**：`enabled` / `interval` / `worker_count` / `batch_size`，外加 `error_queue.max_retry`、`recover_queue.timeout`。
- 需重启字段：`browser.enable` / `headless` / `no_sandbox` / `leakless` / `browser_path` / `pool_size` / `direct_pool_size`、`proxy.*`、`crawler.stages.*`、`crawler.dedup_cache_size`、`crawler.queue_watermark`、`crawler.drain_interval`。
- 持久化：热更只写内存；关停时把「被改字段」写成 `configs/runtime.yaml` 覆盖层，下次启动叠加回 `config.yaml`。
