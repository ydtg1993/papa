# Papa 周期轮询队列手册

> 面向：想让标记了 `Repeatable: true` 的任务（如 catalog 目录页）被周期性地重新抓取、发现新内容。
> 配套：[SCHEDULER.md](./SCHEDULER.md)（业务自定义 cron）、[RECOVER_QUEUE.md](./RECOVER_QUEUE.md)（启动恢复）。

---

## 0. 一句话

框架内置「周期轮询队列」：把**到点的**（`next_repeat_at <= NOW()`，或还没排过期的）、**已完成**（success/failed）的 repeatable 任务重新投递回各自阶段，实现「周期重跑轮询任务」。周期是**每条任务自己的**（`repeat_interval`，0 = 用本站声明的 `Interval`）；站点声明里的 `Interval` 只是"最粗兜底"。走的是与 error_queue 同构的分页 + 并发队列（分页用 keyset 游标，见 [RECOVER_QUEUE.md](./RECOVER_QUEUE.md) 第 6 节）。

队列**按站点拆**：每个站点一条（后台一行一个站，见第 5 节），各自的锁/计数/运行快照/手动入口互不影响。

## 1. 配置

**不在 config.yaml 里**：三个治理队列都按站点写在 `configs/sites/<站名>.go` 的站点声明上
（yaml 里再写 `repeat_queue:` 会被「未知配置键」拒掉，并指路到这里）。**每条站点声明都得写全**：

```go
	// configs/sites/<站名>.go
	on := true // Go 没有字面量取址

	RepeatQueue: &papa.RepeatQueueSpec{
		Enabled:     &on,   // 必写；&off = 本站没有这条队列（其余字段可以不写）
		WorkerCount: 2,     // 并发重新投递 repeatable 任务的数量，> 0
		Interval:    "10m", // 最粗兜底的扫描间隔，> 0（每条任务可用 repeat_interval 定更细的周期）
		BatchSize:   100,   // 每批查询处理的任务数，> 0（分页流式）
	},
```

> `Interval` 的语义是"**最多隔这么久扫一次**"：ticker 会自动提前到"最早一条到点"（见第 3 节），
> 所以任务自己定的 10 分钟就真是 10 分钟，不会因为站点写着 2h 而被拖到 2h。它同时是"新提交/被改过的行
> 最多等多久被发现"的上界。
>
> **"不自动跑"有两层，是「与」关系**（见第 5 节）：声明层的 `RepeatQueue.Enabled`（有没有这条队列）
> 与运营层的 `AutoRepeat`（要不要自动，库为事实、后台可开停）。

## 2. 触发方式

| 方式 | 说明 |
| --- | --- |
| 自动轮询 | `enabled: true` 且 `interval` 非 0，后台按"到点"投递（**每站一条队列**，节拍各自跟随本站最早到点） |
| 手动（OA） | 队列治理面板上**每站一行**的「立即执行」；或 `POST /api/repeatqueue/process?site=<站点>`（**只扫本站到点的**） |
| 手动（代码） | `engine.RepollSiteRepeatableTasks(site)`（只看到点）；`engine.RepollRepeatableTasks()`（所有站点各跑一遍，向后兼容） |
| 手动（忽略周期） | `engine.ForceRepollSiteRepeatableTasks(site)`：把本站**所有可轮询的已完成任务**都投一遍（后台每个站的「轮询任务」总按钮走它，随「站点」页一起上） |

> `POST /api/repeatqueue/process` **不带 `?site=`** = 所有站点各跑一遍（向后兼容业务 cron 的旧用法；站点多时耗时是 Σ 各站）。站点名不认识 → **400**。

## 3. 哪条任务、多久一次：都能运行期改

**要不要轮询**：`repeatable` 是**任务行上的列**，粒度是任务、不是阶段 —— 同一个阶段里可以有的是轮询任务（分类页），有的是一次性任务（详情页）。它原先只有"提交那一刻"一个入口（`papa.Task{Repeatable: true}`，首次落库时写进这一列；已存在的行再提交不会更新它）。

