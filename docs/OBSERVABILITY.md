# Papa 运行观测手册（obs）

> 面向：想知道「这一轮跑得怎么样、哪里失败了、队列为什么堵」的人——不用翻库、不用逐行 grep 日志。
> 配置细节见 [CORE_CONFIG.md](./CORE_CONFIG.md) 的 `obs` 段与 `log.level`；命令见 [CLI.md](./CLI.md) 的 `papa report`。
> 业务项目想加自己的报表段/模式，看本文 §5、§6。

---

## 0. 一句话

框架自带三层运行数据：**指标层**（`logs/events.jsonl` 结构化事件）、**证据层**（人类日志 + 任务 trace + 失败页面归档）、
**状态层**（MySQL 任务表）。`papa report` 把这三层合成 4 个产物：人读的 `report.md`、机读的 `report.json`、
给 AI/接手人用的 `ai-brief.md` 与 `ai-brief.json`。

> **默认关闭**：`obs.events.enabled` 为 `false`，不为不用的项目付事件写入成本。打开即可出报告；
> 关着时报告只读「失败面 + 状态层」，并在 `degraded` 里明确标注数据缺失。

## 1. 三层数据与职责

| 层 | 产出 | 谁写 | 回答什么问题 | 默认 |
| --- | --- | --- | --- | --- |
| 指标层 | `logs/events.jsonl`（JSONL，一行一事件） | 引擎异步 emitter（`pkg/events`） | 成功多少、多快、卡在哪个阶段、失败怎么分组 | 关（`obs.events.enabled` 开） |
| 证据层 | `logs/*.log`、`crawler_task_trace`、`logs/fetcher-html/` 归档 | logrus / 引擎 trace / 归档器 | 失败那一刻的原文、`file:line` 样本、task 级轨迹 | 日志默认开；trace/归档按配置 |
| 状态层 | MySQL `crawler_tasks`（及业务表） | 引擎 / 业务 | 当前事实：谁 pending、谁 processing、终态分布 | 永远在 |

报告是**只读**的：只读事件文件、日志目录、数据库；不写业务表、不重新投递任务、不调用外部接收端。

## 2. 事件契约（v1）

一行一个 JSON 对象，字段分两组：**必填信封** + **可选载荷**。

必填信封：

| 字段 | 含义 |
| --- | --- |
| `v` | 事件契约版本，当前 `1`；不兼容变更才 +1 |
| `seq` | 本 run 内自增序号，从 1 开始；**缺口 = 丢事件**（报告会报 `event_gaps`） |
| `ts` | 发生时间（带时区，RFC3339Nano） |
| `run_id` | 本次进程运行的唯一 ID（`r-<启动毫秒>-<随机>`）；跨 run 报告用 `--run` 过滤 |
| `type` | 事件类型，见下表 |

可选载荷（按类型出现）：`site` / `stage` / `task_id` / `url` / `attempt` / `max_attempts` / `reprocess` /
`worker` / `kind` / `outcome` / `duration_ms` / `queue_wait_ms` / `queue` / `counts` / `backlog` /
`source` / `error` / `attrs` / `stages` / `queues`。

内置事件类型（14 个）：

| 类型 | 语义 |
| --- | --- |
| `run_started` / `run_stopped` | 进程运行边界（`run_stopped` 只在优雅停机时出现） |
| `task_claimed` | worker 认领任务（可算 `queue_wait_ms`） |
| `task_finished` / `task_failed` | 单次尝试的终局（含 `duration_ms`、`kind`、`error`） |
| `task_retry_scheduled` | 失败后安排重试（含 `attempt` / `max_attempts`） |
| `task_requeued` | 治理队列重投（`queue` = `error_queue` / `recover_queue` / `repeat_queue`） |
| `queue_round` | 治理队列一轮跑完（`counts.processed`、`duration_ms`） |
| `backlog_sampled` | 治理队列积压采样 |
| `stage_snapshot` / `stage_pool` | 阶段与工作池的周期快照（排队/在跑/累计/溢出） |
| `spill_changed` | 溢出表变化（`source=added` 新增；`source=drained` 一轮回灌的聚合） |
| `breaker_tripped` | 熔断触发 |
| `pause_resumed` | 暂停/恢复信号变化 |

