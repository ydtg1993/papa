# Papa 运行观测手册（obs）

> 面向：想知道「这一轮跑得怎么样、哪里失败了、队列为什么堵」的人——不用翻库、不用逐行 grep 日志。
> 配置细节见 [CORE_CONFIG.md](./CORE_CONFIG.md) 的 `obs` 段与 `log.level`；命令见 [CLI.md](./CLI.md) 的 `papa report`。
> 业务项目想加自己的报表段/模式，看本文 §5、§6。

---

## 0. 在线辅助报告与离线分析

框架自带三层运行数据：**指标层**（`logs/events.jsonl` 结构化事件）、**证据层**（人类日志 + 任务 trace + 失败页面归档）、
**状态层**（MySQL 任务表）。`papa report` 把这三层合成 4 个产物：人读的 `report.md`、机读的 `report.json`、
给 AI/接手人用的 `ai-brief.md` 与 `ai-brief.json`。

在线报告只接收有限值信号，不读数据库、不扫描历史 JSONL/日志、不执行业务 Section/Pattern。它维护有限维度计数、最后状态和少量失败样本；计数仅表示收到的信号，不能作为权威任务总数。在线产物固定为 `snapshot`，`engine.source=signals`、判定 `unknown`，数据来源和丢弃/截断会明确说明 `partial` / `unavailable`。Runner 的 Status 为纯原子健康查询，不等待聚合或 writer；`archive_dropped` 单独表示归档丢弃，不混入在线报告接收/丢弃口径。

> **默认关闭**：`obs.events.enabled=false` 同时关闭在线聚合与事件归档。开启后，`report.interval` 和 `report.on_exit` 分别控制在线出报告时机。离线 `papa report generate` 不受此开关禁止，但默认及 `--snapshot` 路径没有窗口事件时会报错，不能仅凭日志/DB 自动确定单日报告窗口。显式 `--run` 路径及自行调用离线 `obs.Generate` 可提供其他证据，仍须声明来源不完整。

事件输入只接受当前 writer 的 schema `v: 1`；缺失 `v`、零版本及未知版本与非法 JSON 一样计入事件 `Unparsed`，跳过该行后继续读取，不升级旧事件。当前 schema 内的未知字段仍忽略。事件 schema 版本与 Papa 的主版本号独立。

## 1. 三层数据与职责

| 层 | 产出 | 谁写 | 回答什么问题 | 默认 |
| --- | --- | --- | --- | --- |
| 指标层 | `logs/events.jsonl`（JSONL，一行一事件） | 引擎异步 emitter（`pkg/events`） | 成功多少、多快、卡在哪个阶段、失败怎么分组 | 关（`obs.events.enabled` 开） |
| 证据层 | `logs/*.log`、`traces/`、`logs/fetcher-html/` 归档 | logrus / 引擎 trace / 归档器 | 失败那一刻的原文、`file:line` 样本、task 级轨迹 | 日志默认开；trace/归档按配置 |
| 状态层 | MySQL `crawler_tasks`（及业务表） | 引擎 / 业务 | 当前事实：谁 pending、谁 processing、终态分布 | 永远在 |

离线分析对被观测数据只读：可以读事件文件、日志目录和数据库；不写业务表、不重新投递任务、不调用外部接收端。在线服务只持有值信号与自有报告文件，两者都会写报告产物。

### 诊断输出的脱敏边界

框架 `LoggerSet` 的文件/控制台格式化、JSONL 事件、任务失败 error 列、Webhook 告警正文、日志索引和四份报告产物使用 `pkg/redact` 处理诊断副本。已知键包括 token/sign/sig/key/api_key/access_token/session/password/authorization/cookie/dsn 等，大小写不敏感；URL userinfo 会去除，已知查询参数值变成 `***`，Authorization/Cookie 行与常见 MySQL DSN 也会遮盖。JSON 的 `attrs/fields/headers/environment` 属性包中，敏感键的数字、对象、数组同样整体遮盖；结构化站点/报表的普通 `key` 保持原值。历史 logfmt 先解码再脱敏，发生遮盖的 Raw 样本会重建为等价 logfmt，因此字段顺序/引号可能不同，定位仍看 file:line。