**多久一次**：`repeat_interval`（秒；**0 = 用本站声明的** `Interval`）同样是任务行上的列，只在首次入库时由 `papa.Task{RepeatInterval: 10 * time.Minute}` 播种；之后用下面两条路改（提交不再覆盖它 —— 否则每次启动重投入口任务都会把运营改的周期冲掉）。

| 做什么 | 后台任务表 | 代码 |
| --- | --- | --- |
| 开 / 停轮询 | 行内动作「开轮询」/「停轮询」 | `engine.SetTaskRepeatable(id, on)` |
| 改周期 | 行内动作「设轮询周期」（秒，0 = 用本站声明的周期） | `engine.SetTaskRepeatInterval(id, wasSeconds, seconds)` |

队列判断只看两列：`next_repeat_at`（下次到点，判据）与 `last_repeat_at`（上次轮询时刻，只作记录，后台看得见）。语义如下 —— 升级到这套列需要先跑一次 `papa migrate`（加三列 + 一个索引，见 CHANGELOG）：

- **到点才重投**：重投那一刻同一条语句写 `status = 待处理`、`last_repeat_at = NOW()`、`next_repeat_at = NOW() + 有效周期`。有效周期 = `repeat_interval`，为 0 就用全局 `interval`。
- **周期从"上一轮投递"算起**：任务跑得比自己的周期还久时，等价于"一跑完就又投"（下一轮扫描正好接上）。
- **只改标记，不顺手重投**：开/停/改周期都在**下一轮扫描**时才生效（开轮询会把排期置为"现在"，所以它就是下一轮）。想让某条**立刻**再跑一次，用「重投」。
- **重复点得 409**：`repeatable` 这一列、以及行快照里的旧周期值，本身就是版本守卫；已经开着再开、已经停着再停、周期刚被别人改过，都会被拒并说明原因。
- **停轮询不影响正在跑的那一轮**：它跑完这次就没有下一次。停在"这一轮已经捞到这行"之后也算数 —— 重投前的重置是带条件的（`repeatable = 1 AND status IN (已完成)`），影响 0 行就跳过。
- **周期最小 10 秒**（`repeatMinTick`）：比它细会在写入侧被直接拒掉（返回"周期不合法"），不会静默按 10 秒跑。
- **排期与时间比较都用库里的 `NOW()`**：写入端与判据端同一个时钟，不受应用/数据库时钟偏差影响。

> **`RepeatQueue.Enabled: &false`（这一站没有轮询队列）或 `AutoRepeat: &false`（运营层关掉自动）时，任务级周期也不生效** —— 关掉就是"仅手动触发"，没有自动扫描这回事。这条最容易被误解，配 `Interval` 时留意。
>
> **停轮询 ≠ 放开去重**：行还在（`success`/`failed`），同一个 `stage|url` 再提交仍会被 `SubmitTask` 的去重命中分支静默吞掉（所有已完成任务都这样，与轮询无关）。想立刻再跑只能「重投」。
>
> **失败的任务走 error_queue 的节奏**：本队列只按任务周期捞 `success/failed`，而 `failed` 同时也在 [ERROR_QUEUE.md](./ERROR_QUEUE.md) 的集合里 —— 那条路有自己的 `interval` / `max_retry`，不受任务周期约束。想让"失败也别猛打目标站"，调的是 error_queue。

## 4. 语义：只重投「到点的、已完成的」

`repeat_queue` 只重投 `status = success / failed`、且 `next_repeat_at IS NULL OR next_repeat_at <= NOW()` 的 repeatable 任务，**不碰还在 pending/processing 的**——后者由 [RECOVER_QUEUE.md](./RECOVER_QUEUE.md) 在**启动时**兜底。这样避免「上一轮还没处理完，这一轮又入队」的双入队；到点条件则保证每条任务按自己的周期走。

