package taskstore

import (
	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"strings"
	"testing"
)

func dry(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(mysql.New(mysql.Config{DSN: "unused:unused@tcp(127.0.0.1:1)/unused", SkipInitializeWithVersion: true}), &gorm.Config{DryRun: true, DisableAutomaticPing: true, SkipDefaultTransaction: true})
	if err != nil {
		t.Fatal(err)
	}
	return db
}
func requireSQL(t *testing.T, db *gorm.DB, parts ...string) {
	t.Helper()
	if db.Error != nil {
		t.Fatal(db.Error)
	}
	sql := db.Statement.SQL.String()
	for _, part := range parts {
		if !strings.Contains(sql, part) {
			t.Fatalf("missing %q in %s", part, sql)
		}
	}
}
func TestClaimAndReleaseFences(t *testing.T) {
	requireSQL(t, Claim(dry(t), 7), "status = ?", "not_before <= UTC_TIMESTAMP(3)", "compensation_mode IN", "flow_id = ?")
	requireSQL(t, Release(dry(t), 7), "id = ? AND status = ?")
}
func TestRetryAndRepeatOwnGenerationTransitions(t *testing.T) {
	requireSQL(t, RetryTask(dry(t), 7, models.TaskStatusFailed, RunVersion{3, 2}, 9), "reprocess = ?", "`repeat` = ?", "compensation_version = ?", "`reprocess`=reprocess + 1")
	requireSQL(t, RetryFailed(dry(t), 7, 9, 4), "reprocess < ?", "compensation_version = ?")
	requireSQL(t, AdvanceRepeat(dry(t), 7, 9, 12, 60), "repeatable = ? AND status IN", "`repeat` < ?", "`repeat`=`repeat` + 1", "FROM_UNIXTIME")
}
func TestTerminalScopeFencesExactRound(t *testing.T) {
	requireSQL(t, TerminalScope(dry(t), 7, RunVersion{3, 2}).Updates(map[string]any{"status": models.TaskStatusSuccess}), "id = ? AND status = ? AND `repeat` = ? AND reprocess = ?")
}