实际请求 URL、任务身份/去重键、数据库中的任务 URL 和断点恢复内容不被改写，Webhook 的签名目标 URL 也原样请求。自定义 logger/formatter/sink/notifier 不自动继承这些出口约束。此机制不会清除任意正文中的秘密或未知字段，更不能替代业务采样脱敏；Trace 的任意 data、原始 HTML 归档、SQL 字面量和历史原文件不属于自动清洗承诺范围。敏感原文件仍需访问与保留期管理。

事件的 `counts/stages/queues` 字典键是指标或阶段名称，即使叫 `token/session` 也保留原名和数字/快照类型；队列错误与阶段描述文字单独在副本中脱敏。凭据字段与指标名称不能混为一谈。错误已经截断、缺少闭引号时仍遮盖可识别的凭据片段；URI userinfo 内的单引号、MySQL DSN 密码内的 `@` 也不会使匹配提前停止。Emit 在异步边界内复制并限长字符串和 map，返回后调用方可修改原 map；调用期间仍不得并发修改同一 map。单字符串最多 2 KiB、总字符串最多 16 KiB、所有 map 合计最多 64 项；超出字符串预算时整体省略，不能保留截掉凭据分隔符的前缀后再依赖脱敏；省略内容标记 `truncated`。

`report.md` 与 `ai-brief.md` 的动态标题、表格、摘要和证据按文本上下文转义，换行不再改变表格或注入新标题；复杂代码引用用固定 code 元素包裹已转义文本。报告 JSON 仅脱敏，不混入 Markdown 转义；输入报告对象保持不变。是否允许 HTML 执行仍由具体 Markdown 渲染器决定，这项修复不等同于宣称所有渲染器的 XSS 防护。

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
`source` / `error` / `attrs` / `stages` / `queues` / `truncated`。

内置事件类型：

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
| `breaker_tripped` / `breaker_transition` | 熔断触发及恢复状态变化 |
| `pause_resumed` | 暂停/恢复信号变化 |

约定：

- **框架不解释 `attrs` 的语义**：业务自定义事件（例如「预处理完成」「推送完成」）用自己定义的 `type` + `attrs`
  键值即可，papa 按载荷和队列预算尽力保留，不保证每个字段或事件都被写入。
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
    mode: "auto"         # 仅离线分析：默认 snapshot；验收显式选 acceptance
    on_exit: false       # 进程优雅退出时自动生成报告（默认 false）
    interval: "0"        # 定时生成报告：如 "30m" = Run 期间每 30 分钟刷新一次当天日报；0/不写 = 关
    dir: "./reports"     # 产物根目录：<dir>/<2006-01-02>/（一份 = 一天的日报；--run 时 <dir>/<run_id>/）
    retention: "168h"    # 报告保留期：默认 7 天；负数 = 永久保留（0/不写同默认）；在线有限清理/无筛选离线 CLI 清理
    timeout: "120s"      # 离线生成的协作超时；在线退出与归档合计最多等 2 秒
    thresholds:          # 离线通用分析节（framework）的阈值
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

定时与退出报告都写当前本地日目录，后写覆盖前写。在线聚合按本次进程收到信号的日期切换，不重扫当天历史文件，也不重写历史日；进程重启后的报告不能代表当天此前进程的全量。定时和退出报告需要 `obs.events.enabled=true`。

App 在 Engine 真实排空结果确定后发出 `run_stopped`，随后停止在线入口并尽力生成末次报告。报告与归档共用一个最长 **2 秒**的退出等待期限；超时返回，不阻止业务 DB/浏览器按原排空条件关闭。写盘阻塞时保留同一个固定后台单元，不启动替代 goroutine，也不强杀文件 I/O。未产生退出报告不改变 Engine 的排空事实。

在线聚合为一个固定消费者，写报告为一个固定 writer，最多一个在写与一个待写快照；新快照可合并旧请求。队满、维度上限、panic 和文件错误会丢弃或降级；这些组件共享进程 CPU/内存/磁盘，不承诺物理零开销或隔离 OOM/fatal。

