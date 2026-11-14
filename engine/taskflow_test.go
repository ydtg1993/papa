package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/gorm"
)

func flowEngine() *Engine {
	e := &Engine{stages: map[string]*stageInfo{}}
	for _, site := range []string{"a", "b"} {
		e.stages[site+":catalog"] = &stageInfo{config: StageConfig{Site: site, Entry: true, StrictFlow: true, Downstream: []string{"detail"}}}
		e.stages[site+":detail"] = &stageInfo{config: StageConfig{Site: site, StrictFlow: true, Downstream: []string{"episode"}}}
		e.stages[site+":episode"] = &stageInfo{config: StageConfig{Site: site, StrictFlow: true}}
	}
	return e
}

func TestTaskFlowRejectsForgedAndExpiredSourceBeforeStorage(t *testing.T) {
	e := flowEngine()
	parent := &Task{ID: 7, Site: "a", Stage: "catalog"}
	ctx, release := e.bindTaskExecution(context.Background(), parent)
	child := &Task{Site: "a", Stage: "detail", URL: "https://example.com/1"}
	if err := e.SubmitChildren(context.Background(), parent, child); !errors.Is(err, ErrTaskFlow) {
		t.Fatalf("untrusted context: %v", err)
	}
	copyParent := *parent
	if err := e.SubmitChildren(ctx, &copyParent, child); !errors.Is(err, ErrTaskFlow) {
		t.Fatalf("copied parent: %v", err)
	}
	parent.Stage = "detail"
	if err := e.SubmitChildren(ctx, parent, child); !errors.Is(err, ErrTaskFlow) {
		t.Fatalf("mutated parent: %v", err)
	}
	parent.Stage = "catalog"
	if err := flowEngine().SubmitChildren(ctx, parent, child); !errors.Is(err, ErrTaskFlow) {
		t.Fatalf("wrong engine: %v", err)
	}
	release()
	if err := e.SubmitChildren(ctx, parent, child); !errors.Is(err, ErrTaskFlow) {
		t.Fatalf("expired context: %v", err)
	}
	if child.PID != 0 {
		t.Fatal("rejected child was mutated")
	}
}

func TestTaskFlowValidatesWholeBatchBeforeWrites(t *testing.T) {
	e := flowEngine() // nil DB: any unintended persistence would panic.
	parent := &Task{ID: 7, Site: "a", Stage: "catalog"}
	ctx, release := e.bindTaskExecution(context.Background(), parent)
	defer release()
	for _, bad := range []*Task{
		{Site: "a", Stage: "catalog"}, // same stage
		{Site: "a", Stage: "episode"}, // skips declared direct edge
		{Site: "b", Stage: "detail"},  // cross-site
		{Site: "a", Stage: "detail", PID: 999},
		{Site: "a", Stage: "missing"}, nil,
	} {
		good := &Task{Site: "a", Stage: "detail", URL: "https://example.com/1"}
		if err := e.SubmitChildren(ctx, parent, good, bad); !errors.Is(err, ErrTaskFlow) {
			t.Fatalf("bad child %+v: %v", bad, err)
		}
		if good.PID != 0 {
			t.Fatal("partial batch mutation before rejection")
		}
	}
}

