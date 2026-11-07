package engine

import (
	"errors"
	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/gorm/clause"
	"strings"
	"testing"
)

func TestTaskErrorMaskKeepsRequestAndIdentity(t *testing.T) {
	task := &Task{URL: "https://example.test/a?token=SECRET", Site: "sample", Stage: "detail"}
	identity := task.Unique()
	updates := taskStatusUpdates(models.TaskStatusFailed, errors.New("GET "+task.URL))
	expr := updates["error"].(clause.Expr)
	if strings.Contains(expr.Vars[0].(string), "SECRET") {
		t.Fatal("secret persisted in task error")
	}
	if task.Unique() != identity || task.toModel().URL != task.URL || !strings.Contains(task.URL, "SECRET") {
		t.Fatal("request/identity changed")
	}
}