CLI 报告产物**按本地日历日拆分**：一次生成把事件窗口按本地时区切成若干天，有事件的每天各写一份
`<dir>/<2006-01-02>/`（四份产物；同一天重跑刷新当天那份）。进程内定时和退出报告仅写当前本地日，不重写历史日报。
`papa report generate --run <run_id>` 是例外：只出一份 `<dir>/<run_id>/`（排查单次运行用，不按天拆）。

指定 `--run` 时，事件数和 `event_gaps` 缺口检查只统计该次运行在所选时间窗口内的事件；不指定时，缺口按各运行分别计算后相加。只检查可见事件相邻序号之间的缺口，不把窗口外的序号算作丢失。文件数、无法解析行数和重复行数仍是输入文件读取诊断，不随 `--run` 缩小范围。已有报告需重新生成才能更新这些统计。

默认 CLI 运行报告仍按有事件的每个本地日分别生成，不能因默认模式为 snapshot 而丢掉历史日。显式 `--snapshot` 只生成最后一天；进程内定时/退出报告只写当天。每份日报单独重建引擎上下文，不跨天复用首日状态。

历史日报的数据库表格是**生成时快照，非历史期末状态**。数据库完成度、卡住和一致性检查标「不适用」，不据此判历史窗口失败；窗口内事件失败仍检查。当前日报可结合当前数据库状态分析。

无时间/run 筛选的离线 CLI 生成之后会按 `obs.report.retention` 清理过期目录（默认保留 7 天，负数 = 永久保留）：
只删报告根目录下**直接子目录、且目录里有 `report.md`** 的，手工放在同一目录里的其它东西不碰；
CLI 指定 `--since`、`--until` 或 `--run` 时只生成、不执行保留清理，保证显式历史产物可以取走；无这些筛选的 CLI 生成仍按离线保留期清理。历史产物之后仍可能被自动清理，需要长期保存时请使用独立 `--out` 目录。

离线日报目录按名字里的日期判断（保留最近 N 个自然日），旧 `run_id` / `window-*` 目录按修改时间判断。在线 writer 维护自有目录游标，每轮最多查看 128 个目录项，后续轮次继续扫描，避免无关项长期遮挡后面的过期产物；只清理过期日期目录中的四份已知产物；保留无关文件，目录为空才删除，不读取或清理旧 run/window 目录。

> 离线分析在事件默认关着时，只做「失败分组 + 状态层检查」，`report.json` 的 `degraded` 会写明「事件数据缺失」。
> 想长期出报告就把 `obs.events.enabled` 打开。

## 4. 命令与产物