约定：

- **框架不解释 `attrs` 的语义**：业务自定义事件（例如「预处理完成」「推送完成」）用自己定义的 `type` + `attrs`
  键值即可，papa 只保证字段能落盘、报告能读出计数与分组。
- 事件是**异步有界队列**：满了丢弃并计数（`events.dropped` 可见），**绝不阻塞 worker**。进程被硬杀时
  最后几条可能丢——`seq` 缺口能检测到。
- 事件文件按 logrus 的 lumberjack 轮转策略滚动（`*.gz` 可读），`Discover` 会按需读取归档。

## 3. 怎么开

```yaml
log:
  level: "info"          # debug / info / warn(warning) / error；默认 info
obs:
  events:
    enabled: true        # 打开事件流（默认 false）
    file: "events.jsonl" # 相对 log.dir
    buffer: 8192         # 异步队列容量；满则丢弃并计数
    snapshot_interval: "30s"  # 阶段/队列快照间隔；"0" = 不发
  report:
    mode: "auto"         # 判定语义：auto（默认）= 定时 snapshot、退出/CLI acceptance；可强制 snapshot / acceptance
    on_exit: false       # 进程优雅退出时自动生成报告（默认 false）
    interval: "0"        # 定时生成报告：如 "30m" = Run 期间每 30 分钟刷新一次当天日报；0/不写 = 关
    dir: "./reports"     # 产物根目录：<dir>/<2006-01-02>/（一份 = 一天的日报；--run 时 <dir>/<run_id>/）
    retention: "168h"    # 报告保留期：默认 7 天；负数 = 永久保留（0/不写同默认）；每次生成后清理
    timeout: "120s"      # 单次生成报告的超时（失败只记日志，不阻塞业务/退出）
    thresholds:          # 通用分析节（framework）的阈值
      stuck_after: "10m"
      crawl_fail_rate_max: 0.05
      event_max_gap: "10m"    # 事件时间断层阈值（相邻事件间隔超过它算断层；event_coverage 检查用）
```

> `event_max_gap` 已接入事件完整性检查（`event_coverage`）：相邻两条事件的时间差超过阈值，
> 或窗口起点之后超过阈值才有首条事件，该检查判**未通过**（warn），同时向 `degraded` 写一条
> 「事件存在时间断层，事件口径可能不完整」—— 断档期间的「0 失败」不能当结论。`0` / 不写 = 默认 `10m`。

**三种生成方式，互相独立**：

| 方式 | 配置 | 适用 |
| --- | --- | --- |
| 定时（推荐用于长跑） | `obs.report.interval: "30m"` | 进程常驻、想随时有最新的当天日报，不依赖人工命令或退出一刻 |
| 退出时 | `obs.report.on_exit: true` | 一次性任务 / 自动化作业，结束后要一份最终总结 |
| 命令行 | `papa report generate` | 事后补报告、指定窗口、CI 门禁（`--fail-on-verdict`） |

定时报告与退出报告都写**同一个当天目录**（后写覆盖前写 = 当天最新快照）；只开定时就足够，
不需要再保留 `on_exit`，也不需要定时跑命令。定时器在 `Run` 期间启动；停机时先停定时器、
再出退出报告，最后才关库。

`obs.report.on_exit` 的退出顺序（`internal/app`）：停 HTTP → 停引擎排空 → **停定时报告** → 写 `run_stopped` → **flush 事件** →
生成报告（**此时数据库还开着**）→ 关库/关浏览器。所以退出报告能读到完整的「本轮 + 状态层」。

报告产物**按本地日历日拆分**：一次生成会把事件窗口按本地时区切成若干天，有事件的每天各写一份
`<dir>/<2006-01-02>/`（四份产物；同一天重跑会刷新当天那份）。进程退出的自动报告同理 —— 它的窗口是
事件文件（含轮转备份）里保留的全部事件，所以每次退出会把窗口内有事件的每天各刷一份日报。
`papa report generate --run <run_id>` 是例外：只出一份 `<dir>/<run_id>/`（排查单次运行用，不按天拆）。

