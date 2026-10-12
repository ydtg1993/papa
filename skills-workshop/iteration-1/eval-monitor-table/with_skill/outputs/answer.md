# 将 `models.Episode` 暴露到 Papa 监控后台：实现骨架

## 结论与职责边界

建议采用 **Papa 导出的 `admin/gormsource` 做只读列表 Source + `models` 包仓储方法做条件写入 + `monitor` 包 Handler 做 HTTP/oao 错误映射**。

数据流如下：

```text
浏览器
  -> Papa Monitor 鉴权（IP/CIDR + Access Token）
  -> oao（校验已声明的表、列、筛选、排序、动作；只负责声明和透传）
  -> monitor 的 Source / Action Handler（适配层）
  -> models 仓储方法（查询/业务状态迁移/条件 UPDATE）
  -> MySQL

动作结束（成功或失败）
  -> Papa 注入的 oao.OnAction
  -> crawler_operation_log（operation_log 开启时）
```

必须守住三条边界：

1. **oao 组件不直接查库**：`Table.Source` 指向业务 Source；本项目可复用 Papa 的 `admin/gormsource`，它是宿主侧业务适配器，不是 oao 组件本身。
2. **不信任 `ActionRequest.Row`**：动作只从 `req.ID` 取稳定主键；是否已复核由服务端 SQL 条件判断，不能按客户端回传的 `row.reviewed` 决策。
3. **并发由业务写条件保护**：执行单条原子语句 `UPDATE ... WHERE id=? AND reviewed=false`，并检查 `RowsAffected`。不能用“先查后改”，否则有 TOCTOU 竞态。

---

## 文件放置

| 关注点 | 放置位置 | 说明 |
|---|---|---|
| 表结构 | `crawler/models/episode.go` | 给 `Episode` 增加复核状态字段；模型属于抓取与后台共享的业务数据层，不放 `monitor` |
| 建表清单 | `crawler/models/models.go` | `Episode` 已在 `Models()` 中，无需新增注册项 |
| 条件更新仓储方法 | 建议新建 `crawler/models/episode_repository.go`；也可暂放现有 `repository.go` | 接收 `*gorm.DB`，执行带业务前置条件的 UPDATE，并返回可识别的哨兵错误 |
| 表格声明 | `crawler/monitor/tables.go` | 只声明列、筛选、默认排序、动作和 Source 装配 |
| 列表 Source | 复用 `github.com/ydtg1993/papa/v3/admin/gormsource`，在 `tables.go` 中装配 | 它按 oao 已声明字段执行筛选和排序；若以后查询需要 JOIN/派生列，再新建 `monitor/episode_source.go` 实现 `oao.Source` |
| 动作 Handler | 建议新建 `crawler/monitor/episode_actions.go` | 解析 `req.ID`、调用 models 仓储方法、把业务错误映射为 `oao.Fail`；不自行信任 Row 或拼任意客户端字段 |
| 注册入口 | `crawler/monitor/register.go` | 已有 `app.UseModels(models.Models()...)` 与 `app.UseTables(Tables(app.DB)...)`，一般无需改 |
| 显式迁移入口 | `crawler/main.go` 已存在 | 保持 `monitor.Register(app)` 在 `-migrate` 分支之前，执行 `papa migrate`/`go run . -migrate`，不要在常规启动时 `AutoMigrate` |
| 鉴权 | Papa Monitor 壳层，配置在 `crawler/configs/config.yaml` 的 `server` 段 | `/api/oao/*` 自动经过 IP/CIDR 白名单与 Access Token；不要为该动作创建 `NoAuth` 路由 |
| 审计 | Papa 的中央 `oao.OnAction` + `crawler_operation_log` | `server.operation_log: true` 时，`app.UseTables` 注册的动作无论成功失败均被记录；不要在 Handler 重复写审计 |

> 当前工程使用 Papa v3.1.0、oao v1.3.0，`monitor.Register(app)` 已在显式迁移分支之前调用，现有注册顺序正确。

---

## 1. Models：新增复核状态

最小需求只需要布尔字段。`reviewed=false` 表示未复核，动作只允许 `false -> true`。建议字段名直接使用 `Reviewed`，GORM 列名为 `reviewed`。

`crawler/models/episode.go` 骨架：

