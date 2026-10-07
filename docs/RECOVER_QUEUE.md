# Papa 启动恢复手册

> 面向：进程崩溃 / 重启后，怎么把停在 `pending` / `processing` 的任务捡回来继续跑。
> 配套：[ERROR_QUEUE.md](./ERROR_QUEUE.md)（失败任务重试，两者按状态分工）。

---

## 0. 一句话

进程在抓取中途挂了，会留下一批停在 `pending`（已入库未入队）或 `processing`（取走了但没处理完）的任务。
**这些任务不用「判」，看一眼就知道**：进程刚起，没有任何 worker 在跑，此刻所有 `processing` 都是孤儿。
所以恢复只做一件事 —— **启动时把「未到终态」的任务全部重新入队**。

它**不是**一个队列：没有周期、没有积压、没有后台按钮。跑一次就结束。

## 1. 配置

```yaml
recover_queue:
  enabled: true       # 启用后启动时恢复一次（默认 true）
  worker_count: 2     # 并发重新入队的数量
  batch_size: 1000    # 每批查询处理的任务数；0 = 默认 1000（分页流式）
```

配置里**没有** `interval` 和 `timeout`，理由见下节。

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

判据就是 `status IN (pending, processing)`，**没有时间条件**。

- `pending` 也要捞：它们可能是上次高水位溢出到 DB 的（溢出列表在内存里，随进程一起没了），
  也可能入库了但没来得及入队 —— 不捞就永远躺在「待处理」。
- 重投走正常提交路径（`SubmitTask`），`Repeatable` / `Urgent` 照抄行上的值，不会在恢复时丢掉这两个属性。
- **重复投递是安全的**：worker 认领走的是条件更新（`pending → processing`），两份里只有一份能认领成功，
  另一份拿到 0 行直接跳过。
- 重新投递失败（阶段没注册、提交出错）时，这一行会被标成 `failed` 并把原因追加进 `error` 列
  （`crawler.Engine.markRequeueFailed`）。

## 4. 触发方式

只有一种：**启动时**（`enabled: true`，异步跑，不阻塞启动）。

代码里 `engine.ProcessRecoverQueue()` 是导出的，业务可以自己调 —— 但**别在运行期调**：
那时的 `processing` 是正在跑的任务，重投它们就是让同一行被两个 worker 同时写。
（这既是它当年按超时猜卡死时踩的坑，也是后台那个「恢复队列」手动按钮被删掉的原因。）

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
见 `crawler/batch.go` 的 `processInBatches` 与回归测试 `TestProcessInBatchesUsesKeysetCursor`。