每次生成报告之后会按 `obs.report.retention` 清理过期目录（默认保留 7 天，负数 = 永久保留）：
只删报告根目录下**直接子目录、且目录里有 `report.md`** 的，手工放在同一目录里的其它东西不碰；
日报目录按名字里的日期判断（保留最近 N 个自然日），升级前留下的 `run_id` / `window-*` 旧目录按修改时间判断。

> 事件默认关着时，报告只做「失败分组 + 状态层检查」，`report.json` 的 `degraded` 会写明「事件数据缺失」。
> 想长期出报告就把 `obs.events.enabled` 打开。

## 4. 命令与产物

```bash
# 按配置出报告（默认读 configs/config.yaml 的 db / log.dir / obs 段）
# 默认按本地日历日拆分：有事件的每天各写一份 <dir>/<2006-01-02>/（同一天重跑刷新当天那份）
papa report generate -c configs/config.yaml

# 指定窗口 / 只出某一次运行的报告（单份 <dir>/<run_id>/，不按天拆）/ 跳过日志证据 / 产物目录
papa report generate -c configs/config.yaml --since 2026-10-28T00:00:00+08:00 \
  --until 2026-10-28T23:59:59+08:00 --run r-xxx --out ./reports --no-logs

# 列出报告目录下的子目录（按天拆分后通常是日期；也兼容升级前留下的 run_id / window-*）
papa report list -c configs/config.yaml
```

每次生成会逐份打印每天的判定与事件统计；真的清掉过期目录时，最后再打印「清理过期报告 N 份」。
`--fail-on-verdict` 在**任一**日报判定为「不通过」或「未判定」时以非零码退出（CI / 自动化门禁用）。

产物固定 4 个文件（同一 `<dir>/<2006-01-02>/` 下；`--run` 时是 `<dir>/<run_id>/`）：

| 文件 | 给谁 | 内容 |
| --- | --- | --- |
| `report.md` | 人 | 头部含**生成时间（最近一次更新）**与**模式**；判定、问题清单（含证据）、运行概览、队列/耗时、失败分组、**引擎状态**、**积压与老化（含 ETA）**、各检查段 |
| `report.json` | 机器/看板 | 同源结构化数据：`generated_at`（生成时间）/ `mode` / `verdict` / `verdict_reason` / `run` / `events` / `logs` / `degraded` / `engine` / `inputs`（自证：版本、阈值、事件覆盖）/ `sections` / `checks` / `problems` |
| `ai-brief.md` | AI / 接手排查的人 | 头部含**生成时间**；边界（只读、修复后必须复验；snapshot 报告追加「不要当验收结论，修完用 acceptance 复验」）、环境事实、问题证据 + 假设 + 验证命令 + 修复方向、已知陷阱、未知项、下一步（最多 8 条；没有假设的 check 类问题按首条证据定位） |
| `ai-brief.json` | AI 工具链 | 同源结构化版本（含 `generated_at`） |

> **生成时间 = 最近一次更新时间**：定时报告每天覆盖同一目录，重复生成会刷新这个时间；
> 看到它就能判断手上这份报告是不是最新的，不用依赖文件 mtime（分发、拷贝都会丢 mtime 语义）。

判定规则：任一 `fail` 级检查失败 → `fail`；只有 `warn` → `warn`；否则 `pass`。
`degraded` 非空表示数据不全（例如事件文件缺失、日志索引失败、DB 查询失败），此时结论只能当参考。

判定项表的结果列有四种：`✅ 通过` / `❌ 未通过`（只用于 `fail` / `warn`）、`ℹ️ 状态`
（`info` 级：只陈述状态是否成立，不表示通过或失败，如活跃阶段池、引擎空闲、恢复进度、漏斗进度）、
`➖ 不适用`（`skipped`，带原因）。不要把 `ℹ️ 状态` 当红绿灯读；判定只看 fail/warn。

