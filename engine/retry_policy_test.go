package engine

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/internal/breaker"
	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/gorm"
)

type permanentCustomFailure struct{}

func (permanentCustomFailure) Error() string   { return "custom permanent" }
func (permanentCustomFailure) Retryable() bool { return false }

func TestRetryPolicyTerminalAndJournal(t *testing.T) {
	for _, cause := range []error{core.WrapNoRetry(errors.New("permanent")), permanentCustomFailure{}} {
		sql := dryDB(t).ToSQL(func(db *gorm.DB) *gorm.DB {
			return db.Model(&models.CrawlerTask{}).Where("id = ?", 1).Updates(taskStatusUpdates(models.TaskStatusFailed, cause))
		})
		if !strings.Contains(sql, "COALESCE(retry_blocked, TRUE) OR true") {
			t.Fatal(sql)
		}
	}
	for _, value := range []*bool{nil, func() *bool { b := true; return &b }()} {
		p := breaker.PendingFailure{RetryBlocked: value, Error: "saved failure"}
		data, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		var restored breaker.PendingFailure
		if err = json.Unmarshal(data, &restored); err != nil {
			t.Fatal(err)
		}
		sql := dryDB(t).ToSQL(func(db *gorm.DB) *gorm.DB {
			return db.Model(&models.CrawlerTask{}).Where("id = ?", 1).Updates(pendingFailureUpdates(&restored))
		})
		if !strings.Contains(sql, "COALESCE(retry_blocked, TRUE) OR true") {
			t.Fatal(sql)
		}
	}
	updates := taskStatusUpdates(models.TaskStatusSuccess, nil)
	if updates["auto_retry_count"] != 0 || updates["retry_blocked"] != false {
		t.Fatal(updates)
	}
	if _, ok := updates["reprocess"]; ok {
		t.Fatal("success reset monotonic generation")
	}
}
