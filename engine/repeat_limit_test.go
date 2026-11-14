package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/internal/workerpool"
	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/gorm"
)

func TestRepeatLimitBoundaryOnlyRestrictsRepeatQueue(t *testing.T) {
	for _, tc := range []struct {
		name         string
		count, limit int
		queue        string
		want         bool
	}{
		{"before-limit", 29, 30, "repeat", true}, {"at-limit", 30, 30, "repeat", false}, {"over-limit", 31, 30, "repeat", false},
		{"unlimited", 100, 0, "repeat", true}, {"recover-at-limit", 30, 30, "recover", true}, {"error-at-limit", 30, 30, "error", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeTaskDB()
			pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
			e := queueEngine(t, f, pool)
			e.stages["stub:stub"].config.RepeatLimit = tc.limit
			row := actionRow(7, "stub")
			row.Repeat = tc.count
			row.Repeatable = models.RepeatableYes
			row.Status = models.TaskStatusSuccess
			var got bool
			switch tc.queue {
			case "repeat":
				got = e.requeueRepeatTaskIn(row)
			case "recover":
				row.Status = models.TaskStatusPending
				got = e.requeueRecoverTask(row)
			case "error":
				row.Status = models.TaskStatusFailed
				got = e.requeueFailedTaskIn(row)
			}
			if got != tc.want {
				t.Fatalf("requeue=%v want %v", got, tc.want)
			}
			if !tc.want && f.written() != "" {
				t.Fatalf("capped task was modified: %s", f.written())
			}
			if tc.queue == "repeat" && tc.want && tc.limit > 0 && !strings.Contains(f.written(), "`repeat` < ?") {
				t.Fatalf("missing atomic reset guard: %s", f.written())
			}
		})
	}
}

func TestRepeatLimitSQLScopesAndScheduler(t *testing.T) {
	db := dryRunMySQL(t)
	e := &Engine{db: db, stages: map[string]*stageInfo{
		"limited:detail": {stage: "detail", config: StageConfig{Site: "limited", RepeatLimit: 30}},
		"other:detail":   {stage: "detail", config: StageConfig{Site: "other"}},
	}}
	for name, q := range map[string]*gorm.DB{"auto": e.repeatQueueQuery()(), "force": e.repeatForceQueueQuery()(), "scheduler": e.soonestRepeatQueryOn(db)} {
		t.Run(name, func(t *testing.T) {
			result := q.Find(&[]models.CrawlerTask{})
			sql := result.Statement.SQL.String()
			if !strings.Contains(sql, "NOT (site = ? AND stage = ? AND `repeat` >= ?)") {
				t.Fatalf("missing scoped cap: %s", sql)
			}
			args := result.Statement.Vars
			found := false
			for i := 0; i+2 < len(args); i++ {
				if args[i] == "limited" && args[i+1] == "detail" && args[i+2] == 30 {
					found = true
				}
			}
			if !found {
				t.Fatalf("wrong scope args: %v", args)
			}
		})
	}
}

func TestRepeatLimitGroupsOnlyEqualStageAndLimit(t *testing.T) {
	e := &Engine{stages: map[string]*stageInfo{
		"a:detail":    {stage: "detail", config: StageConfig{Site: "a", RepeatLimit: 3}},
		"b:detail":    {stage: "detail", config: StageConfig{Site: "b", RepeatLimit: 3}},
		"c:detail":    {stage: "detail", config: StageConfig{Site: "c", RepeatLimit: 5}},
		"a:catalog":   {stage: "catalog", config: StageConfig{Site: "a", RepeatLimit: 2}},
		"d:catalog":   {stage: "catalog", config: StageConfig{Site: "d", RepeatLimit: 2}},
		"free:detail": {stage: "detail", config: StageConfig{Site: "free"}},
	}}
	db := dryRunMySQL(t)
	var previous string
	for i := 0; i < 10; i++ {
		sql := db.ToSQL(func(d *gorm.DB) *gorm.DB {
			return e.withRepeatLimits(d.Model(&models.CrawlerTask{})).Find(&[]models.CrawlerTask{})
		})
		for _, want := range []string{
			"NOT (site IN ('a','b') AND stage = 'detail' AND `repeat` >= 3)",
			"NOT (site = 'c' AND stage = 'detail' AND `repeat` >= 5)",
			"NOT (site IN ('a','d') AND stage = 'catalog' AND `repeat` >= 2)",
		} {
			if !strings.Contains(sql, want) {
				t.Fatalf("missing %s in %s", want, sql)
			}
		}
		if strings.Count(sql, "NOT (") != 3 || strings.Contains(sql, "'free'") {
			t.Fatalf("wrong cap groups: %s", sql)
		}
		if i > 0 && sql != previous {
			t.Fatal("map iteration changed group/parameter order")
		}
		previous = sql
	}
}

func TestRepeatCountDeliveredAfterRequeue(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := queueEngine(t, f, pool)
	got := make(chan int, 1)
	pool.Start(context.Background(), func(_ context.Context, task *Task) error { got <- task.Repeat; return nil })
	defer pool.Stop(time.Second)
	row := actionRow(7, "stub")
	row.Repeat = 12
	row.Repeatable = models.RepeatableYes
	row.Status = models.TaskStatusSuccess
	if !e.requeueRepeatTaskIn(row) {
		t.Fatal("requeue failed")
	}
	select {
	case count := <-got:
		if count != 13 {
			t.Fatalf("handler count=%d want13", count)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("not delivered")
	}
	if !strings.Contains(f.written(), "`repeat` + 1") {
		t.Fatalf("DB not incremented: %s", f.written())
	}
}

func TestTaskRepeatIsReadOnlyAndRebuilt(t *testing.T) {
	e := &Engine{}
	task := e.taskFromRecord(&models.CrawlerTask{Repeat: 17})
	if task.Repeat != 17 {
		t.Fatalf("rebuild lost count: %v", task.Repeat)
	}
	if task.toModel().Repeat != 0 {
		t.Fatal("caller must not seed counter")
	}
}