**未判定（`unknown`）**：三种情况会判未判定 —— 一个可判定的检查都没有（全被标成不适用或为空）、
本来会判 `pass` 但 `degraded` 非空（数据不全是「没测到」，不是「没问题」）、`snapshot` 模式下引擎暂停/空闲
且没有别的问题。「未判定」既不是通过也不是失败：`report.md` 判定行写 `⏸ 未判定`（`report.json` 是
`"verdict": "unknown"` + `verdict_reason`），需要人工确认。

**`snapshot` 与 `acceptance` 的区别**：`acceptance`（验收）回答「这一轮通过没」—— 期末应当排空，
暂停和历史积压照常判；`snapshot`（快照）回答「此刻怎样」—— 引擎暂停或没有活跃阶段时，
`stuck_processing` / `unfinished_tasks` / `queue_drained` 这几项判不了，判定项里标 `➖ 不适用`
（`report.json` 是 `"skipped": true` + `skip_reason`），既不参与判定、也不合成 Problem。
引擎在跑时这两项（`stuck_processing` / `unfinished_tasks`）仍会报出来，但 severity 从 `fail` 降为 `warn`：
常驻进程在跑批中本来就有积压，定时日报不该因此长期判「不通过」；`acceptance` 保持 `fail`。
模式由触发方式定（定时=snapshot、退出/CLI=acceptance，`--snapshot` 强制，`obs.report.mode` 写死），
报告头部与 `report.json` 的 `mode` 会写明。

`--fail-on-verdict`：`fail` **或** `unknown` 都以非零码退出（`warn` 仍放行）—— CI 门禁不能把
「没判定出来」当通过。

> **后台也能看**：侧边栏 **General →「obs报告」** 页会把 `obs.report.dir` 下的历史报告列出来
> （判定 / 生成时间 / 产物数），选中后按纯文本看四份产物的原文并下载当前文件 —— 不用登服务器。
> 接口与安全约束见 [MONITOR.md](./MONITOR.md) §8。

## 5. 通用分析节与模式库

- **通用分析节** `framework`（`obs/analyze`）：运行边界与耗时、队列快照、失败分组（站点×阶段×错误类型）、
  任务终态分布，以及事件缺口 / 日志坏行 / 队列收敛 / 抓取失败率 / 卡死 / 未完成 / 事件覆盖等检查。
  阈值来自 `obs.report.thresholds`。
- **内置模式库**（`obs/pattern`，5 个通用模式）：`event-loss`、`process-restart`、`queue-not-draining`、
  `breaker-noise`、`stage-not-registered`。Pattern 只做「推断 + 验证方法」，不自动改数据。

batch2 起框架报告固定带三段排查信息，`report.md` 按注册顺序渲染（通用概览 → 引擎状态 → 积压与老化），
分别回答「现在是不是停着」「还有多少、还要多久」「事件本身全不全」：

### 5.1 引擎状态（段标题「引擎状态」）

回答「报告时刻引擎停没停、recover 跑到哪了」。三张表：

- **站点状态**：每站一行 `站点 / 暂停 / 暂停时间 / 阶段数`。它只陈述「报告时刻是否暂停」，暂停原因看后台铃铛
  或事件里的 `pause_resumed`（`source=manual` / `breaker`）。
- **阶段池（窗口内最后一次快照）**：`阶段 / 排队 / 未完成（含排队） / worker`，来自窗口内最后一条 `stage_snapshot`。
  注意「未完成」= `submitted - completed - failed`，**已经包含仍排队的任务**，所以它可能大于 worker 数；
  不要把它当"正在跑"的并发数。事件没开时这张表为空。
- **恢复进度**：窗口内 `task_requeued` 按队列（`recover_queue` / `error_queue` / `repeat_queue`）计数与首末时间，
  外加「DB 旧 processing」—— `updated_at` 超过 `stuck_after` 仍是 processing 的行数。启动恢复是分批的：
  processing 逐批变回 pending 是正常过程，不是卡死。判定项 `recover_in_progress` 的说明会把
  `recover_queue` 与其它队列的重投数分开写，避免把正常的轮询重投算进「恢复中」。

