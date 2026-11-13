# Papa 数据落地手册

框架任务时间列及 UTC 存储规则见 [TASK_TIMING.md](../runtime/TASK_TIMING.md)。当前 v4 模型变更需要停 worker 后显式迁移；旧版本使用独立数据库，见 §6。不将任务最近结束记录视为业务事务提交时间。

> 面向：写完 fetcher 之后，抓到的内容怎么落地成表、写进数据库、以及写在哪里。
> 配套阅读：[FETCHER_WRITING_GUIDE.md](FETCHER_WRITING_GUIDE.md)（抓取逻辑怎么写）。

---

## 0. 一句话

采集结果由业务**自己建表、自己写**；框架只保存调度状态：

- `crawler_tasks` 记的是「这条 URL 属于哪个站点的哪个阶段、跑到哪一步、重试几次、要不要轮询、业务键是什么」——**它没有结果列**。
- 抓到的内容要落库，就在项目的 `internal/models/` 包里定义结构、把它登记进 `models.Models()`，然后用 `engine.Upsert` 写进去（建表走 `papa migrate`）。

> 任务表不提供业务结果读写 API；业务表通过宿主注册和迁移，与任务状态分开维护。

---

## 1. 两类数据，两条生命周期

框架身份表 `crawler_browser_profiles` / `crawler_browser_identity_bindings` 保存档案与绑定修订，不存采集结果或凭据，随显式框架迁移创建。Cookie/Token/浏览器状态及改绑候选仍走账号 Store。匿名业务需要持久保存自己的稳定会话键；`htmlfetch.Session` 不新增 ID。详见 [BROWSER_IDENTITY.md](../accounts/BROWSER_IDENTITY.md)。

| | 调度状态 | 业务结果 |
| --- | --- | --- |
| 存在哪 | `crawler_tasks`（框架的表） | 你自己的表（`internal/models/` 里定义） |
| 谁写 | 只有引擎 | 只有你（fetcher / 后台动作） |
| 生命周期 | 任务跑完、被清理、被重投都归调度管 | 数据本身长期存在，与任务行无关 |
| 形状 | 固定的几个列 | 一类数据一张表，字段你定 |

把它们分开的好处很实在：业务表能建唯一键、外键、按业务维度建索引，后台能单独给它开一张表格页；而任务行被重投或清理时，业务数据不会跟着遭殃。

---

## 2. 任务表 `crawler_tasks`：业务**只读**，不要写

| 列 | 说明 |
| --- | --- |
| `id` / `p_id` | 任务 ID / 父任务 ID（阶段串联靠它） |
| `url` | 执行 URL；任务身份独立使用 identity_type/value/hash |
| `stage` / `site` | 属于哪个阶段、哪个站点（`SiteSpec.Key`，**必填** —— 没有「未归属」那一档） |
| `account_pool` | 任务在该站的账号池归属；空表示未使用池，不参与标准任务身份去重 |
| `status` / `retry` / `error` | 运行态：待处理 / 处理中 / 成功 / 失败、重试次数、错误信息 |
| `repeatable` / `repeat` / `repeat_interval` / `next_repeat_at` / `last_repeat_at` | 轮询相关，调度用 |
| `reprocess` / `urgent` | 失败重投的代次（同时是后台动作的版本守卫）、加急标记 |
| `identity_type` / `identity_value` / `identity_hash` | 标准身份类型、原值和 SHA-256，见下 |
| `failure_kind` / `compensation_mode` / `compensation_reason` / `next_compensation_at` / `compensation_version` | 失败分类、补偿门控和版本，由执行及补偿组件维护 |
| `last_account_id` / `last_account_selection_version` / `account_attempts` | 最近账号与最多 8 次认领关联，独立于 Trace；不保存凭据 |
| `meta` | **业务键**（JSON），见下 |

- **业务键写 `meta`**：给 `papa.Task{Meta: map[string]string{...}}` 赋值即可，它随行落库。
  恢复队列、轮询队列、错误队列重投、后台「重投 / 加急」都按这一列还原任务身份 ——
  不落库的话任务重投一次身份就没了（handler 拿不到业务键，报错还指不到这里）。
  放**键**（`series_id` / `episode_id`），别放数据本身。
- **唯一索引建在 `(site, stage, identity_hash)`**，同一站点阶段内按 ID 或规范 URL 去重；旧 URL 哈希不再独立限制唯一性。
- `Task.AccountPool` 可在首次提交时显式指定，或由当前站点/阶段运行策略解析；之后恢复、轮询及失败补偿都保存这一归属。重复提交指定不同池会报冲突，不改旧行、不创建新任务。实际账号在执行尝试时取得，换号不会改变任务去重键，也不会迁移已有任务的池名。Cookie/Token 不写进 Meta 或任务表。
- **标准身份**：`Task.ResourceID` 非空时采用 ID，否则采用保留 origin 的规范 URL。框架存储 `identity_type`、`identity_value`、`identity_hash`；哈希输入是包含版本、站点、阶段、类型和值的 JSON 字符串数组，不直接拼接分隔符。
- 只接受 v4 身份字段；旧任务键/哈希字段与回填升级逻辑已删除。
- 改用 ResourceID 是身份契约变化，不应把已有 URL 身份静默转换成 ID 身份。框架不会自动合并两条记录，也不重写业务表身份。
- 其余列由引擎维护，业务**不要直接改写**；后台任务表的「重投 / 加急 / 开停轮询 / 标失败 / 删除」已经覆盖了常见的运维动作。

