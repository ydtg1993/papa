# 将 `models.Episode` 暴露到 Papa 监控后台

## 结论与职责边界

推荐采用 **薄 Table + 业务 Source/Handler**：

- `crawler/models/episode.go`：只放持久化模型和表名；增加“已复核”的持久化状态与并发控制字段。
- `crawler/models/models.go`：只维护迁移模型清单；`Episode` 已登记，无需另建重复模型。
- `crawler/monitor/tables.go`：只声明列、筛选器、默认排序、动作，以及把 Source/Handler 组装进 `oao.Table`；不要在组件或闭包里散落 SQL。
- 建议新增 `crawler/monitor/episode_source.go`：实现 `oao.Source`，负责白名单筛选、白名单排序、分页和读库。
- 建议新增 `crawler/monitor/episode_handler.go`：实现“标记已复核”的业务动作，重新校验 ID，并用带状态/版本条件的 UPDATE 防并发覆盖。
- Papa/oao 组件本身不改：`oao` 只解析和裁剪请求、转发给业务；不能让组件直接查库，也不能信任客户端回传的 `Row`。
- 鉴权与审计复用 Papa 已有装配：oao 路由使用 `mon.Auth`（IP 白名单 + 访问令牌），`server.operation_log: true` 时 `OnAction` 自动记录成功和失败。

这里不建议直接套当前 Papa 内部的 `admin/gormsource`：crawler 的 `go.mod` 依赖发布版 `github.com/ydtg1993/papa/v3`，而 `admin/gormsource` 是 Papa 模块内部实现，业务项目不应反向依赖框架内部包。为 Episode 写一个很小的业务 Source，边界更清楚，也能显式固定 SELECT 列与默认排序。

## 1. models 放在哪里

修改 `crawler/models/episode.go`，保留“一张表一个文件”的现有约定。最小字段建议如下：

```go
package models

import "time"

type Episode struct {
    ID        uint64 `gorm:"primaryKey" json:"id"`
    SeriesID  uint64 `gorm:"not null;index:idx_episodes_series_id;uniqueIndex:ux_episodes_series_no,priority:1" json:"series_id"`
    EpisodeNo uint32 `gorm:"type:int unsigned;not null;uniqueIndex:ux_episodes_series_no,priority:2" json:"episode_no"`
    Title     string `gorm:"type:varchar(512)" json:"title,omitempty"`
    SourceURL string `gorm:"type:varchar(1024);not null" json:"source_url"`
    CoverURL  string `gorm:"type:varchar(2048)" json:"cover_url,omitempty"`
    CoverPath string `gorm:"type:varchar(1024)" json:"cover_path,omitempty"`
    MediaURL  string `gorm:"type:varchar(2048)" json:"media_url,omitempty"`

    // NULL = 尚未复核；非 NULL = 已复核。一个字段即可表达状态和复核时间。
    ReviewedAt *time.Time `gorm:"index:idx_episodes_reviewed_at" json:"reviewed_at,omitempty"`

    // 用于防止“页面打开后数据已变化”的陈旧操作；由业务写入统一递增。
    Version uint64 `gorm:"not null;default:0" json:"version"`

    CreatedAt time.Time `json:"created_at"`
    UpdatedAt time.Time `json:"updated_at"`
}

func (Episode) TableName() string { return "episodes" }
```

`crawler/models/models.go` 当前已有：

```go
func Models() []any { return []any{&Series{}, &Episode{}} }
```

所以无需再注册一次。若不需要检测“查看后其它字段变化”，仅用 `WHERE id = ? AND reviewed_at IS NULL` 也能保证动作幂等和防重复复核；但为了完整满足并发更新保护，推荐保留 `Version`，并让所有更新 Episode 的业务写路径都带版本条件、成功后 `version = version + 1`。

## 2. `monitor/tables.go` 放什么

此文件只做后台元数据声明和依赖装配。骨架：