```go
package models

import "time"

// Episode 是一部短剧中的独立分集。
type Episode struct {
    ID        uint64    `gorm:"primaryKey" json:"id"`
    SeriesID  uint64    `gorm:"not null;index:idx_episodes_series_id;uniqueIndex:ux_episodes_series_no,priority:1" json:"series_id"`
    EpisodeNo uint32    `gorm:"type:int unsigned;not null;uniqueIndex:ux_episodes_series_no,priority:2" json:"episode_no"`
    Title     string    `gorm:"type:varchar(512)" json:"title,omitempty"`
    SourceURL string    `gorm:"type:varchar(1024);not null" json:"source_url"`
    CoverURL  string    `gorm:"type:varchar(2048)" json:"cover_url,omitempty"`
    CoverPath string    `gorm:"type:varchar(1024)" json:"cover_path,omitempty"`
    MediaURL  string    `gorm:"type:varchar(2048)" json:"media_url,omitempty"`

    // 只允许后台动作执行 false -> true；抓取 upsert 不应更新此列。
    Reviewed  bool      `gorm:"not null;default:false;index:idx_episodes_reviewed" json:"reviewed"`

    CreatedAt time.Time `json:"created_at"`
    UpdatedAt time.Time `json:"updated_at"`
}

func (Episode) TableName() string { return "episodes" }
```

说明：

- `Reviewed` 不应加入 `UpsertEpisodeDirectory` 或 `UpsertEpisodeMedia` 的更新列，否则重抓可能把人工复核结果覆盖回去。当前两个 upsert 都显式列出更新字段，因此新增字段后默认不会被抓取流程改写。
- `index:idx_episodes_reviewed` 对本需求不是硬性要求；如果后台只按 series/title 查询且数据量小，可不建。若后续经常查看“未复核”，保留索引更合适。
- 如果业务还要求记录“谁、何时复核”，应增加 `ReviewedAt *time.Time`、`ReviewedBy string`。`ReviewedBy` 必须从服务端鉴权上下文获取，不能来自 Action 表单；但这超出当前最小需求，中央操作日志已能记录操作者与动作。

`crawler/models/models.go` 已经是：

```go
func Models() []any { return []any{&Series{}, &Episode{}} }
```

因此不需要重复注册模型。

---

## 2. Models 仓储：原子条件更新

建议新建 `crawler/models/episode_repository.go`，让 SQL 与业务状态迁移留在数据层，而不是散落在 `monitor/tables.go`。

```go
package models

import (
    "context"
    "errors"

    "gorm.io/gorm"
)

var (
    ErrEpisodeNotFound        = errors.New("episode not found")
    ErrEpisodeAlreadyReviewed = errors.New("episode already reviewed")
)

// MarkEpisodeReviewed 将分集从未复核原子地迁移为已复核。
// WHERE 中的 reviewed = false 是最终并发保护：重复点击、多人同时操作、请求重放，
// 只有一个请求能影响一行。
func MarkEpisodeReviewed(ctx context.Context, db *gorm.DB, id uint64) error {
    tx := db.WithContext(ctx).
        Model(&Episode{}).
        Where("id = ? AND reviewed = ?", id, false).
        Update("reviewed", true)
    if tx.Error != nil {
        return tx.Error
    }
    if tx.RowsAffected == 1 {
        return nil
    }

    // 条件更新为 0 后才做只读诊断，仅用于区分 404 与 409；
    // 正确性不依赖这次查询，真正的并发保护已经在上面的 UPDATE 内完成。
    var count int64
    if err := db.WithContext(ctx).
        Model(&Episode{}).
        Where("id = ?", id).
        Count(&count).Error; err != nil {
        return err
    }
    if count == 0 {
        return ErrEpisodeNotFound
    }
    return ErrEpisodeAlreadyReviewed
}
```

这里刻意没有：

```go
// 错误示例：存在竞态窗口
var ep Episode
db.First(&ep, id)
if !ep.Reviewed {
    db.Model(&ep).Update("reviewed", true)
}
```

如果不在意区分 404/409，也可在 `RowsAffected == 0` 时统一返回 `ErrEpisodeAlreadyReviewed`/`ErrEpisodeChanged`，从而少一次诊断查询。

---

## 3. Source：声明驱动的查询

当前需求只是单表分页、`series_id = ?`、标题模糊匹配、按声明字段排序，Papa 的 `admin/gormsource` 正好适用。它实现 `oao.Source`，根据 `q.Filters()` 和 `q.SortFields()` 生成查询；oao 本身仍然不查库。