```bash
# 按配置出报告（默认读 configs/config.yaml 的 db / log.dir / obs 段）
# 默认按本地日历日拆分：有事件的每天各写一份 <dir>/<2006-01-02>/（同一天重跑刷新当天那份）
papa report generate -c configs/config.yaml

# 指定窗口 / 只出某一次运行的报告（单份 <dir>/<run_id>/，不按天拆）/ 跳过日志证据 / 产物目录
papa report generate -c configs/config.yaml --since 2026-10-28T00:00:00+08:00 \
  --until 2026-10-28T23:59:59+08:00 --run r-xxx --out ./reports --no-logs

# 列出报告目录下的子目录（按天拆分后通常是日期；显式单次报告也可使用 run_id / window-*）
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

离线判定规则：任一 `fail` 级检查失败 → `fail`；只有 `warn` → `warn`；否则 `pass`。
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
引擎在跑时这两项（`stuck_processing` / `unfinished_tasks`）以 `info` 展示待处理状态：
单次存留或长期未更新时间不足以证明执行者失活；`acceptance` 保留显式排空验收。
离线 `auto` 默认为 `snapshot`；`obs.report.mode: acceptance` 或 CLI `--acceptance` 显式启用离线验收。在线固定 snapshot，不读取这些验收参数。`--snapshot` / `--acceptance` 互斥且优先于配置，
报告头部与 `report.json` 的 `mode` 会写明。

`--fail-on-verdict`：`fail` **或** `unknown` 都以非零码退出（`warn` 仍放行）—— CI 门禁不能把
「没判定出来」当通过。

> **后台也能看**：侧边栏 **General →「obs报告」** 页会把 `obs.report.dir` 下的历史报告列出来
> （判定 / 生成时间 / 产物数），选中后按纯文本看四份产物的原文并下载当前文件 —— 不用登服务器。
> 接口与安全约束见 [MONITOR.md](./MONITOR.md) §8。

## 5. 离线通用分析节与模式库

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

数据来源：在线定时/退出报告为 `source=signals`，不查询完整引擎状态或数据库，不运行本节分析器；`papa report generate` 离线时读
`crawler_config.sites` 的当前快照、并用窗口内事件推断阶段池（`source=db` / `events`）；两者都没有时摘要写
「无运行上下文（离线报告）」，此时不据此做暂停/空闲跳过。`engine_paused` 仅在有熔断暂停时告警（warn）；人工/启动暂停为 info；
`stage_pools` / `recover_in_progress` / `engine_idle` 都是 info，不参与判定。

### 5.2 积压与老化（段标题「积压与老化（含 ETA）」）

回答「还有多少、堆了多久、按当前速度还要多久」。两张表：

- **积压分布（crawler_tasks）**：按 `站点 × 阶段 × 状态 × 是否轮询 × 年龄桶` 聚合，只统计**非终态**
  （`待处理` / `处理中`）。年龄桶：待处理行按 `created_at` 算（排队等了多久），处理中行按 `updated_at` 算
  （距最后一次更新多久）。桶位是 `lt10m` / `10m-1h` / `1h-6h` / `gt6h`；`gt6h` 的处理中行多半是上次进程
  留下的孤儿，或彻底卡死。
- **吞吐与 ETA**：`站点/阶段 / 窗口完成数 / 吞吐(条/分) / 非轮询待处理 / ETA`。
  窗口完成数 = 窗口内 `task_finished` + `task_failed` 事件数；吞吐 = 完成数 ÷ **有事件依据的运行时长（含暂停）**。
  时长按窗口内各 `run_id` 的首末可见事件形成区间，再合并重叠区间；单条事件没有正的时间跨度。
  它包含区间内暂停，不代表抓取有效工作时间，也不按「整日窗口 − 停机」推算。已识别的
  `run_stopped` → 下一次 `run_started` 停机区间另列展示。ETA = 该站该阶段非轮询待处理 ÷ 吞吐；段摘要里的「预计」是**整体口径**
  （全部非轮询待处理 ÷ 总吞吐），不是某一行阶段 ETA 的复述。**轮询（repeatable）待处理不计入 ETA** ——
  它们按轮询周期反复处理，算进去只会把 ETA 放大成没有意义的数。引擎暂停/空闲时 ETA 显示
  「ETA 不适用（引擎未运行）」。
  离线报告中的 DB 数字是**生成时快照**；历史日报不能用当前暂停状态、积压或老化反推历史期末，相关 DB 检查标不适用。窗口内事件证据仍保留，不能把当前 DB 数字当作历史 ETA 的分子。

判定项 `throughput_zero`：snapshot 中以 info 展示状态，显式 acceptance 中保留 warn。只有有足够事件跨度且检查适用时，
才能结合非轮询待处理量分析零吞吐；没有正的可观测跨度、引擎暂停/空闲或历史日报时，检查标不适用，
不能据此推断 worker/队列故障。有吞吐时说明行写实际吞吐。积压数字是**报告时刻的全库快照**，不是窗口末。

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
且**不写 degraded**。已识别停机单独展示；吞吐/ETA 的分母按各 run 首末可见事件区间合并计算，
运行边界表标为「有事件依据的运行时长（含暂停）」，不能用整日时长减停机替代。
只有无法用运行边界解释的断层才按上面的丢事件路径处理。

引擎暂停/空闲时（`snapshot` 模式）该检查标不适用：暂停时本来就不产生事件，稀疏不代表丢失；
`acceptance` 模式仍按窗口检查（验收要看这一轮事件全不全）。
`report.json` 的 `inputs.events`（`first_ts` / `last_ts` / `max_gap` / `max_gap_from` / `max_gap_to`）是同一份口径，
可拿去独立复核；`report.md` 头部的「事件覆盖」行也来自它。

## 6. 业务项目怎么扩展

papa 是通用框架：**报告层不认识任何业务语义**（没有 prepare / push / 折扣 / 站点名）。业务在项目侧扩展：

```go
// 1) 在业务自己的离线命令中显式调用，不注册到 App 在线生命周期。
rep, err := obs.Generate(ctx, obs.Options{
    DB: db, // 借用连接；由离线命令负责生命周期
    Sections: sections.All(), // 实现 Key / Title / Build
    Patterns: myPatterns,     // 实现 Key / Match
})
if err != nil { return err }
_, err = report.Write(outputDir, rep)

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