func TestTaskFlowRootAndContextSubmissionBoundaries(t *testing.T) {
	e := flowEngine()
	root := &Task{Site: "a", Stage: "catalog"}
	entryCtx := context.WithValue(context.Background(), entrySubmissionContextKey{}, e)
	if err := e.validateSubmissionContext(entryCtx, []*Task{root}); err != nil {
		t.Fatal(err)
	}
	if err := e.validateSubmissionContext(context.Background(), []*Task{root}); !errors.Is(err, ErrTaskFlow) {
		t.Fatalf("legacy parentless self-submission bypass: %v", err)
	}
	for _, task := range []*Task{{Site: "a", Stage: "detail"}, {Site: "a", Stage: "catalog", PID: 7}} {
		if err := e.validateSubmissionContext(entryCtx, []*Task{task}); !errors.Is(err, ErrTaskFlow) {
			t.Fatalf("root bypass: %v", err)
		}
	}
	parent := &Task{ID: 7, Site: "a", Stage: "catalog"}
	ctx, release := e.bindTaskExecution(context.Background(), parent)
	defer release()
	if err := e.validateSubmissionContext(ctx, []*Task{{PID: 7, Site: "a", Stage: "detail"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.validateSubmissionContext(ctx, []*Task{root}); !errors.Is(err, ErrTaskFlow) {
		t.Fatalf("handler root bypass: %v", err)
	}
	parent2 := &Task{ID: 8, Site: "a", Stage: "detail"}
	ctx2, release2 := e.bindTaskExecution(context.Background(), parent2)
	defer release2()
	if err := e.SubmitChildren(ctx2, parent2, &Task{Site: "a", Stage: "catalog"}); !errors.Is(err, ErrTaskFlow) {
		t.Fatalf("reverse edge: %v", err)
	}
}

func TestLegacyParentlessSelfSubmissionRejectedBeforeStorage(t *testing.T) {
	e := flowEngine() // nil DB: accidentally accepting this would panic or hang.
	root := &Task{Site: "a", Stage: "catalog", URL: "https://example.com/page/2"}
	if err := e.SubmitTask(root); !errors.Is(err, ErrTaskFlow) {
		t.Fatalf("SubmitTask self-submission: %v", err)
	}
	if err := e.SubmitTasks([]*Task{root}); !errors.Is(err, ErrTaskFlow) {
		t.Fatalf("SubmitTasks self-submission: %v", err)
	}
	if root.ID != 0 {
		t.Fatal("rejected root was persisted")
	}
	parent := &Task{ID: 7, Site: "a", Stage: "catalog"}
	ctx, release := e.bindTaskExecution(context.Background(), parent)
	defer release()
	if err := e.SubmitEntryTaskContext(ctx, root); !errors.Is(err, ErrTaskFlow) {
		t.Fatalf("handler entry bypass: %v", err)
	}
	if err := e.SubmitEntryTask(&Task{Site: "a", Stage: "detail"}); !errors.Is(err, ErrTaskFlow) {
		t.Fatalf("non-entry root: %v", err)
	}
	if err := e.SubmitEntryTask(&Task{PID: 7, Site: "a", Stage: "catalog"}); !errors.Is(err, ErrTaskFlow) {
		t.Fatalf("entry with parent: %v", err)
	}
	if err := e.SubmitEntryTaskContext(nil, root); !errors.Is(err, ErrTaskFlow) {
		t.Fatalf("nil entry context: %v", err)
	}
}

func TestTaskFlowChecksPersistedDedupIdentity(t *testing.T) {
	e := flowEngine()
	task := &Task{Site: "a", Stage: "detail", IdempotencyKey: "shared-key"}
	for _, tc := range []struct {
		site, stage string
		valid       bool
	}{
		{"a", "detail", true},
		{"b", "detail", false},
		{"a", "catalog", false},
		{"a", "episode", false},
		{"", "", false},
	} {
		record := &models.CrawlerTask{ID: 99, Site: tc.site, Stage: tc.stage}
		err := e.validatePersistedTask(task, record)
		if tc.valid && err != nil {
			t.Fatal(err)
		}
		if !tc.valid && !errors.Is(err, ErrTaskFlow) {
			t.Fatalf("persisted %s:%s accepted: %v", tc.site, tc.stage, err)
		}
		if task.ID != 0 || task.Site != "a" || task.Stage != "detail" {
			t.Fatal("validation mutated submitted identity")
		}
	}
	if err := e.validatePersistedTask(task, nil); !errors.Is(err, ErrTaskFlow) {
		t.Fatalf("nil persisted row: %v", err)
	}
	if err := e.validatePersistedTask(nil, &models.CrawlerTask{}); !errors.Is(err, ErrTaskFlow) {
		t.Fatalf("nil submitted task: %v", err)
	}
}

func TestTaskFlowRejectsHandlerAdministrativeAdmission(t *testing.T) {
	e := flowEngine()
	parent := &Task{ID: 7, Site: "a", Stage: "catalog"}
	ctx, release := e.bindTaskExecution(context.Background(), parent)
	if err := rejectTaskAdminContext(ctx); !errors.Is(err, ErrTaskFlow) {
		t.Fatalf("active handler admin bypass: %v", err)
	}
	release()
	if err := rejectTaskAdminContext(ctx); !errors.Is(err, ErrTaskFlow) {
		t.Fatalf("expired handler admin bypass: %v", err)
	}
	if err := rejectTaskAdminContext(context.Background()); err != nil {
		t.Fatalf("ordinary control-plane rejected: %v", err)
	}
	if err := rejectTaskAdminContext(nil); !errors.Is(err, ErrTaskFlow) {
		t.Fatalf("nil context: %v", err)
	}
}

func TestSaveTaskMetaUsesProcessingAndIdentityGuard(t *testing.T) {
	e := flowEngine()
	f := newFakeTaskDB()
	e.db = openFakeTaskDB(t, f)
	parent := &Task{ID: 7, Site: "a", Stage: "catalog", Meta: map[string]string{"next_page": "2"}}
	ctx, release := e.bindTaskExecution(context.Background(), parent)
	defer release()
	if err := e.SaveTaskMeta(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.writtenArgs(), "next_page") {
		t.Fatal("checkpoint not persisted")
	}
	p, err := e.executionPermit(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	db := dryDB(t).Session(&gorm.Session{SkipDefaultTransaction: true})
	sql := taskMetaScope(db, p).Updates(map[string]any{"meta": metaToJSON(parent.Meta)}).Statement.SQL.String()
	for _, want := range []string{"id = ?", "site = ?", "stage = ?", "status = ?"} {
		if !strings.Contains(sql, want) {
			t.Fatalf("missing %s in %s", want, sql)
		}
	}
	f.affected = 0
	if err := e.SaveTaskMeta(ctx, parent); err == nil {
		t.Fatal("missing/settled task accepted checkpoint")
	}
	release()
	if err := e.SaveTaskMeta(ctx, parent); !errors.Is(err, ErrTaskFlow) {
		t.Fatalf("expired checkpoint: %v", err)
	}
}