无需自建 Source 类型，直接在 `Tables` 装配：

```go
Source: gormsource.New(gormsource.Config{
    DB:     db,
    Model:  &models.Episode{},
    Search: []string{"title"}, // 可选：让顶部全局搜索也搜标题
}),
```

筛选语义由 `oao.Filter` 声明：

- `series_id`：`KindNumber + OpEq`，最终是参数绑定的等值条件；已有 `idx_episodes_series_id` 索引。
- `title`：`OpLike`，最终是 `%关键字%`。它满足“标题筛选”，但前导 `%` 通常不能走普通 B-tree 索引；数据量很大时需另评估全文索引/搜索服务，不能把普通 title 索引当成优化方案。
- 默认排序：`DefaultSort: "-id"`。

如果将来需要 JOIN `series.title`、租户隔离、复杂权限或派生字段，再新建 `crawler/monitor/episode_source.go`，显式实现：

```go
type episodeSource struct{ db *gorm.DB }

func (s episodeSource) List(
    ctx context.Context,
    q oao.Query,
) (rows []map[string]any, total int64, err error) {
    // 只解释声明过的 q.Filters()/q.SortFields()；
    // 所有值参数绑定，字段名只来自服务端白名单声明。
    panic("custom query skeleton")
}
```

当前不需要为简单查询重复实现一套。

---

## 4. Handler：只信 ID，调用业务条件写

建议新建 `crawler/monitor/episode_actions.go`：

```go
package monitor

import (
    "context"
    "errors"
    "net/http"
    "strconv"

    "crawler/models"
    "github.com/ydtg1993/oao"
    "gorm.io/gorm"
)

func markEpisodeReviewedHandler(db *gorm.DB) oao.ActionHandler {
    return func(ctx context.Context, req oao.ActionRequest) error {
        // req.ID 是定位键，也仍须服务端校验格式与范围。
        id, err := strconv.ParseUint(req.ID, 10, 64)
        if err != nil || id == 0 {
            return oao.Fail(http.StatusBadRequest, "无效的分集 ID：%q", req.ID)
        }

        // 不读取 req.Row["reviewed"]，也不读取客户端传来的任意 Values。
        // 操作方向由服务端动作定义固定为 false -> true。
        err = models.MarkEpisodeReviewed(ctx, db, id)
        switch {
        case err == nil:
            return nil
        case errors.Is(err, models.ErrEpisodeNotFound):
            return oao.Fail(http.StatusNotFound, "分集不存在，可能已被删除")
        case errors.Is(err, models.ErrEpisodeAlreadyReviewed):
            return oao.Fail(http.StatusConflict, "该分集已复核，请刷新后重试")
        default:
            // 未知内部错误原样交给 oao：前端只得到统一 500，避免泄露 SQL/库结构；
            // Papa 的 OnAction 仍会把失败记入审计。
            return err
        }
    }
}
```

> 若当前 oao 版本没有导出的 `oao.ActionHandler` 命名类型，就把工厂返回类型写成 `func(context.Context, oao.ActionRequest) error`；赋给 `Action.Handler` 的函数签名不变。

为什么不使用 `req.Row` 作为版本条件？对于“标记已复核”这个单向状态迁移，服务端已知唯一合法前置状态就是 `reviewed=false`，直接把它写入 `WHERE` 比客户端 Row 更可靠。Row 可被伪造且可能过期，不能用来证明当前状态。

---

## 5. `monitor/tables.go`：表、筛选、默认排序与动作

完整骨架：

