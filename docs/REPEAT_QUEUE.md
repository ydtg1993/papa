# Papa 周期轮询队列手册

> 面向：想让标记了 `Repeatable: true` 的任务（如 catalog 目录页）被周期性地重新抓取、发现新内容。
> 配套：[SCHEDULER.md](./SCHEDULER.md)（业务自定义 cron）、[RECOVER_QUEUE.md](./RECOVER_QUEUE.md)（启动恢复）。

---

## 0. 一句话

框架内置一个「周期轮询队列」`repeat_queue`：把**到点的**（`next_repeat_at <= NOW()`，或还没排过期的）、**已完成**（success/failed）的 repeatable 任务重新投递回各自阶段，实现「周期重跑轮询任务」。周期是**每条任务自己的**（`repeat_interval`，0 = 跟全局）；全局 `interval` 只是"最粗兜底"。走的是与 error_queue 同构的分页 + 并发队列（分页用 keyset 游标，见 [RECOVER_QUEUE.md](./RECOVER_QUEUE.md) 第 6 节）。

## 1. 配置

```yaml
repeat_queue:
  enabled: true       # 是否启用周期轮询 repeatable 任务（总开关：false 时任务级周期也不生效）
  worker_count: 2     # 并发重新投递 repeatable 任务的数量
  interval: "10m"     # 最粗兜底的扫描间隔（每条任务可用 repeat_interval 定更细的周期）；0 = 不自动轮询，仅手动触发
  batch_size: 1000    # 每批查询处理的任务数；0 = 默认 1000（分页流式）
```

> `interval` 的语义是"**最多隔这么久扫一次**"：ticker 会自动提前到"最早一条到点"（见第 3 节），所以任务自己定的 10 分钟就真是 10 分钟，不会因为全局写着 2h 而被拖到 2h。它同时是"新提交/被改过的行最多等多久被发现"的上界。

## 2. 触发方式

| 方式 | 说明 |
| --- | --- |
| 自动轮询 | `enabled: true` 且 `interval` 非 0，后台按"到点"投递（节拍自动跟随） |
| 手动（OA） | `POST /api/repeatqueue/process` —— **也只扫到点的**，想强制某条立刻重跑用任务表的「重投」 |
| 手动（代码） | `engine.RepollRepeatableTasks()`（同上，只看到点） |

## 3. 哪条任务、多久一次：都能运行期改

**要不要轮询**：`repeatable` 是**任务行上的列**，粒度是任务、不是阶段 —— 同一个阶段里可以有的是轮询任务（分类页），有的是一次性任务（详情页）。它原先只有"提交那一刻"一个入口（`papa.Task{Repeatable: true}`，首次落库时写进这一列；已存在的行再提交不会更新它）。

**多久一次**：`repeat_interval`（秒；**0 = 跟全局** `interval`）同样是任务行上的列，只在首次入库时由 `papa.Task{RepeatInterval: 10 * time.Minute}` 播种；之后用下面两条路改（提交不再覆盖它 —— 否则每次启动重投入口任务都会把运营改的周期冲掉）。

| 做什么 | 后台任务表 | 代码 |
| --- | --- | --- |
| 开 / 停轮询 | 行内动作「开轮询」/「停轮询」 | `engine.SetTaskRepeatable(id, on)` |
| 改周期 | 行内动作「设轮询周期」（秒，0 = 跟全局） | `engine.SetTaskRepeatInterval(id, wasSeconds, seconds)` |

队列判断只看两列：`next_repeat_at`（下次到点，判据）与 `last_repeat_at`（上次轮询时刻，只作记录，后台看得见）。语义如下 —— 升级到这套列需要先跑一次 `papa migrate`（加三列 + 一个索引，见 CHANGELOG）：

- **到点才重投**：重投那一刻同一条语句写 `status = 待处理`、`last_repeat_at = NOW()`、`next_repeat_at = NOW() + 有效周期`。有效周期 = `repeat_interval`，为 0 就用全局 `interval`。
- **周期从"上一轮投递"算起**：任务跑得比自己的周期还久时，等价于"一跑完就又投"（下一轮扫描正好接上）。
- **只改标记，不顺手重投**：开/停/改周期都在**下一轮扫描**时才生效（开轮询会把排期置为"现在"，所以它就是下一轮）。想让某条**立刻**再跑一次，用「重投」。
- **重复点得 409**：`repeatable` 这一列、以及行快照里的旧周期值，本身就是版本守卫；已经开着再开、已经停着再停、周期刚被别人改过，都会被拒并说明原因。
- **停轮询不影响正在跑的那一轮**：它跑完这次就没有下一次。停在"这一轮已经捞到这行"之后也算数 —— 重投前的重置是带条件的（`repeatable = 1 AND status IN (已完成)`），影响 0 行就跳过。
- **周期最小 10 秒**（`repeatMinTick`）：比它细会在写入侧被直接拒掉（返回"周期不合法"），不会静默按 10 秒跑。
- **排期与时间比较都用库里的 `NOW()`**：写入端与判据端同一个时钟，不受应用/数据库时钟偏差影响。

> **`repeat_queue.enabled=false` 或 `interval=0` 时，任务级周期也不生效** —— 总开关关掉就是"仅手动触发"，没有自动扫描这回事。这条最容易被误解，配 `interval` 时留意。
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


## 5. 与业务 cron 的分工

| 能力 | 归属 | 调度方式 |
| --- | --- | --- |
| 周期轮询 repeatable（推荐） | 本文 `repeat_queue` | 到点才投（节拍自动跟随最早到点） |
| 业务自定义定时任务 / 指定时刻轮询 | [SCHEDULER.md](./SCHEDULER.md) 的 `RegisterCronJob` + `RepollRepeatableTasks` | cron（6 段秒级） |

> 如果轮询需要「每天 08:00 整点」这种 cron 语义，用 `RegisterCronJob`；如果只要「每隔 N 分钟」，用 `repeat_queue`。