---

## 3. 结果表怎么建

三类决定，定完就能写代码：

1. **一类数据一张表**。剧集一张、分集一张、下载记录一张 —— 不要塞进一个 JSON 列再靠应用层拆。
2. **唯一键选「站点内稳定标识」**：站内 ID（如 `book_id`）最稳；只有 URL 可靠时用规范化后的 URL 或它的 hash。
   多站同进程时唯一键要带上站点（`site + 业务键`），否则两个站撞同一行。
3. **登记进 `models.Models()`**，否则 `papa migrate` 不会建表：

```go
// models/models.go
func Models() []any { return []any{&Series{}, &Episode{}} }
```

```go
// models/series.go
type Series struct {
    ID        uint64    `gorm:"primaryKey;comment:主键"`
    Site      string    `gorm:"type:varchar(128);not null;uniqueIndex:ux_series_site_key,priority:1;comment:站点 Key"`
    SourceKey string    `gorm:"type:varchar(191);not null;uniqueIndex:ux_series_site_key,priority:2;comment:站点内稳定标识"`
    Title     string    `gorm:"type:varchar(512);not null;comment:标题"`
    CoverURL  string    `gorm:"type:varchar(2048);comment:封面 URL"`
    Status    int       `gorm:"index;comment:业务自己的状态机"`
    CreatedAt time.Time `gorm:"autoCreateTime;comment:创建时间"`
    UpdatedAt time.Time `gorm:"autoUpdateTime;comment:更新时间"`
}
```

建表：项目目录里跑 `papa migrate`（脚手架中等价于 `make migrate`）。底层 AutoMigrate 通常可重复执行，但也可能调整数据库支持的类型和约束；生产变更须审查 DDL，不能把重复执行理解成无条件安全。

**每个字段都要写 `comment:...`**：注释随 `papa migrate` 进库（`information_schema.COLUMNS.COLUMN_COMMENT`），排查时不用再猜列含义。注意 gorm 的 `MigrateColumn` 会跳过**已存在表的主键列**注释，老库的 `id` 注释要手工 `ALTER TABLE ... MODIFY COLUMN ... COMMENT` 补（新建表时随 CREATE TABLE 自动带上）。

---

## 4. 怎么写

### 4.1 单个对象：`engine.Upsert`

冲突更新并回填主键 —— 不用自己拼 `ON DUPLICATE KEY UPDATE`：

```go
series := &models.Series{Site: task.Site, SourceKey: key, Title: title, CoverURL: coverURL}
if err := engine.Upsert(series, []string{"site", "source_key"}, []string{"title", "cover_url"}); err != nil {
    return err
}
// series.ID 此时已回填，可以直接拿它当子任务的外键
```

- 第二个参数是**冲突键**（要和表上的唯一键一致），第三个是**冲突时要更新的列**（`created_at` / 主键不要放进来）。
- 拿不到对象、只想写几列时，也可以 `engine.GetDB()` 拿 gorm 实例自己写。

### 4.2 关联两个阶段：用业务外键，别回写父任务行

父任务的 `id` 只适合放进子任务的 `meta`（还原身份用）。**业务关系写业务表**：

```go
// 详情阶段：先 upsert 剧集，拿到 ID
if err := engine.Upsert(series, []string{"site", "source_key"}, []string{"title"}); err != nil { return err }
// 再把它派给下一阶段（业务键随行落库，重投时还在）
return engine.SubmitTask(&papa.Task{
    PID: task.ID, URL: detailURL, Stage: StageEpisode, Site: task.Site,
    Meta: map[string]string{"series_id": fmt.Sprint(series.ID)},
})
```

### 4.3 写入必须幂等

任务会被重投、轮询会重跑、后台会「重投 / 加急」——**同一个 handler 可能对同一份数据跑好几次**。所以：

- 一律 `Upsert`（或带唯一键的 INSERT），不要裸 `Create`；
- 别在 handler 里累加（`count = count + 1` 这类要带条件更新）；
- 回写别用「读-改-写整行」：并发下会丢更新，优先用一条 `UPDATE ... SET x = ?` 或唯一键冲突更新。

引擎在业务返回后遇到终态写库错误，会保留当前 worker 并只重试状态写入；这减少了运行期因写回故障重复执行业务的机会，但业务数据与任务状态不在同一事务中，进程崩溃后的恢复仍可能重跑，不能省略幂等约束。

