package engine

import (
	"encoding/json"
	"errors"

	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/gorm"
)

// ErrTaskBatchProtected means the requested generic operation would break a
// periodic batch's identity or completion accounting.
var ErrTaskBatchProtected = errors.New("周期批次任务受保护：不可删除控制任务/成员，不可单独轮询成员，仅失败成员允许原批重投")

// RuleTaskRole derives a presentation hint from durable metadata. Never trust
// a role sent back by a client; Engine operations always load the stored row.
// Invalid metadata is protected conservatively until repaired offline.
func RuleTaskRole(meta []byte) string {
	if len(meta) == 0 {
		return "ordinary"
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(meta, &m) != nil {
		return "invalid"
	}
	if _, ok := m[scheduleMeta]; ok {
		if _, member := m[ruleBatchMeta]; member {
			return "invalid" // contradictory roles must never weaken member guards
		}
		return "schedule"
	}
	if _, ok := m[ruleBatchMeta]; ok {
		return "member"
	}
	return "ordinary"
}

func taskRetryScope(db *gorm.DB, t *models.CrawlerTask, was int) *gorm.DB {
	q := retryScope(db, t.ID, was)
	if RuleTaskRole(t.Meta) == "member" {
		// The failed-state guard must remain in the UPDATE, not just the read:
		// another retry/worker may already have advanced this row.
		q = q.Where("status = ? AND repeatable = ?", models.TaskStatusFailed, models.RepeatableNo)
	}
	return q
}