App 的 `UseReportSection` / `UseReportPattern` 已移除，不保留转发或静默忽略的空入口。扩展只在业务自己的离线 `obs.Generate` 调用中执行。Key/Title/Build/Match 的 panic 会局部转成 degraded；调用之间检查 context，取消返回错误。任意阻塞回调不能被强制终止，离线宿主仍须正确管理借用资源。

扩展点契约（`papa/obs`）：

| 类型 | 用途 |
| --- | --- |
| `obs.Section` | 一个报告段：`Key()` / `Title()` / `Build(ctx, *obs.Input) (*SectionResult, error)` |
| `obs.Check` + `obs.NewCheck` | 判定项：`ID` / `Severity` / `Passed` / `Message`；`Skipped` + `SkipReason` 声明本次不适用；任一**适用**的 fail 未通过即整体 fail |
| `obs.Pattern` | 模式：从输入推导 `Problem`（含 `Evidence` / `Hypothesis` / 验证命令） |
| `obs.Problem` / `obs.Evidence` / `obs.Hypothesis` / `obs.Table` | 问题、证据、假设、表格的通用结构 |

业务段也能用同一套判定语义：`Build` 拿到的 `Input.Mode`（`snapshot` / `acceptance`）决定按哪种口径判，
`Check.Skipped` + `SkipReason` 用来声明「本次不适用」（暂停、空闲、窗口对不上）—— 不适用的检查不参与判定、
也不合成 Problem。框架自带的 `stuck_processing` / `unfinished_tasks` / `queue_drained` / `event_coverage` /
`throughput_zero` 已按模式与引擎状态自己跳过，业务段按自己的执行语义判定；抓取引擎暂停不代表预处理或推送暂停，不能据此跳过业务失败检查。

边界铁律：**业务段只读**（不写表、不重投、不调接收端）、**确定性输出**（同一输入两次跑结果一致）、
**不把业务阈值写进 papa**（阈值放业务配置，如 `cfg.BusinessSection`）。

## 7. 报告怎么读（排查路径）

在线先看信号来源、缺失/丢弃和最后状态，需要查库或历史对账时显式运行离线命令。离线按「判定/原因 → 引擎状态 → 积压/ETA → 失败组证据」四步读，别一上来就 grep 日志：

1. **判定/原因**：先看 `report.json` 的 `verdict` / `verdict_reason` 与 `checks` —— 哪些 `passed=false`、
   `severity` 是 `fail` 还是 `warn`、哪些是 `➖ 不适用`（不适用既不是通过也不是失败）。若 `event_coverage` 未通过，
   说明事件本身就缺了一段，先补数据再谈其它结论。
2. **引擎状态（是不是暂停了）**：看「引擎状态」段 —— 哪些站点暂停、活跃阶段池几个、`recover_queue` 还在不在重投
   （恢复进度）。暂停/恢复期间「期末积压、卡住、0 吞吐」都不代表故障，先确认引擎在不在干活。
3. **积压/ETA（还要多久）**：看「积压与老化」段 —— 非轮询待处理多少、年龄落在哪个桶（`1h-6h` / `gt6h` 要认真看）、
   窗口吞吐与 ETA。先确认有正的可观测事件跨度、引擎确在运行且检查适用；零吞吐与积压本身不足以证明 worker/队列故障。
4. **失败组证据（为什么）**：最后看 `problems` —— 每条都带证据（事件 `task_id` / 日志 `file:line` 原文 / SQL 计数），
   失败分组按站点×阶段×错误类型聚合，组内证据按 `task_id` / URL 精确匹配（不再整组指向同一条日志行）。
   需要原文时按证据里的 `file:line` 去 `logs/*.log` 看上下文；event 证据可直接用 `--run` 过滤重出报告。

不确定从哪下手时用 `ai-brief.md`：它按「验证命令 → 修复方向 → 验收标准」组织，并带同样的段摘要。

