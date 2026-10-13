# Papa 数据落地手册

> 面向：写完 fetcher 之后，抓到的内容怎么落地成表、写进数据库、以及写在哪里。
> 配套阅读：[FETCHER_WRITING_GUIDE.md](./FETCHER_WRITING_GUIDE.md)（抓取逻辑怎么写）。

---

## 0. 一句话

框架只管**调度状态**，业务数据**自己建表、自己写**：

- `crawler_tasks` 记的是「这条 URL 属于哪个站点的哪个阶段、跑到哪一步、重试几次、要不要轮询、业务键是什么」——**它没有结果列**。
- 抓到的内容要落库，就在项目的 `models/` 包里定义结构、把它登记进 `models.Models()`，然后用 `engine.Upsert` 写进去（建表走 `papa migrate`）。

> **升级提醒**：v3.3.2 之前框架提供过 `crawler_tasks.title` / `content` 两个结果列与
> `SaveResult` / `SaveContent` / `GetResult` 三个 API，**已全部删除**。迁移步骤见 §6。

---

## 1. 两类数据，两条生命周期

| | 调度状态 | 业务结果 |
| --- | --- | --- |
| 存在哪 | `crawler_tasks`（框架的表） | 你自己的表（`models/` 里定义） |
| 谁写 | 只有引擎 | 只有你（fetcher / 后台动作） |
| 生命周期 | 任务跑完、被清理、被重投都归调度管 | 数据本身长期存在，与任务行无关 |
| 形状 | 固定的几个列 | 一类数据一张表，字段你定 |

把它们分开的好处很实在：业务表能建唯一键、外键、按业务维度建索引，后台能单独给它开一张表格页；而任务行被重投或清理时，业务数据不会跟着遭殃。

---

## 2. 任务表 `crawler_tasks`：业务**只读**，不要写

| 列 | 说明 |
| --- | --- |
| `id` / `p_id` | 任务 ID / 父任务 ID（阶段串联靠它） |
| `url` / `url_hash` | 任务 URL 与它的 sha256（唯一索引用哈希，因为长 URL 整串进不了索引） |
| `stage` / `site` | 属于哪个阶段、哪个站点（`SiteSpec.Key`，**必填** —— 没有「未归属」那一档） |
| `status` / `retry` / `error` | 运行态：待处理 / 处理中 / 成功 / 失败、重试次数、错误信息 |
| `repeatable` / `repeat` / `repeat_interval` / `next_repeat_at` / `last_repeat_at` | 轮询相关，调度用 |
| `reprocess` / `urgent` | 失败重投的代次（同时是后台动作的版本守卫）、加急标记 |
| `idempotency_key` | 去重键，见下 |
| `meta` | **业务键**（JSON），见下 |

- **`meta` 是唯一该由你填的任务列**：给 `papa.Task{Meta: map[string]string{...}}` 赋值即可，它随行落库。
  恢复队列、轮询队列、错误队列重投、后台「重投 / 加急」都按这一列还原任务身份 ——
  不落库的话任务重投一次身份就没了（handler 拿不到业务键，报错还指不到这里）。
  放**键**（`series_id` / `episode_id`），别放数据本身。
- **唯一索引建在 `(url_hash, stage, site)`**：同一个站点的同一个 URL 在同一个阶段只有一行 ——
  三个都在里面是必须的：**阶段名只在本站内唯一**，两个站可以各有自己的 `catalog` 并各自抓同一个 URL。
- **`idempotency_key` 的规范格式是 `site:stage:<标识>`，而且唯一性由数据库兜底**：
  自定义时显式写上（如 `shop:catalog:https://shop.example/books`、`shop:detail:series:5`），
  留空则回退成同形状的 `site:stage:url`。三段缺一不可 —— 少了 `site` / `stage`，
  两个站的同名阶段会撞成同一条任务（B 站被判「已存在」、静默不跑）。
  - **唯一索引建在 `idempotency_hash` 上**（键原文的 sha256，64 位十六进制），不是键原文上 ——
    键里带 URL，`varchar(1000)` 在 utf8mb4 下超过 InnoDB 的 3072 字节键长上限（真库实测 `ERROR 1071`）。
    所以原文只存文本、`idempotency_hash` 才是身份；**同一个自定义键在库里只会有一行**，并发也插不进第二行。
  - **空键落 NULL**（不是空串）：MySQL 的唯一索引把多个 NULL 视作互不相同，落空串等于全表只能有一行。
    NULL 同时也是"没自定义键"的标记，这类行由 `(url_hash, stage, site)` 那条索引管。
  - 键原文上限 1000 字符（第三段用规范化后的短 URL；实测三段的键都在 60 字符内）。