数据来源：定时/退出报告用进程内实时快照（`source=engine`）；`papa report generate` 离线时读
`crawler_config.sites` 的当前快照、并用窗口内事件推断阶段池（`source=db` / `events`）；两者都没有时摘要写
「无运行上下文（离线报告）」，此时不据此做暂停/空闲跳过。判定项里只有 `engine_paused` 会告警（warn，有站点暂停）；
`stage_pools` / `recover_in_progress` / `engine_idle` 都是 info，不参与判定。

### 5.2 积压与老化（段标题「积压与老化（含 ETA）」）

回答「还有多少、堆了多久、按当前速度还要多久」。两张表：

- **积压分布（crawler_tasks）**：按 `站点 × 阶段 × 状态 × 是否轮询 × 年龄桶` 聚合，只统计**非终态**
  （`待处理` / `处理中`）。年龄桶：待处理行按 `created_at` 算（排队等了多久），处理中行按 `updated_at` 算
  （距最后一次更新多久）。桶位是 `lt10m` / `10m-1h` / `1h-6h` / `gt6h`；`gt6h` 的处理中行多半是上次进程
  留下的孤儿，或彻底卡死。
- **吞吐与 ETA**：`站点/阶段 / 窗口完成数 / 吞吐(条/分) / 非轮询待处理 / ETA`。
  窗口完成数 = 窗口内 `task_finished` + `task_failed` 事件数；吞吐 = 完成数 ÷ **有效运行时长**
  （窗口时长 − 停机时长；停机 = `run_stopped` → 下一次 `run_started` 的区间，见运行边界表的
  「有效运行时长（剔除停机）」）。ETA = 该站该阶段非轮询待处理 ÷ 吞吐；段摘要里的「预计」是**整体口径**
  （全部非轮询待处理 ÷ 总吞吐），不是某一行阶段 ETA 的复述。**轮询（repeatable）待处理不计入 ETA** ——
  它们按轮询周期反复处理，算进去只会把 ETA 放大成没有意义的数。引擎暂停/空闲时 ETA 显示
  「ETA 不适用（引擎未运行）」。
  离线 `papa report generate` 的引擎状态取**当前** DB 快照：站点现在是暂停的，历史窗口的 ETA 也会标不适用
  （判定项同理），要算历史 ETA 得在引擎运行时用定时/退出报告。

判定项 `throughput_zero`（warn）：有非轮询待处理、窗口吞吐却是 0 = 引擎在跑但没吃任务（worker/队列问题）；
有吞吐时说明行写实际吞吐，不再复用「0 吞吐」的失败句式；引擎暂停/空闲时该检查标不适用
（这时 0 吞吐是正常的）。注意整段数字是**报告时刻的全库快照**，不是窗口末。

**队列/溢出表**：报告只看 `stage_snapshot` 的队列与在途数，加上 `spill_changed` 的溢出表 backlog
（引擎队列达高水位时任务先寄放在内存溢出表，`drain_interval` 再回灌）。所以 `queue_drained` 的
「未完成」= `Σ未完成（含排队） + Σ治理队列 backlog + 溢出表 backlog`，不再把排队数重复加一遍；
报告会单列一张「溢出表（spill，引擎内存）」小表，方便和 DB 的 pending 对上。

### 5.3 事件完整性（判定项 `event_coverage`）

`seq` 缺口只能发现「丢了单条」；进程被硬杀、日志轮转没接上时丢的是一整段，`seq` 反而连续 ——
只能靠**时间断层**发现：相邻两条事件的时间差超过 `obs.report.thresholds.event_max_gap`（默认 `10m`），
或窗口起点之后超过阈值才有首条事件，`event_coverage` 就判未通过（warn），message 带断档起止时间，
同时向 `degraded` 写「事件存在时间断层，事件口径可能不完整」。所以这项红了先怀疑事件文件缺了一段，
不要拿这段时间的「0 失败 / 0 完成」当结论 —— 先补数据，再做判定。

