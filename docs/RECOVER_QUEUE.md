# Papa 启动恢复手册

> 面向：进程崩溃 / 重启后，怎么把停在 `pending` / `processing` 的任务捡回来继续跑。
> 配套：[ERROR_QUEUE.md](./ERROR_QUEUE.md)（失败任务重试，两者按状态分工）。

---

## 0. 一句话

进程在抓取中途挂了，会留下一批停在 `pending`（已入库未入队）或 `processing`（取走了但没处理完）的任务。
恢复只处理**本进程启动前遗留**的未终态任务：`status IN (pending, processing)` 且 `updated_at < 本进程启动时间`。这个启动边界不是“超过 N 小时就算卡死”的启发式，而是用来排除本进程启动后已经认领或更新过的任务。

它**不是**一个队列：没有周期、没有积压、没有后台按钮。跑一次就结束。

## 1. 配置

写在 `configs/config.yaml` 的全局段（**不再按站点拆**）：

```yaml
recover_queue:
  enabled: true               # 启动恢复；false / 不写 = 不跑
  worker_count: 2             # 并发重新入队的数量，开启时必填 > 0
  batch_size: 100             # 每批查询处理的任务数，开启时必填 > 0（分页流式）
```

启动时全局扫一次（不再逐站点）。这个类型上**没有 `interval` / `max_retry`** —— 它只在启动跑一次，
没有"多久跑一次"也没有"再处理代数"这回事，配置里**没有** `interval` 和 `timeout`，理由见下节。

## 2. 为什么不按超时判「卡死」

早先的判定是 `status IN (pending, processing) AND updated_at < now - timeout`（默认 6h），再配一个
`interval` 定时扫。这条启发式两头不讨好，已整个删掉：

| 毛病 | 说明 |
| --- | --- |
| **漏** | 崩溃前几分钟认领的任务，`updated_at` 不满足超时条件，**启动那一次根本捞不到它**；而 `interval: 0`（配置注释里明确允许「仅启动时 + 手动触发」）时连定时轮询都没有 —— 那批任务会一直挂在「处理中」 |
| **误伤** | 真跑得久的任务（下整部剧、大文件）过了 `timeout` 就被当成卡死重投，**与仍在跑的 worker 撞车写同一行 `content`** |

「意外中断」在启动这一刻是**确定**的，不需要靠时间推测。运行期真正的卡死应该靠给外部调用设超时解决
（`htmlfetch` 与 `rod` 都有自己的 timeout），不是靠事后扫库。

## 3. 判定与重投

判据是 `status IN (pending, processing) AND updated_at < engine.startedAt`。这里没有可配置的超时窗口；`startedAt` 只是本进程启动边界，用于避免异步恢复扫描误捞本次运行已经碰过的行。

- `pending` 也要捞：它们可能是上次高水位溢出到 DB 的（溢出列表在内存里，随进程一起没了），
  也可能入库了但没来得及入队 —— 不捞就永远躺在「待处理」。
- 重投走正常提交路径（`SubmitTask`），`Repeatable` / `Meta` 照抄行上的值，不会在恢复时丢掉这两个属性；
  `Urgent` **强制置真**（走加急快车道）—— 恢复的是上一轮没跑完的孤儿，插到队首先跑完，
  `claimTask` 认领时把 urgent 归零，所以它只是排队位置、跑过一次即变普通。
  其中 `Meta`（业务键，见 [FETCHER_WRITING_GUIDE.md](./FETCHER_WRITING_GUIDE.md) 1.2）是要害：
  它是 handler 认业务行的依据，恢复出来的任务要是没带上，任务会以「结构错误」永久失败 ——
  而这类失败通常不可重试，人工点「重投」也走同一条路。
- 恢复前还会以条件更新再次确认该行仍是 `pending` / `processing`；提交后的 worker 认领走 `pending → processing` 条件更新。即使出现重复排队，也只有一份能认领成功，另一份影响 0 行后跳过。
- 重新投递失败（阶段没注册、提交出错）时，这一行会被标成 `failed` 并把原因追加进 `error` 列
  （`engine.Engine.markRequeueFailed`）。

## 4. 触发方式

只有一种：**启动时**（`enabled: true`，异步跑，不阻塞启动）。

代码里 `engine.ProcessRecoverQueue()` 是导出的，但它只应由启动恢复流程调用。当前查询用 `updated_at < startedAt` 排除本次运行已经更新过的任务；运行期再次调用通常没有意义，而且会把“启动恢复”错误地变成业务调度接口。后台因此不提供恢复队列按钮或 HTTP 入口。

## 5. 与 error_queue 的分工

| 机制 | 目标状态 | 什么时候跑 |
| --- | --- | --- |
| `recover_queue` | `pending` / `processing` | **只在启动时**一次 |
| `error_queue` | `failed` | 按 `interval` 周期 + 手动触发 |

一个任务同一时刻只属于其中一种状态，两者不会重叠处理。

## 6. 分页游标

恢复是分页扫的（`batch_size`），游标是**自己的 `id`**（keyset），不是 OFFSET。

这是硬要求，不是优化：被恢复的行会从 `processing` 变成 `pending`，**依然满足**「未到终态」——
分页若靠「重新查同一批」，它会原地打转、永远跑不完（启动恢复再也回不来）。
见 `engine/batch.go` 的 `processInBatches` 与回归测试 `TestProcessInBatchesUsesKeysetCursor`。