```go
package monitor

import (
    "crawler/models"

    "github.com/ydtg1993/oao"
    "github.com/ydtg1993/papa/v3/admin/gormsource"
    "gorm.io/gorm"
)

func Tables(db *gorm.DB) []oao.Table {
    episode := oao.Table{
        Key:   "episode",
        Label: "分集",
        Group: "业务",
        Source: gormsource.New(gormsource.Config{
            DB:     db,
            Model:  &models.Episode{},
            Search: []string{"title"},
        }),
        Columns: []oao.Column{
            // 动作依赖默认 IDField="id"；主键列必须声明，否则前端拿不到 req.ID。
            {Field: "id", Label: "ID", Kind: oao.KindNumber, Width: "80px", NoEdit: true},
            {Field: "series_id", Label: "剧集 ID", Kind: oao.KindNumber, Width: "100px", NoEdit: true},
            {Field: "episode_no", Label: "集数", Kind: oao.KindNumber, Width: "80px", NoEdit: true},
            {Field: "title", Label: "标题"},
            {Field: "source_url", Label: "来源", Render: oao.RenderLink, Href: "{source_url}", NewTab: true, NoSort: true, NoEdit: true},
            {Field: "media_url", Label: "媒体地址", Render: oao.RenderLink, Href: "{media_url}", NewTab: true, NoSort: true, NoEdit: true},
            {Field: "reviewed", Label: "已复核", Kind: oao.KindBool, Width: "90px", NoEdit: true},
            {Field: "created_at", Label: "创建时间", Kind: oao.KindTime, NoEdit: true},
            {Field: "updated_at", Label: "更新时间", Kind: oao.KindTime, NoEdit: true},
        },
        Filters: []oao.Filter{
            {Field: "series_id", Label: "剧集 ID", Kind: oao.KindNumber, Op: oao.OpEq},
            {Field: "title", Label: "标题", Op: oao.OpLike},
            // 非题目硬性要求，但通常便于运营确认结果；不需要可删除。
            {Field: "reviewed", Label: "复核状态", Kind: oao.KindBool, Op: oao.OpEq},
        },
        DefaultSort: "-id",
        PageSize:    20,
        Actions: []oao.Action{
            {
                Key:     "mark_reviewed",
                Label:   "标记已复核",
                Tone:    oao.ToneOK,
                Confirm: "确认将该分集标记为已复核？此操作不能重复执行。",
                Handler: markEpisodeReviewedHandler(db),
            },
        },
    }

    return []oao.Table{episode}
}
```

注意：

- `id` 必须出现在 `Columns`；不想显示可以加 `Hidden: true`，但不能不声明。
- `reviewed` 作为展示列是必要的，否则操作者看不到状态；动作正确性依然不能依赖这份浏览器快照。
- 只声明允许筛选、排序和下发的字段。oao 会剥离未声明列、筛选和表单值，形成服务端白名单。
- `SourceURL`、`MediaURL` 等不是题目硬性要求，可按后台实际需要删减，以减少下发数据。
- 不应使用通用 `EditAction` 暴露整行编辑；本需求只有受约束的单向业务动作。

`crawler/monitor/register.go` 已经正确装配：

```go
func Register(app *papa.App) {
    app.UseModels(models.Models()...)
    app.UseTables(Tables(app.DB)...)
    view.Register(app)
    Routes(app)
}
```

无需为 oao 动作额外添加自定义 Route/Controller。只有自定义页面或非 oao API 才应走 `router.go` + `controller/`。

---

## 6. 迁移

这是已有表的增量列变更：

1. 修改模型后，在 `crawler` 业务模块目录运行：

   ```text
   papa migrate
   ```

   等价入口是：

   ```text
   go run . -migrate
   ```

2. 该命令会经现有 `main.go` 调用 `monitor.Register(app)`，由 `app.UseModels(models.Models()...)` 注册 `Episode`，再执行 `app.Migrate()`。
3. 正常 `go run .` **不会自动迁移**；不要在常规启动路径调用 `AutoMigrate`。
4. GORM `AutoMigrate` 适合增加列/索引，不负责破坏性迁移。生产环境应先备份、审查 SQL，并在上线应用前完成迁移。
5. 若 `server.operation_log: true`，还要确保迁移已经创建 `crawler_operation_log`；当前配置已开启，但新环境仍需执行一次迁移。

对于已有数据，`NOT NULL DEFAULT false` 会把历史 Episode 视为“未复核”，这是本设计的明确语义。如果业务希望历史数据默认已复核，应另写显式数据回填，不能隐含在动作里。

---

## 7. 鉴权

不在业务 Handler 里重复实现 Token 校验，也不要创建 `NoAuth` 路由。

- `app.UseTables` 挂载的 `/api/oao/*` 由 Papa Monitor 壳层统一保护。
- 请求先经过 `server.whitelist` / `whitelist_file` 的 IP/CIDR 校验，再校验数据库中的 Access Token。
- 浏览器请求通过 `Authorization: Bearer <token>` 或 `X-Auth-Key: <token>`；不要把令牌放进 URL、Action values 或日志详情。
- 每个运维人员应有独立 token，`crawler_access_token.operator` 才能为审计提供稳定操作者身份。
- 特别注意 Papa 的既有语义：令牌表为空时，白名单内 `/api/*` 会开放并在启动时告警；生产环境必须至少创建一个启用的访问令牌，并配置收敛的白名单。
- `markEpisodeReviewedHandler` 只做业务参数/状态校验；请求是否有权抵达动作由 Papa 统一鉴权。若未来需要“只有审核员角色可执行”的细粒度授权，应在 Handler 从 `req.Req.Context()` 获取服务端认证身份并校验服务端角色，绝不能接收客户端声明的角色。