---

## 5. 常见坑

1. **忘了登记 `models.Models()`** → `papa migrate` 不建表，运行时才报 `Table doesn't exist`。
2. **没建唯一键** → 重投/轮询一轮就多一行；`Upsert` 也会退化成普通插入。
3. **站点维度漏了一处**（提交任务时不带 `site`、按阶段取配置只给名字）→ 声明或提交被拒绝，或查不到对应阶段。标准任务身份由框架自动纳入站点与阶段，业务不再手拼作用域前缀。
4. **`meta` 里塞数据** → 它是给"还原任务身份"用的，塞大对象既没索引也不该随任务清理而消失。
5. **业务列名与 gorm 生成的实际列名对不上**（后台表格页 `Field` 写 `pid` 而库里是 `p_id`）→ 后台永远渲染成空，见 [MONITOR.md](../operations/MONITOR.md)。
6. **改了表结构没跑迁移** → 普通启动不会迁移。`AutoMigrate` 会建表、补列/索引，并可能调整其支持的类型和约束；不会替业务推断删列、改名或数据回填。涉及这些意图时应审查并编写专用迁移 SQL，见 [CORE_CONFIG.md](../configuration/CORE_CONFIG.md) 第 2 节。

---

## 6. 独立 v4 数据库

v4 不提供旧版本的表结构或任务身份转换。使用独立数据库，通过显式 `papa migrate` 创建当前模型；已有任务表必须通过 v4 身份列和数据检查，失败时在迁移写入前返回错误。框架不删除旧数据库或其中的业务数据。

当前 v4 的后续模型变更仍走显式 AutoMigrate；普通启动不执行迁移。跨版本业务数据搬迁由独立业务模块自行规划，不属于框架兼容入口。

## 7. 速查

| 我想… | 怎么做 |
| --- | --- |
| 落地抓到的数据 | 自己的表 + `engine.Upsert` |
| 建/改表 | `models.Models()` 登记 + `papa migrate`（改类型 / 删列 / 回填要自己写 SQL） |
| 记住「这条任务是哪条业务行」 | `papa.Task{Meta: ...}`（随任务行落库） |
| 防重复投递 | 任务的 `(site, stage, identity_hash)` 唯一索引；稳定业务 ID 使用 `ResourceID` |
| 复杂查询 / 事务 | `engine.GetDB()` |
| 查任务运行态 | 后台「任务」表格页（重投 / 加急 / 追踪 / 开停轮询） |

## 调度运行关系

运行治理的当前模型由显式 `papa migrate` 创建：包括 `crawler_config.revision`、`crawler_tasks.account_pool` 与 `crawler_account_pools` / `crawler_account_management`。策略 CAS 版本只以 `crawler_config.revision` 列为权威；`runtime_policy` 是独立键，`sites` 仍是运行快照。账号池表保存成员/当前选择/最近 100 条切换记录，账号管理表保存停用/隔离/冷却，不存凭据，也不复用 `crawler_task_flows`。

任务最近账号与选择版本、最近 8 次认领关联保存在任务行中，不依赖 Trace 开关；这是一段有界关联，不能作为永久身份历史。失败账号的任务由错误队列或人工重投复用原行，数据库结果写入仍必须满足业务幂等性。详细运行生命周期见 [RUNTIME_CONTROL.md](../runtime/RUNTIME_CONTROL.md)。

可选 crawler_task_flows 保存运行策略和修订；crawler_tasks 的 flow_id/flow_lane/flow_branch/flow_path 保存关系。启用 task_flow.enabled 并显式 papa migrate；任务列是本版本共同 schema，默认任务使用空标识。禁止绕过 API 修改/删除关系，详见 [动态阶段调度](../runtime/TASK_FLOW.md)。


熔断运行数据分为当前快照 `crawler_breaker_incidents` 与追加历史 `crawler_breaker_history`，均通过宿主显式迁移创建，普通启动不自动迁移。显式离线 obs 可查询历史表，不用最新快照覆盖过去某天的状态；在线 obs 只接收有限信号，不读这两张表。两表同事务写入，历史主键为事件 ID 与事件内序号。

### 步骤追踪文件存储

Trace 不再参与框架数据库迁移或启动表结构检查。旧 `crawler_task_trace` 表不会自动删除，但也不会回退读取；需要历史 SQL 证据时自行保留原库备份。本次不改变任务主表认领、终态和结算条件。

新执行在成功认领后取得 UUID，同轮重试共用；`errors` 模式保存异常尝试，`all` 保存全部尝试，`off` 关闭。没有手动选择任务的 Trace 模式。执行身份与归档关联保持一致，归档下载路径独立于 Trace。详见 [CORE_CONFIG.md](../configuration/CORE_CONFIG.md) 的 Trace 文件存储。