- 其余列由引擎维护，业务**不要直接读写**；后台任务表的「重投 / 加急 / 开停轮询 / 标失败 / 删除」已经覆盖了常见的运维动作。

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
    ID        uint64    `gorm:"primaryKey"`
    Site      string    `gorm:"type:varchar(128);not null;uniqueIndex:ux_series_site_key,priority:1"`
    SourceKey string    `gorm:"type:varchar(191);not null;uniqueIndex:ux_series_site_key,priority:2;comment:站点内稳定标识"`
    Title     string
    CoverURL  string
    Status    int       `gorm:"index;comment:业务自己的状态机"`
    CreatedAt time.Time `gorm:"autoCreateTime"`
    UpdatedAt time.Time `gorm:"autoUpdateTime"`
}
```

建表：项目目录里跑 `papa migrate`（等价于 `make migrate`）；它只增不减，重复跑安全。

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

---

## 5. 常见坑

1. **忘了登记 `models.Models()`** → `papa migrate` 不建表，运行时才报 `Table doesn't exist`。
2. **没建唯一键** → 重投/轮询一轮就多一行；`Upsert` 也会退化成普通插入。
3. **站点维度漏了一处**（提交任务时不带 `site`、自定义幂等键少写 `site:` 那一段、按阶段取配置只给名字）→ 查不到那个阶段，或者两个站的数据互相顶掉。
4. **`meta` 里塞数据** → 它是给"还原任务身份"用的，塞大对象既没索引也不该随任务清理而消失。
5. **业务列名与 gorm 生成的实际列名对不上**（后台表格页 `Field` 写 `pid` 而库里是 `p_id`）→ 后台永远渲染成空，见 [MONITOR.md](./MONITOR.md)。
6. **改了表结构没跑迁移** → `AutoMigrate` 只增不减（不改类型、不删列）；改列类型、删列、回填都得自己写 SQL，见 [CORE_CONFIG.md](./CORE_CONFIG.md) 第 2 节。

---

## 6. 从旧版本升级

**框架不为更早版本的表结构负责。** 升级跑 `papa migrate` 只做 `AutoMigrate`（建表 / 补列 / 补索引），
它**不会**替你删旧索引、改列类型、回填数据或删死列 —— 早先版本里那几段"给旧库补课"的显式迁移
已经删掉了（见框架仓库的 `docs/issues/changes.md`，那份不随脚手架复制到项目里）。

从更早的框架版本升上来的库，只有两条路：

1. **手工对齐**表结构（对着当前的 `models/` 看差在哪：多出来的列/索引、改过类型的列、要回填的值，
   自己写 SQL 或 `ALTER TABLE`；停机窗口里做，先备份）。
2. **重建**库（`papa migrate` 建出当前结构，业务数据自己导）。

新建的库（`papa migrate` 从空库建起来）不受影响，永远是当前结构。

## 7. 速查

| 我想… | 怎么做 |
| --- | --- |
| 落地抓到的数据 | 自己的表 + `engine.Upsert` |
| 建/改表 | `models.Models()` 登记 + `papa migrate`（改类型 / 删列 / 回填要自己写 SQL） |
| 记住「这条任务是哪条业务行」 | `papa.Task{Meta: ...}`（随任务行落库） |
| 防重复投递 | 任务的 `(url_hash, stage, site)` 唯一索引；需要业务语义时设 `IdempotencyKey`（`site:stage:<标识>`） |
| 复杂查询 / 事务 | `engine.GetDB()` |
| 查任务运行态 | 后台「任务」表格页（重投 / 加急 / 追踪 / 开停轮询） |