```go
package monitor

import (
    "github.com/ydtg1993/oao"
    "gorm.io/gorm"
)

func Tables(db *gorm.DB) []oao.Table {
    source := NewEpisodeSource(db)
    actions := NewEpisodeActions(db)

    episode := oao.Table{
        Key:         "episode",
        Label:       "分集",
        Group:       "业务",
        IDField:     "id",
        DefaultSort: "-id",
        PageSize:    20,
        Source:      source,
        Columns: []oao.Column{
            {Field: "id", Label: "ID", Kind: oao.KindNumber, Width: "90px", NoEdit: true},
            {Field: "series_id", Label: "剧集 ID", Kind: oao.KindNumber, Width: "110px", NoEdit: true},
            {Field: "episode_no", Label: "集数", Kind: oao.KindNumber, Width: "80px", NoEdit: true},
            {Field: "title", Label: "标题", NoEdit: true},
            {Field: "source_url", Label: "来源", Render: oao.RenderLink, Href: "{source_url}", NewTab: true, NoSort: true, NoEdit: true},
            {Field: "reviewed_at", Label: "复核时间", Kind: oao.KindTime, NoEdit: true},
            // Hidden 仍会下发，供客户端发起动作时携带；Handler 只把它当乐观锁 token，不能当事实。
            {Field: "version", Kind: oao.KindNumber, Hidden: true, NoEdit: true},
            {Field: "created_at", Label: "创建时间", Kind: oao.KindTime, NoEdit: true},
            {Field: "updated_at", Label: "更新时间", Kind: oao.KindTime, NoEdit: true},
        },
        Filters: []oao.Filter{
            {Field: "series_id", Label: "剧集 ID", Kind: oao.KindNumber, Op: oao.OpEq},
            {Field: "title", Label: "标题", Kind: oao.KindString, Op: oao.OpLike},
        },
        Actions: []oao.Action{
            {
                Key:     "mark_reviewed",
                Label:   "标记已复核",
                Tone:    oao.ToneOK,
                Confirm: "确认将该分集标记为已复核？",
                Handler: actions.MarkReviewed,
            },
        },
    }

    return []oao.Table{episode}
}
```

`DefaultSort: "-id"` 会由前端作为默认查询排序发送，但 Source 仍必须在 `q.SortFields()` 为空时自行执行 `ORDER BY id DESC`，避免绕过前端或旧客户端时分页顺序不稳定。

## 3. Source 放在哪里、如何实现

建议新建 `crawler/monitor/episode_source.go`。Source 属于业务接入层：它可以依赖 GORM 和 models，但 oao 组件不能反过来依赖数据库。

```go
package monitor

import (
    "context"
    "strings"

    "crawler/models"
    "github.com/ydtg1993/oao"
    "gorm.io/gorm"
)

type EpisodeSource struct{ db *gorm.DB }

func NewEpisodeSource(db *gorm.DB) *EpisodeSource {
    return &EpisodeSource{db: db}
}

func (s *EpisodeSource) List(ctx context.Context, q oao.Query) ([]map[string]any, int64, error) {
    applyFilters := func(db *gorm.DB) *gorm.DB {
        if f := q.Get("series_id"); !f.Empty() {
            if id, ok := f.Int(); ok && id > 0 {
                db = db.Where("series_id = ?", id)
            }
        }
        if f := q.Get("title"); !f.Empty() {
            // 值参数化；列名是服务端常量，不采用客户端字段名。
            db = db.Where("title LIKE ?", "%"+strings.TrimSpace(f.String())+"%")
        }
        return db
    }

    base := s.db.WithContext(ctx).Model(&models.Episode{})
    var total int64
    if err := applyFilters(base).Count(&total).Error; err != nil {
        return nil, 0, err
    }

    db := applyFilters(base).Select([]string{
        "id", "series_id", "episode_no", "title", "source_url",
        "reviewed_at", "version", "created_at", "updated_at",
    })

    sorts := q.SortFields() // oao 已按声明列做过白名单过滤
    if len(sorts) == 0 {
        db = db.Order("id DESC")
    } else {
        // 不直接拼 q.Sort；再用服务端映射，防止未来声明变化造成 SQL 注入面。
        allowed := map[string]string{
            "id": "id", "series_id": "series_id", "episode_no": "episode_no",
            "title": "title", "reviewed_at": "reviewed_at",
            "created_at": "created_at", "updated_at": "updated_at", "version": "version",
        }
        for _, sf := range sorts {
            col, ok := allowed[sf.Field]
            if !ok {
                continue
            }
            dir := " ASC"
            if sf.Desc {
                dir = " DESC"
            }
            db = db.Order(col + dir)
        }
    }
    // id 作为最终 tie-breaker，避免同值跨页漂移。
    db = db.Order("id DESC")

    rows := []map[string]any{}
    err := db.Offset((q.Page - 1) * q.Size).Limit(q.Size).Find(&rows).Error
    return rows, total, err
}
```

注意：如果显式排序本身包含 `id`，可避免再追加第二个 `id DESC`；生产实现可用一个布尔变量判断。上述骨架重点是：筛选和排序列来自服务端白名单，值全部参数绑定；组件只传规范 Query，不查库。

标题筛选既可以仅使用 `Filters`，也可以额外配置 Source 的 `q.Search` 为标题全局搜索。但需求明确是“按标题筛选”，以 `filter[title]` 为主更清晰，无需扩大搜索语义。

## 4. Handler 放在哪里、如何实现