**停机不是丢事件**：如果最大断层刚好是一段 `run_stopped` → 下一次 `run_started`（起止秒级吻合），
说明那段时间引擎没在跑，`event_coverage` 判通过，message 写「引擎停机 X（起 ~ 止），不是事件丢失」，
且**不写 degraded**；运行边界表会给出「有效运行时长（剔除停机）」，吞吐/ETA 也按它算。
只有无法用运行边界解释的断层才按上面的丢事件路径处理。

引擎暂停/空闲时（`snapshot` 模式）该检查标不适用：暂停时本来就不产生事件，稀疏不代表丢失；
`acceptance` 模式仍按窗口检查（验收要看这一轮事件全不全）。
`report.json` 的 `inputs.events`（`first_ts` / `last_ts` / `max_gap` / `max_gap_from` / `max_gap_to`）是同一份口径，
可拿去独立复核；`report.md` 头部的「事件覆盖」行也来自它。

## 6. 业务项目怎么扩展

papa 是通用框架：**报告层不认识任何业务语义**（没有 prepare / push / 折扣 / 站点名）。业务在项目侧扩展：

```go
// 1) 注册自己的分析节（实现 obs.Section：ID/Title/Build）
app.UseReportSection(sections.All()...)

// 2) 注册自己的模式（实现 obs.Pattern：ID/Title/Match）
app.UseReportPattern(myPatterns...)

// 3) 发自己的业务事件（类型常量 + Attrs 自由键值，框架不解释）
if ev := app.Events(); ev != nil {
    ev.Emit(events.Event{
        Type:  "prepare_round_finished",
        Site:  "siteA", Stage: "prepare",
        Counts: map[string]int{"ok": 12, "failed": 1},
        Attrs:  map[string]string{"reason": "..."},
    })
}
```

扩展点契约（`papa/obs`）：

| 类型 | 用途 |
| --- | --- |
| `obs.Section` | 一个报告段：`ID()` / `Title()` / `Build(ctx, *obs.Input) (SectionResult, error)` |
| `obs.Check` + `obs.NewCheck` | 判定项：`ID` / `Severity` / `Passed` / `Message`；`Skipped` + `SkipReason` 声明本次不适用；任一**适用**的 fail 未通过即整体 fail |
| `obs.Pattern` | 模式：从输入推导 `Problem`（含 `Evidence` / `Hypothesis` / 验证命令） |
| `obs.Problem` / `obs.Evidence` / `obs.Hypothesis` / `obs.Table` | 问题、证据、假设、表格的通用结构 |

业务段也能用同一套判定语义：`Build` 拿到的 `Input.Mode`（`snapshot` / `acceptance`）决定按哪种口径判，
`Check.Skipped` + `SkipReason` 用来声明「本次不适用」（暂停、空闲、窗口对不上）—— 不适用的检查不参与判定、
也不合成 Problem。框架自带的 `stuck_processing` / `unfinished_tasks` / `queue_drained` / `event_coverage` /
`throughput_zero` 已按模式与引擎状态自己跳过，业务段要自己按 `Input.Mode` / `Input.Engine` 判。

边界铁律：**业务段只读**（不写表、不重投、不调接收端）、**确定性输出**（同一输入两次跑结果一致）、
**不把业务阈值写进 papa**（阈值放业务配置，如 `cfg.BusinessSection`）。

## 7. 报告怎么读（排查路径）

按「判定/原因 → 引擎状态 → 积压/ETA → 失败组证据」四步读，别一上来就 grep 日志：

1. **判定/原因**：先看 `report.json` 的 `verdict` / `verdict_reason` 与 `checks` —— 哪些 `passed=false`、
   `severity` 是 `fail` 还是 `warn`、哪些是 `➖ 不适用`（不适用既不是通过也不是失败）。若 `event_coverage` 未通过，
   说明事件本身就缺了一段，先补数据再谈其它结论。
2. **引擎状态（是不是暂停了）**：看「引擎状态」段 —— 哪些站点暂停、活跃阶段池几个、`recover_queue` 还在不在重投
   （恢复进度）。暂停/恢复期间「期末积压、卡住、0 吞吐」都不代表故障，先确认引擎在不在干活。