---

## 8. 审计

当前 `crawler/configs/config.yaml` 已设置：

```yaml
server:
  operation_log: true
```

因此 `app.UseTables` 注册的 `mark_reviewed` 动作会由 Papa 的中央 `OnAction` 钩子审计：

- 成功与失败都会记录；
- `table` 为 `episode`；
- `action` 为 `mark_reviewed`；
- 目标 ID、操作者、来源 IP、时间、错误等由宿主记录；
- 操作者来自通过鉴权的 Access Token，而不是客户端 Row；
- Handler 不应再单独写一份操作日志，以免重复、身份口径不一致或发生“业务成功但手写审计失败”的耦合。

若操作日志表尚未建立，运行一次显式迁移。不要把明文 token、媒体密钥或其他秘密塞进 Action values/审计详情。本动作没有表单值，审计面也更小。

如果业务法规要求把“最后复核人/时间”作为 Episode 的业务状态长期保存（而非只作为审计事件查询），再增加 `reviewed_at`、`reviewed_by`，并在同一条受条件保护的 UPDATE 中一起写入；`reviewed_by` 必须取自服务端认证上下文。

---

## 9. 建议测试

至少覆盖以下行为：

### Models 仓储测试

1. 未复核 Episode：第一次调用成功，`reviewed=true`。
2. 同一 ID 第二次调用：返回 `ErrEpisodeAlreadyReviewed`，影响行数为 0。
3. 不存在 ID：返回 `ErrEpisodeNotFound`。
4. 两个 goroutine/两个独立数据库连接同时标记：恰好一个成功，另一个冲突。
5. 抓取 upsert 后：已经为 true 的 `reviewed` 不被覆盖。

### Monitor 声明/Handler 测试

1. `episode` 表包含 `id`、`series_id`、`title`、`reviewed` 列。
2. 筛选仅声明 `series_id` 等值与 `title` 模糊；默认排序是 `-id`。
3. 非数字、0 ID 返回 400。
4. 已复核返回 409，不存在返回 404，数据库异常保持内部 500。
5. 伪造 `req.Row.reviewed=false/true` 都不能改变结果，证明 Handler 不信任 Row。
6. `gormsource` 生成的列表按 series_id/title 过滤并按 id 倒序。

建议从 `crawler` 模块运行：

```text
go test ./models/... ./monitor/...
go test ./...
go vet ./...
```

有可用数据库时再运行：

```text
papa check . --boot
```

无数据库的声明检查可先运行 `papa check .`。当前 `configs/config.yaml` 仍含框架文档标记为已移除的 `scheduler:` 旧配置；若严格配置检查失败，应把它作为已有配置漂移单独处理，不应为本监控功能把旧 scheduler 语义带回实现。

---

## 最终核对清单

- [ ] `Episode` 新增 `Reviewed bool`，且爬虫 upsert 不更新它。
- [ ] `models.Models()` 继续注册 `&Episode{}`。
- [ ] `tables.go` 声明 `series_id` 等值筛选、`title` 模糊筛选和 `DefaultSort: "-id"`。
- [ ] `id` 在 Columns 中声明，动作能取得稳定主键。
- [ ] Source 是业务侧 Source（此处复用 `admin/gormsource`），oao 不直接查库。
- [ ] Handler 只解析 `req.ID`，不信任 `req.Row` 或任意客户端 Values。
- [ ] 仓储使用 `WHERE id=? AND reviewed=false` 并检查 `RowsAffected`。
- [ ] 重复/并发动作返回 409，不存在返回 404。
- [ ] 通过现有 `monitor.Register -> app.UseTables` 接入，不新增无必要的自定义路由。
- [ ] 显式执行迁移，不在正常启动时自动迁移。
- [ ] 保持 Papa `/api/*` 的统一鉴权，不使用 `NoAuth`。
- [ ] 保持 `server.operation_log: true`，让中央 OnAction 审计成功和失败动作，不重复手写审计。