建议新建 `crawler/monitor/episode_handler.go`。Handler 必须只信任：

1. `req.ID` 经服务端严格解析后的主键；
2. `req.Row["version"]` 仅作为客户端持有的乐观锁 token；
3. 数据库条件和 `RowsAffected` 才是最终事实。

不要从客户端 Row 读取 `reviewed_at`、`series_id` 或标题来决定能否更新；Row 可被任意伪造。

```go
package monitor

import (
    "context"
    "net/http"
    "strconv"
    "strings"
    "time"

    "crawler/models"
    "github.com/ydtg1993/oao"
    "gorm.io/gorm"
)

type EpisodeActions struct{ db *gorm.DB }

func NewEpisodeActions(db *gorm.DB) *EpisodeActions {
    return &EpisodeActions{db: db}
}

func (a *EpisodeActions) MarkReviewed(ctx context.Context, req oao.ActionRequest) error {
    id, err := strconv.ParseUint(strings.TrimSpace(req.ID), 10, 64)
    if err != nil || id == 0 {
        return oao.Fail(http.StatusBadRequest, "分集 ID 非法")
    }

    // Row 不可信，只允许把 version 当成 CAS token；必须严格解析，缺失就拒绝。
    rawVersion := req.RowString("version")
    version, err := strconv.ParseUint(rawVersion, 10, 64)
    if err != nil {
        return oao.Fail(http.StatusBadRequest, "页面数据已过期，请刷新后重试")
    }

    now := time.Now()
    result := a.db.WithContext(ctx).
        Model(&models.Episode{}).
        Where("id = ? AND reviewed_at IS NULL AND version = ?", id, version).
        Updates(map[string]any{
            "reviewed_at": now,
            "version":     gorm.Expr("version + 1"),
            "updated_at":  now,
        })
    if result.Error != nil {
        return result.Error // oao 对未知内部错误统一返回 500，不泄露 SQL 细节
    }
    if result.RowsAffected == 1 {
        return nil
    }

    // 0 行可能是不存在、已复核，或版本冲突；重新查库给出安全且准确的业务错误。
    var current struct {
        ReviewedAt *time.Time
        Version    uint64
    }
    err = a.db.WithContext(ctx).Model(&models.Episode{}).
        Select("reviewed_at", "version").
        Where("id = ?", id).
        Take(&current).Error
    if err == gorm.ErrRecordNotFound {
        return oao.Fail(http.StatusNotFound, "分集不存在")
    }
    if err != nil {
        return err
    }
    if current.ReviewedAt != nil {
        return oao.Fail(http.StatusConflict, "该分集已复核，请刷新列表")
    }
    return oao.Fail(http.StatusConflict, "该分集已被其他操作修改，请刷新后重试")
}
```

核心 CAS 是：

```sql
UPDATE episodes
SET reviewed_at = ?, version = version + 1, updated_at = ?
WHERE id = ? AND reviewed_at IS NULL AND version = ?;
```

只有 `RowsAffected == 1` 才算成功。这样可防重复点击、两个管理员同时操作，以及页面陈旧导致的覆盖。若业务希望“重复标记已复核”是幂等成功，也仍要通过数据库条件判断；可在查到 `reviewed_at != NULL` 后返回 nil，但审计会记作成功，语义需提前约定。推荐返回 409，让并发冲突可见。

另外，仓库其他写 Episode 的逻辑也应统一 CAS；若抓取更新仍无条件保存并不递增 `version`，这个版本字段不能完整检测并发变化。

## 5. 迁移放在哪里、怎么跑

迁移入口已经存在：

- `monitor.Register(app)` 中的 `app.UseModels(models.Models()...)` 注册业务模型；
- `main.go -migrate` 调用 `app.Migrate()`；
- 启动服务不会自动迁移。

因此：

1. 在 `models.Episode` 增加 `ReviewedAt`、`Version`；
2. 保持 `models.Models()` 中 `&Episode{}`；
3. 部署前执行 `make migrate`、`go run . -migrate`，或在项目目录执行 `papa migrate`；
4. 先迁移，再发布引用新列的应用版本。

GORM `AutoMigrate` 会新增列/索引，但不负责复杂回填、删除或危险类型变更。`version` 使用 `not null;default:0`，现有行可安全初始化为 0；`reviewed_at` 为 NULL 表示未复核。生产数据库仍应在预发验证生成 SQL，并做好备份/回滚计划。

如团队要求显式 SQL 迁移，应该放在 crawler 自己的迁移目录/迁移机制中，而不是放进 `oao` 或 `monitor/tables.go`；当前仓库约定是 `models.Models + app.Migrate`，优先沿用现有机制。

## 6. 鉴权放在哪里