## 8. 局限与注意

- **事件不是审计日志**：异步、有界、可能丢；缺口的判据是 `seq` 不连续。
- **`event_coverage` 只反映事件文件里能看到的**：进程被硬杀、轮转没接上导致的整段丢失只能靠上面的时间断层发现；
  事件本来就没开（`obs.events.enabled: false`）时报告已有「事件数据缺失」的 degraded，别指望这项兜底。
- **ETA 是观测吞吐的线性外推**：完成数取窗口内 `task_finished` + `task_failed`，分母为各 run 首末可见事件
  区间合并后的时长（含暂停），不从整日减去停机估算。再假设吞吐不变、剩余任务同质；限速、批次失败、阶段切换
  和事件缺失都会影响结果。缺少正的事件跨度时不给出有效 ETA，也不据此判零吞吐异常；只作量级参考，别当 SLA。
- **`inputs.snapshot_at` 是本份输入快照时刻**；在线为信号聚合时刻，离线可对应 DB 采样，`generated_at` 是这份产物最后写出的时刻，两者可能差几秒
  （先取快照，再分析、渲染、落盘）。判断「数字是哪一刻的」看 `snapshot_at`，判断「文件新不新」看 `generated_at`。
- 离线积压/年龄/ETA 是**报告时刻的全库快照**（不是窗口末）；待处理行年龄按 `created_at` 算（排队时长），
  处理中行按 `updated_at` 算（距上次更新）。
- 日志证据窗口会前后放宽 5 分钟（事件毫秒精度 vs 日志秒精度）；非 logrus 格式的文件会记进
  `logs.unparsed`，不影响判定但会拉低日志证据质量。
- **按天拆分看的是服务器本地时区**（`time.Local`）：零点前后的事件只落一天；日志证据也按天索引，
  前后 5 分钟的富余可能让边界日志同时出现在相邻两天的报告里。跨时区部署时以跑报告那台机器为准。
- 耗时口径：用事件的 `duration_ms` / trace；**不要用 `crawler_tasks.updated_at` 算任务耗时**（每次写都会刷新）。
- `stuck_after` 是**报告口径**（默认 10m），不是引擎的恢复口径；引擎的启动恢复按「启动边界」判定
  （见 [RECOVER_QUEUE.md](./RECOVER_QUEUE.md)）。
- **单实例范围**：Dashboard 的阶段概览读取当前进程内存；在线报告只消费有限值信号。离线 CLI 可读取该实例的数据库和日志生成报告，不汇总多个采集实例。
- 报告不替代监控后台：后台是**实时看板**（当前队列/worker/暂停），报告是**事后总结**（一段时间窗口）。

## 9. 相关手册

- 配置项字典：[CORE_CONFIG.md](./CORE_CONFIG.md)（`obs` / `log.level` / `crawler` 各段）
- CLI：[CLI.md](./CLI.md)（`papa report`、调试命令）
- 实时看板：[MONITOR.md](./MONITOR.md)
- 三条治理队列：[ERROR_QUEUE.md](./ERROR_QUEUE.md) / [RECOVER_QUEUE.md](./RECOVER_QUEUE.md) / [REPEAT_QUEUE.md](./REPEAT_QUEUE.md)

单实例调度、报告输入和下载预算的装配 API、有限默认值及超限处理见 [资源预算与背压](RESOURCE_BUDGETS.md)。这些预算与站点/阶段的业务并发配置分开，不是新增 YAML 键；旧公开配置布局保留。


### 在线接收时间与不完整维度

标准事件时间在异步入口采样，阻塞归档不会改写为排空时间；显式注入的时钟回调仍隔离在消费者。在线日报按接收自然日处理，日期只向前推进；旧日迟到信号只进运行累计，单列 `late_day_signals`，不承诺重写昨日文件。同日窗口覆盖实际计入信号。超过 256 字节或已经截断的站点/阶段信号保留总计，但不进入精确分组并计入 `group_overflow`，避免前缀相同的维度混算。


归档配置 run_id 计入事件总字符串预算；在线报告再次限制失败样本时同样整值省略，避免二次截短破坏凭据脱敏。合法预算内的内容保持原语义，省略不等于任务执行失败。
