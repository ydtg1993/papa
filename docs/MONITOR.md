# Papa 监控服务后台手册

> 面向：看懂并用好 OA 监控后台——Dashboard、设置、数据浏览、动态配置、队列手动触发、日志导出。
> 配置项见 [CORE_CONFIG.md](./CORE_CONFIG.md) 的 `server` 段。

---

## 0. 一句话

框架内置一个 HTTP 监控后台（`server.enabled` + `server.monitor`），访问 `http://localhost:<port>/monitor`，提供 Dashboard、任务队列概览、队列治理、数据浏览、设置、动态配置与日志导出。

## 1. 鉴权（密钥 + 白名单）

所有 `/api/` 接口都过两道校验：

1. **IP/CIDR 白名单**（`server.whitelist` / `whitelist_file`），空 = 不限制。
2. **访问密钥**（`server.auth_key` / `auth_key_file`），空 = 不校验。请求带 `Authorization: Bearer <key>`，或 `X-Auth-Key: <key>`，或 `?key=<key>`。

`papa new` 已自动生成 `configs/secret`（密钥）和 `configs/whitelist`（白名单文件，每行一个 IP/CIDR，`#` 注释）。

## 2. 设置 API

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/settings` | 返回白名单、密钥/白名单文件路径与是否存在、日志目录 |
| POST | `/api/settings/whitelist` | 更新白名单并持久化到 `whitelist_file`；body `{"whitelist": ["127.0.0.1","10.0.0.0/8"]}` |
| POST | `/api/settings/secret` | 重新生成密钥并写回 `auth_key_file`；返回 `{"key":"<新密钥>"}` |
| POST | `/api/settings/shutdown` | 触发优雅退出 |

## 3. 数据浏览 API（通用 model 浏览）

把业务 model 登记进 OA（代码层 `papa.WithBrowsableModels`），即可在「数据浏览」模块分页/搜索/筛选/排序查看：

```go
app, err := papa.New(
    papa.WithBrowsableModels(
        papa.ModelDef{Key: "episode", Label: "剧集", Model: &models.Episode{}},
    ),
)
```

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/data/models` | 列出可浏览模型及列元数据 |
| GET | `/api/data/:model` | 分页列表；query：`page`、`size`（默认 20 上限 200）、`search`、`sort`（`col`/`-col`）、`filter[col]=val` |

示例：`/api/data/episode?page=1&size=20&search=火影&sort=-id&filter[downloaded]=true`

- `ModelDef.Key`：URL 安全标识（字母/数字/下划线/连字符），别用中文或空格。
- 列能力由 GORM schema 自动判定：字符串列可 LIKE 搜索，数字/bool/时间可排序+等值筛选，JSON 只读。
- `WithBrowsableModels` 的模型在 `app.env: dev` 时自动迁移建表，无需再写 `WithModels`。
- 只读，无写端点；列名走白名单校验，非法列被忽略。

## 4. 动态配置 API

运行期热更爬虫参数（`browser.pool_size` / `max_idle_time` / `headers`，`html.timeout` / `max_body_size` / `headers` 等）：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/config` | 返回当前覆盖层 + 热更字段 / 需重启字段清单 |
| PUT | `/api/config` | 应用运行期配置；body 只收热更字段，需重启字段返回 400 |

热更只写内存，关停时落盘到 `configs/runtime.yaml`，重启后叠加生效（详见 [CORE_CONFIG.md](./CORE_CONFIG.md)）。

## 5. 队列治理

「队列治理」模块展示 `error_queue` / `recover_queue` / `repeat_queue` 三个后台治理队列的运行情况，并可逐个手动触发：

| 列 | 含义 |
| --- | --- |
| 状态 | `运行中`（附带本轮已执行时长）/ `空闲` / `已停用`（`*.enabled=false`）；有上次执行错误时鼠标悬停可见 |
| 上次执行 | 上次执行完成距今多久（悬停看绝对时间）· 上次执行耗时 |
| 处理量 | 空闲时=上次处理数，运行中=本轮已处理数；副行显示累计处理数 |
| 待处理 | 当前排队待处理的任务数（低频采样，副行显示采样时间） |
| 操作 | 立即执行一次该队列 |

手动触发的 HTTP 接口（等价于页面上的「立即执行」）：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/api/errorqueue/process` | 手动触发失败任务重投；返回 `{"status":"ok","processed":N}` |
| POST | `/api/recoverqueue/process` | 手动触发卡死任务恢复；返回 `{"status":"ok","recovered":N}` |
| POST | `/api/repeatqueue/process` | 手动触发周期轮询（重投已完成的 repeatable 任务）；返回 `{"status":"ok","repolled":N}` |

数据来源分两类，均**不实时**，且不占用监控页刷新路径：

- 运行状态/耗时/处理量：引擎内存计数，随队列执行即时更新。
- 待处理积压：`COUNT` 查询，由后台按 `server.queue_sample_interval`（默认 `1m`）低频采样；每轮队列执行结束也会立即补采一次。采样频率越低，DB 压力越小。
- 采样仅在 `server.monitor=true` 时启动。

对应的 `/api/monitor` 响应字段：

```jsonc
"queues": {
  "error_queue": {
    "name": "error_queue", "enabled": true, "running": false, "runs": 12,
    "started_at": "...", "last_finish_at": "...", "last_duration": 4000000000,
    "last_processed": 37, "run_processed": 0, "total_processed": 421,
    "backlog": 128, "backlog_at": "...", "last_error": ""
  },
  "recover_queue": { /* ... */ },
  "repeat_queue":  { /* ... */ }
}
```

`last_duration` / `started_at` 等时间字段：`duration` 为纳秒，时间为 RFC3339。

## 6. 日志导出

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/logs` | 列出日志目录文件 |
| GET | `/api/logs/download` | 下载日志；`?file=name` 下载单个，缺省打包全部为 zip |

## 7. 业务自定义数据展示

fetcher 里调 `engine.RecordMetric("key", value)`，监控页「自定义数据」模块实时展示（配合 `engine.GetMetrics()` 读快照）。

`GetMetrics()` 除了业务自定义数据，还会附带**框架级队列治理计数**（同样展示在「自定义数据」模块）：

| 键 | 含义 |
| --- | --- |
| `queue_spilled` | 累计溢出任务数（队列达高水位被回灌 DB 的次数） |
| `queue_spill_backlog` | 当前待回灌的溢出任务数（>0 说明队列持续满） |
| `recover_total` | 累计恢复任务数（recover_queue） |
| `error_retry_total` | 累计失败重投任务数（error_queue） |
| `repeat_repoll_total` | 累计周期轮询重投任务数（repeat_queue） |

> 这三个累计值同时也是「队列治理」模块「处理量」的累计数，同一份引擎内存计数，不会重复统计。