这条判据是纯比较、走 `idx_repeat_due`（`repeatable, status, next_repeat_at`）：**周期换算只发生在写路径**（重投那一条 UPDATE 算 `now + 周期`），读路径一行算术都没有 —— 函数谓词（`TIMESTAMPDIFF(...) >= 周期` 那类）会让索引失效，而且 SQL 里的算术没法在没有真库的测试里验。
重新投递失败（队列满以外的提交错误）时，这一行会被标成 `failed` 并把原因追加进 `error` 列
（`engine.Engine.markRequeueFailed`）。早先这里只记日志、行留在 `pending` —— 而本队列只捞 `failed`，
于是这条任务再也没人管：运营看着是"排队中"，实际永远不会执行。


## 5. 站点维度：每站一条队列 + 站点级开关

轮询队列**按站点拆开**：后台「队列治理」里每个站点一行，队列名是 `repeat_queue:<站点 Key>`；
默认 scope（任务的 `site` 为空串）那条仍是历史名字 `repeat_queue`，它**在后台永远占一行**，但**不跑** ——
未归属的任务没有站点声明可挂（`crawler_tasks.site` 是 v3.1 才有的列，更早入库的行是空串；
要给它们治理，得先把它们归到某个站点键上）。

本站"要不要**自动**轮询"是**运营层**的闸门：站点声明上的 `AutoRepeat`（它是新站点的**播种值**，
之后**库为事实** —— `crawler_sites.auto_repeat`，后台「站点」页能随时开停、重启按库走）：

```go
off := false
site := papa.SiteSpec{
    Key:        "huangguo",
    AutoRepeat: &off,   // 不写 = 自动（与框架一直以来的行为一致）；这里显式关掉
    // …
}
```

- **`false` = 本站不自动轮询**：那一行在后台显示「已停用」，但**两个手动入口照旧可用** ——
  面板上的「立即执行」（只投到点的）与 `engine.ForceRepollSiteRepeatableTasks`（忽略周期、全投一遍）。
- 它与声明层的 `RepeatQueue.Enabled`（这一站**有没有**这条队列）是**与**关系：声明里就没这条队列，
  `AutoRepeat` 也就无从谈起。
- 默认 scope（未归属）**不跑**：它没有站点声明可挂，`AutoRepeat` 恒为自动那一层也就没意义。
- 判定读的是**内存里的声明快照**（监控页每次刷新都会问"这队列开没开"，所以不查库）；ticker 照常走，
  只是到了那一跳先看闸门 —— 这样"状态读不到"不会把整站静默停成永久不轮询。

### 统计落到站点表

每站轮询跑完一轮，会把该站的统计按列写回 `crawler_sites`（`last_repeat_at` / `repeat_total` /
`repeat_backlog` / `last_repeat_error`），同时更新内存快照（监控页 3 秒一刷读的是内存，不查库）。
后台顶部那排**站点 Tab** 与站点概要就是读它；熔断暂停/恢复也会把 `breaker_paused` 写进去。
`base_url` / `auto_repeat` / `stage_count` 是启动时按声明抄的 —— **改声明要重启，直接改库不生效**。

## 6. 与业务 cron 的分工

| 能力 | 归属 | 调度方式 |
| --- | --- | --- |
| 周期轮询 repeatable（推荐） | 本文 `repeat_queue` | 到点才投（节拍自动跟随最早到点） |
| 业务自定义定时任务 / 指定时刻轮询 | [SCHEDULER.md](./SCHEDULER.md) 的 `RegisterCronJob` + `RepollRepeatableTasks` | cron（6 段秒级） |

> 如果轮询需要「每天 08:00 整点」这种 cron 语义，用 `RegisterCronJob`；如果只要「每隔 N 分钟」，用 `repeat_queue`。