无需给 Episode 单独写 HTTP 鉴权。Papa 在 `internal/app` 组装 oao 时已有：

```go
cfgOao := oao.Config{
    Tables: tables,
    Logger: app.Logger.Sys,
    Auth:   mon.Auth,
}
```

`mon.Auth` 复用监控后台的 **IP 白名单 + 数据库访问令牌**；访问令牌还把操作人写进 request context。Episode 的列表和动作路由都会经过这层。

业务侧要做的是：

- 保持表格动作走 oao 路由，不要另开 `NoAuth()` 接口；
- 至少创建一条访问令牌并绑定 operator。配置注释明确：令牌表为空时，白名单内访问会开放，这不适合生产；
- `server.whitelist` / `whitelist_file` 配置最小可信网段；
- 如未来需要“可查看但不可复核”的细粒度 RBAC，应在 Handler 中读取鉴权上下文中的主体/权限并拒绝 403，而不是相信前端隐藏按钮。当前代码只提供 operator，没有现成角色模型，不应假装已有 RBAC。

## 7. 审计放在哪里

不在 Episode Handler 里手写审计表。配置：

```yaml
server:
  operation_log: true
```

Papa 会创建 `oplog.Recorder`，并把：

```go
cfgOao.OnAction = func(ev oao.ActionEvent) {
    rec.Record(ev, operatorOf(ev))
}
```

接到 oao。`handleAction` 对成功和失败都会触发 `OnAction`，记录：表 key、动作 key、行 ID、声明过的 Values、结果/错误、操作人、直连 IP、时间。对应事件会是：

- table: `episode`
- action: `mark_reviewed`
- row_id: Episode ID
- operator: 由访问令牌解析
- ok/error: Handler 的真实结果

这个动作没有表单，`Values` 为空是正常的；不要把客户端 `Row` 写入审计，因为 Row 不可信，且可能扩大敏感数据暴露。若审计必须包含服务端确定的复核时间，可扩展业务审计事件或在成功后写专用领域审计，但要和状态更新放在同一事务/outbox 语义中；现有通用 operation log 已足以审计“谁对哪行执行了什么以及是否成功”。

需要理解现有审计是异步旁路：Recorder 写库失败时会降级到日志，且审计失败不会把业务动作改判失败。这适合后台操作日志；若“复核记录”是合规强一致数据，应把 reviewer/reviewed_at 作为 Episode 领域状态在同一事务内更新，不能只依赖旁路 operation log。

## 8. 建议测试清单

### Source

- 无筛选时按 `id DESC`，分页稳定；相同排序值时以 id 做 tie-breaker。
- `series_id` 仅接受合法正整数，生成参数化等值条件。
- `title` 使用参数化 `LIKE`；带引号、`%`、`_` 的语义按产品约定测试（若要求字面包含，需额外转义通配符）。
- 未声明 filter/sort 不进入 SQL。
- Count 与 List 使用相同筛选条件。
- 只 SELECT 后台声明需要的列，不暴露 `media_url` 等未声明字段。

### Handler

- 合法未复核行：更新 1 行并递增 version。
- 非法/空 ID：400。
- 不存在：404。
- 已复核：409。
- 两个请求携带同一 version：仅一个成功，另一个 409。
- Row 伪造 `reviewed_at`、`series_id`、标题不影响业务判断。
- 缺失或伪造 version：400/409，不能无条件更新。
- 数据库错误不向客户端泄露 SQL，只返回通用 500，同时服务端日志保留错误。

### 接线、安全与审计

- `Tables(db)` 返回 episode 表且默认排序为 `-id`。
- 无白名单/令牌的请求被鉴权层拒绝。
- 动作成功和冲突失败均产生 operation log，operator 来自令牌上下文。
- `operation_log: false` 时业务动作仍正常，但明确没有审计页/审计入库。
- 迁移后旧数据 `version=0`、`reviewed_at=NULL`，动作可正常执行。

## 最终文件布局

```text
crawler/
├─ models/
│  ├─ episode.go              # Episode 表结构：ReviewedAt、Version
│  └─ models.go               # 迁移清单，保留 &Episode{}
└─ monitor/
   ├─ tables.go               # 纯 Table/Column/Filter/Action 声明与装配
   ├─ episode_source.go       # oao.Source：查询、筛选、排序、分页
   ├─ episode_handler.go      # 动作业务逻辑：ID 校验 + 条件更新 + RowsAffected
   └─ register.go             # 现有 UseModels / UseTables 接线，无需堆业务逻辑
```

该方案满足三条关键边界：**oao 组件不直接查库；Handler 不把客户端 Row 当事实；并发写由数据库中的状态/版本条件和 `RowsAffected` 保护。**
