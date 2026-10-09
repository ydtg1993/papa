package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type Task struct {
	ID             int    `json:"id"`  // 数据库记录 ID
	PID            int    `json:"pid"` // 父级任务ID
	URL            string // 要打开的 URL
	Retry          int
	Stage          string // 阶段标识，如 "catalog", "detail", "video"
	Site           string // 所属站点（SiteSpec.Key）；空 = 未归属。随行落库，供后台按站点筛/排障
	Repeatable     bool
	Meta           map[string]string // 业务键（如 series_id/episode_id），与 URL 解耦
	IdempotencyKey string            // 自定义幂等键，为空时回退 Stage|URL
	NotBefore      time.Time         // 延迟投递：最早可执行时间
	Delay          time.Duration     // 延迟投递：相对当前时间的延迟
	Urgent         bool              // 加急：投到所属阶段的快车道，插到常规队列前面

	// Trace 本次尝试的步骤记录器，由 worker 在调用 FetchHandler 前挂上。
	// 追踪未开启时为 nil，Step/Fail 是安全的 no-op。handler 无需判空。
	Trace *Trace
}

// deliverAt 返回任务的延迟投递时间；无延迟时返回零值 time.Time。
func (t *Task) deliverAt() time.Time {
	if !t.NotBefore.IsZero() {
		return t.NotBefore
	}
	if t.Delay > 0 {
		return time.Now().Add(t.Delay)
	}
	return time.Time{}
}

func (t *Task) IncRetry(db *gorm.DB) {
	t.Retry++
	db.Model(&models.CrawlerTask{}).Where("id = ?", t.ID).
		Update("retry", gorm.Expr("retry + ?", 1))
}

// toModel 将任务转换为数据库记录（不含 ID，由数据库生成）。
func (t *Task) toModel() models.CrawlerTask {
	repeat := models.RepeatableNo
	if t.Repeatable {
		repeat = models.RepeatableYes
	}
	return models.CrawlerTask{
		PID:            uint(t.PID),
		URL:            t.URL,
		Stage:          t.Stage,
		Site:           t.Site,
		IdempotencyKey: t.IdempotencyKey,
		Meta:           metaToJSON(t.Meta),
		Repeatable:     repeat,
		Urgent:         t.Urgent,
		Status:         models.TaskStatusPending,
	}
}

// metaToJSON 把业务键序列化成 meta 列的值；空 Meta 落 NULL（不是 `null` 这个字面量）。
//
// map[string]string 编不出来失败的情形（没有 chan/func/NaN 这类值），故忽略错误。
// 走 marshalNoHTMLEscape 与 content / trace 的 JSON 列保持一致（业务键里也可能带 & 或 <>）。
func metaToJSON(m map[string]string) datatypes.JSON {
	if len(m) == 0 {
		return nil
	}
	b, _ := marshalNoHTMLEscape(m)
	return datatypes.JSON(b)
}

// metaFromJSON 解出 meta 列；空列（NULL / 空串）、`null`、空对象都返回 nil。
//
// **空列不算"解不出来"**：老行从来没写过 Meta，不带 Meta 的任务也是合法的 —— 它们占绝大多数，
// 每次恢复/重投都刷一条 Warn 只会把日志淹掉（实测：启动恢复一次就为几条老行各刷一条）。
//
// 真正的坏值只可能来自框架之外（这一列只有 toModel 一处写）—— 那才不静默：按无 Meta 继续，
// 但留一条 Warn，否则 handler 报的是"缺少 series_id"这类业务语义的错，排查方向全在业务侧。
func (e *Engine) metaFromJSON(raw datatypes.JSON) map[string]string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		e.loggerSet.Engine.Warnf("task meta 列解不出来（按无 Meta 继续）：%s（原值 %s）", err.Error(), string(raw))
		return nil
	}
	return m
}

// taskFromRecord 把一行任务还原成内存里的 Task —— **「行 → Task」唯一的入口**。
//
// 恢复队列、轮询队列、错误队列、后台「重投」、后台「加急」五条路都走它。收成一个函数的原因：
// 这五处原来各写一遍字段字面量，于是每加一个属于「这条任务」的字段就漏一遍 ——
// `Meta` 就是这么丢的（库里有 URL、有幂等键，唯独业务键没落库也没读回来，
// 进程重启后恢复出来的任务全成了"没有业务身份"的空壳）。
// 以后再往 Task 上挂这种字段，加在这里一处即可。
//
// 与它语义不同的那几项由调用方拿到之后再改（见各调用点）：`Repeatable`/`Urgent` 按需要覆盖，
// 行里 retry 刚被归零的那两条路要把内存副本跟上。这里不塞分支 —— 重建的字段是确定的。
func (e *Engine) taskFromRecord(t *models.CrawlerTask) *Task {
	return &Task{
		ID:             int(t.ID),
		PID:            int(t.PID),
		URL:            t.URL,
		Stage:          t.Stage,
		Site:           t.Site,
		Retry:          t.Retry,
		Repeatable:     t.Repeatable == models.RepeatableYes,
		Urgent:         t.Urgent,
		IdempotencyKey: t.IdempotencyKey,
		Meta:           e.metaFromJSON(t.Meta),
	}
}

func (t *Task) Insert(db *gorm.DB) error {
	crawlerTask := t.toModel()
	if err := db.Create(&crawlerTask).Error; err != nil {
		return err
	}
	t.ID = int(crawlerTask.ID)
	return nil
}

// UpdateStatus 写入任务状态；status 为 failed 时把错误追加到 error 列。
//
// **只更新 status / error 两列，不整行 Save**：worker 标状态和业务回写结果是两条并发的路
// （`SaveResult` / `SaveContent` 也是按列写的），整行写会把这里读到的旧快照盖回去 ——
// 业务刚写进 content 的内容就这么没了。追加错误同理，走 SQL 的 CONCAT，不把行读出来拼。
//
// 返回值是「这条语句执行成功了」，**不是**「行存在」—— 原来返回 false 表示行不见了，
// 但没有任何调用方用它判存在性，别依赖它。也正因为不需要判存在性，这里少了一次 SELECT。
func (t *Task) UpdateStatus(db *gorm.DB, status models.TaskStatus, err error) bool {
	if t.ID == 0 {
		return false
	}
	updates := map[string]any{"status": status}
	if status == models.TaskStatusFailed {
		var errMsg string
		if err != nil {
			errMsg = err.Error()
		}
		updates["error"] = gorm.Expr("CONCAT(COALESCE(error, ''), ?)", errMsg+"\n")
	}
	return db.Model(&models.CrawlerTask{}).Where("id = ?", t.ID).Updates(updates).Error == nil
}

func (t *Task) Unique() string {
	if t.IdempotencyKey != "" {
		return t.IdempotencyKey
	}
	return t.Stage + "|" + t.URL
}

// Fetcher 爬虫操作业务逻辑接口
type Fetcher interface {
	GetStage() string
	FetchHandler(ctx context.Context, task *Task, engine *Engine) error
}