3. **积压/ETA（还要多久）**：看「积压与老化」段 —— 非轮询待处理多少、年龄落在哪个桶（`1h-6h` / `gt6h` 要认真看）、
   窗口吞吐与 ETA。有积压、吞吐却为 0、引擎又在跑，才是真的 worker/队列问题。
4. **失败组证据（为什么）**：最后看 `problems` —— 每条都带证据（事件 `task_id` / 日志 `file:line` 原文 / SQL 计数），
   失败分组按站点×阶段×错误类型聚合，组内证据按 `task_id` / URL 精确匹配（不再整组指向同一条日志行）。
   需要原文时按证据里的 `file:line` 去 `logs/*.log` 看上下文；event 证据可直接用 `--run` 过滤重出报告。

不确定从哪下手时用 `ai-brief.md`：它按「验证命令 → 修复方向 → 验收标准」组织，并带同样的段摘要。

## 8. 局限与注意

- **事件不是审计日志**：异步、有界、可能丢；缺口的判据是 `seq` 不连续。
- **`event_coverage` 只反映事件文件里能看到的**：进程被硬杀、轮转没接上导致的整段丢失只能靠上面的时间断层发现；
  事件本来就没开（`obs.events.enabled: false`）时报告已有「事件数据缺失」的 degraded，别指望这项兜底。
- **ETA 是有效运行时长内吞吐的线性外推**：完成数取窗口内 `task_finished` + `task_failed`，分母是
  「窗口 − 停机」；再假设吞吐不变、剩余任务同质 —— 遇到限速、批次性失败、阶段切换会偏乐观或偏悲观；
  只作量级参考，别当 SLA。
- **`inputs.snapshot_at` 是取 DB 快照的时刻**，`generated_at` 是这份产物最后写出的时刻，两者可能差几秒
  （先取快照，再分析、渲染、落盘）。判断「数字是哪一刻的」看 `snapshot_at`，判断「文件新不新」看 `generated_at`。
- 积压/年龄/ETA 是**报告时刻的全库快照**（不是窗口末）；待处理行年龄按 `created_at` 算（排队时长），
  处理中行按 `updated_at` 算（距上次更新）。
- 日志证据窗口会前后放宽 5 分钟（事件毫秒精度 vs 日志秒精度）；非 logrus 格式的文件会记进
  `logs.unparsed`，不影响判定但会拉低日志证据质量。
- **按天拆分看的是服务器本地时区**（`time.Local`）：零点前后的事件只落一天；日志证据也按天索引，
  前后 5 分钟的富余可能让边界日志同时出现在相邻两天的报告里。跨时区部署时以跑报告那台机器为准。
- 耗时口径：用事件的 `duration_ms` / trace；**不要用 `crawler_tasks.updated_at` 算任务耗时**（每次写都会刷新）。
- `stuck_after` 是**报告口径**（默认 10m），不是引擎的恢复口径；引擎的启动恢复按「启动边界」判定
  （见 [RECOVER_QUEUE.md](./RECOVER_QUEUE.md)）。
- **多进程/多副本**：Dashboard 的阶段概览读的是本进程内存快照；报告读共享 DB + 本机日志——
  多进程时按进程分别出报告，或把 `log.dir` 放到共享卷再统一读。
- 报告不替代监控后台：后台是**实时看板**（当前队列/worker/暂停），报告是**事后总结**（一段时间窗口）。

## 9. 相关手册

- 配置项字典：[CORE_CONFIG.md](./CORE_CONFIG.md)（`obs` / `log.level` / `crawler` 各段）
- CLI：[CLI.md](./CLI.md)（`papa report`、调试命令）
- 实时看板：[MONITOR.md](./MONITOR.md)
- 三条治理队列：[ERROR_QUEUE.md](./ERROR_QUEUE.md) / [RECOVER_QUEUE.md](./RECOVER_QUEUE.md) / [REPEAT_QUEUE.md](./REPEAT_QUEUE.md)
- 版本变更：papa 仓库的 `docs/issues/changes.md`（[GitHub](https://github.com/ydtg1993/papa/blob/master/docs/issues/changes.md)；该文件不随 `papa new` 复制进业务项目）
