# Papa 监控服务后台手册

> 面向：看懂并用好 OA 监控后台——Dashboard、设置、表格页、动态配置、队列手动触发、日志导出。
> 配置项见 [CORE_CONFIG.md](./CORE_CONFIG.md) 的 `server` 段。

---

## 0. 一句话

框架内置一个 HTTP 监控后台（`server.enabled` + `server.monitor`），访问 `http://localhost:<port>/monitor`，提供 Dashboard、任务队列概览、队列治理、表格页、设置、动态配置与日志导出。

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

## 3. 表格页（oao 组件）

后台的表格页由独立组件 [github.com/ydtg1993/oao](https://github.com/ydtg1993/oao) 渲染。
**组件是纯展示层，不碰数据层**：业务声明"显示什么、怎么显示"，并实现 `oao.Source` 提供数据。

```go
app, err := papa.New(papa.WithModels(&models.Episode{}))

// 在 New 之后、Run 之前注册（这样能用 app.DB 构造 Source）
app.UseTables(oao.Table{
    Key: "episode", Label: "剧集", Group: "业务",
    Source:  myEpisodeSource,          // 业务实现，负责查库/调接口
    Columns: []oao.Column{
        {Field: "id", Kind: oao.KindNumber, Width: "70px"},
        {Field: "title", Label: "标题"},
        {Field: "downloaded", Label: "已下载", Kind: oao.KindBool},
        {Field: "cover", Label: "封面", Render: oao.RenderImage, Size: 56},
        {Field: "remark", Label: "备注", Render: oao.RenderInput, MaxLen: 30},
    },
    Filters: []oao.Filter{
        {Field: "title", Label: "标题", Op: oao.OpLike},
        {Field: "downloaded", Label: "已下载", Kind: oao.KindBool, Op: oao.OpIn},
        {Field: "created_at", Label: "创建时间", Kind: oao.KindTime, Op: oao.OpBetween},
    },
    DefaultSort: "-id",
})
```

**操作列**（可选）：声明 `Actions` 才有，不声明就是只读表、不注册任何写路由。动作请求转发给业务的 `Handler`，
业务用 `oao.Fail` 返回带状态码的提示语（其他错误统一 500，细节不回前端）：

```go
Actions: []oao.Action{
    {Key: "retry", Label: "重投", Tone: oao.ToneInfo,
     Confirm: "重置该任务并重新投递？",          // 非空 → 先弹确认框
     Handler: func(ctx context.Context, req oao.ActionRequest) error {
         id, err := strconv.ParseUint(req.ID, 10, 64)   // 主键是字符串，取 Table.IDField 指定的字段
         if err != nil {
             return oao.Fail(http.StatusBadRequest, "无效的 ID")
         }
         // 防重复提交：把行快照里的版本值写进 UPDATE 的条件，并检查影响行数。
         // req.Row 是客户端回传的、不可信的展示快照，只适合当"版本号"用。
         if err := engine.RetryTask(uint(id), wasReprocess(req)); err != nil {
             return oao.Fail(http.StatusConflict, "%v", err)
         }
         return nil
     }},
    {Key: "fail", Label: "标失败", Tone: oao.ToneWarn, Confirm: "确认？",
     Form: []oao.Field{{Name: "reason", Label: "失败原因", Widget: oao.WidgetTextarea, Required: true}}},
    oao.EditAction(...),    // 表单字段由列声明推导（跳过图片/JSON/链接与 NoEdit 列）
    oao.RemoveAction(...),  // 自带二次确认
}
```

两条容易踩的约定：

- **主键列必须声明**：前端靠 `Table.IDField`（默认 `id`）指定的字段定位目标行，而列表下发时
  只保留**声明过的列**——没声明这一列，点按钮只会弹「这一行没有 id 字段」。不想显示就用
  `Hidden: true`（仍会下发）。`oao.New` 在注册时就会校验，声明不一致直接报错。
- **防并发/重复提交是业务的事**：组件只透传，不做去重（见 oao README 的「组件的职责边界」）。
  正确做法是把前置条件写进**自己的写语句**并检查影响行数，别用"先查再写"（TOCTOU 等于没锁）。

动作声明得多了也不会撑行：**超过 3 个时前两个平铺、其余自动收进「更多 ▾」**（面板支持键盘与点外部关闭）。

内置「任务」表（`internal/tasksource`）就带三个动作：**重投**、**标失败**（必填原因）、**删除**——
它同时是动作如何接线的参考实现：三个动作都走 `crawler.Engine` 的条件更新，
`url` 列用 `NewTab: true` 在新标签页打开。成功与失败都会调 `Config.OnAction`，papa 用它写操作日志
（见 [CORE_CONFIG.md](./CORE_CONFIG.md) 的 `server.operation_log`，不开就不记）。

数据源只需实现一个方法：

```go
type Source interface {
    List(ctx context.Context, q oao.Query) (rows []map[string]any, total int64, err error)
}
```

`Query` 是组件把 HTTP 参数规范化后的结果（`Page/Size/Search/Sort/Filter`），
筛选算子（`In` 用 `"a,b,c"`、`Between` 用 `"a..b"`）由业务自己解释 —— 组件只透传。
papa 的 `internal/gormsource` 把算子落成 SQL：`OpLike` → `LIKE '%值%'`（前后通配，用不上索引），
`OpPrefix` → `LIKE '值%'`（只有后通配，能走索引），`OpEq`/`OpIn`/`OpBetween`/`OpGt`/`OpLt` 各对应同名条件。
参考实现看 `internal/tasksource`（内置「任务」表）或 hg2 的 `tablesource`。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `{prefix}/tables` | 列出已注册的表格及列/筛选/操作元数据（侧边栏菜单用） |
| GET | `{prefix}/{table}` | 分页数据；query：`page`、`size`（默认取表声明，上限 200）、`search`、`sort`（`col`/`-col`）、`filter[col]=val` |
| POST | `{prefix}/{table}/action/{key}` | 转发操作给业务 Handler（表未声明该动作一律 404） |

`prefix` 默认 `/api/oao`；静态资源挂在 `/static/oao/`。两者都必须过监控后台的白名单 + 密钥校验（组件复用宿主注入的中间件）。

- **列怎么显示**：`Render` 可选 `text`（省略号 + 悬停看全文）/ `input`（只读输入框，
  长度可控、横向滚动看全，**不是编辑**）/ `link` / `image` / `enum`（彩色标签）/ `time` / `json` / `custom`。
  留空按 `Kind` 推断。
- **哪些能筛**：只声明需要的字段，未声明的字段不可筛；列名与排序同样走白名单，杜绝注入。
- **菜单自动出现**：按 `Group` 分组渲染到侧边栏，注册即出现，前端无需改代码。
- **刷新策略**：表格页只在用户操作（筛选/排序/翻页）时刷新，不参与 Dashboard 的 3 秒轮询。

内置的「任务」表（`internal/tasksource`）展示的就是 `crawler_task`，可作为完整示例。

## 4. 自定义页与注入（逃生舱）

声明式的表格页覆盖不到的场景（自定义看板、审核页、特殊展示），可以往后台加**自定义页**，
或直接注入受信任的 JS/CSS。四个 API 都在 `Run` 之前调用：

```go
// 一页 = 侧边栏一个菜单项 + 一段渲染脚本
app.UsePage(papa.Page{
    Key: "review", Label: "审核", Group: "业务",   // Key 只允许字母数字下划线连字符
    Script: `
        Papa.page("review", function (el, meta) {
            // el 是页面容器；meta 是清单项 {key,label,group}
            el.innerHTML = '<h2>' + esc(meta.label) + '</h2>';
            apiFetch('/api/my/review-list').then(r => r.json()).then(function (d) {
                el.innerHTML += '<p>共 ' + d.total + ' 条待审</p>';
            });
        });`,
})

app.UseScript(`window.__myFlag = true;`)      // 全局注入一段 JS
app.UseScriptFile("assets/admin.js")          // 读文件注入（读不到直接 panic）
app.UseCSS(`.my-badge { border-color: var(--primary); }`)
app.UseCSSFile("assets/admin.css")
```

前端契约：页脚本在加载后调 `Papa.page(key, fn)` 注册渲染函数，点菜单时框架调用 `fn(el, meta)`。
页面脚本里可以直接用后台的全局件：`Toast` / `Dialog` / `skeletonRows` / `esc` / `tipAttr` /
`apiFetch` / `apiPost`（都带密钥）。没注册渲染函数的页会显示一句明确提示，不会白屏。

| 方法 | 路径 | 鉴权 | 说明 |
| --- | --- | --- | --- |
| GET | `/api/pages` | 白名单 + 密钥 | 自定义页清单（侧边栏菜单用），`{"pages":[{key,label,group}]}` |
| GET | `/static/custom.js` | **免鉴权** | `UseScript` / `UsePage` 的脚本拼在一起 |
| GET | `/static/custom.css` | **免鉴权** | `UseCSS` 的样式（在 mo.css / oao.css 之后加载，可覆盖） |

> **注入内容按"非机密"对待**：浏览器加载 `<script src>` / `<link>` 时带不了自定义请求头，
> 所以这两个端点走的是和 `oao.js` / `mo.js` 一样的免鉴权静态路由 —— **别把密钥、令牌之类的敏感值写进注入的 JS/CSS**。
> 数据接口仍然全部在白名单 + 密钥后面（清单 `/api/pages` 就是要密钥的）。
>
> 声明有问题（Key 非法 / 重复 / Script 为空 / 注入文件读不到）会在启动时 **panic**，不会等到点了菜单才发现。

## 5. 动态配置 API

运行期热更爬虫参数（`browser.max_idle_time` / `headers`，`html.timeout` / `max_body_size` / `headers` 等；浏览器池大小 `pool_size` / `direct_pool_size` 需重启）：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/config` | 返回当前覆盖层 + 热更字段 / 需重启字段清单 |
| PUT | `/api/config` | 应用运行期配置；body 只收热更字段，需重启字段返回 400 |

热更只写内存，关停时落盘到 `configs/runtime.yaml`，重启后叠加生效（详见 [CORE_CONFIG.md](./CORE_CONFIG.md)）。

## 6. 队列治理

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

## 7. 日志导出

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/logs` | 列出日志目录文件 |
| GET | `/api/logs/download` | 下载日志；`?file=name` 下载单个，缺省打包全部为 zip |

## 8. 业务自定义数据展示

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
