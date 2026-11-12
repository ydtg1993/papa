package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/gorm"
)

// Exercise the storage service without an Engine, pools, ticker or stage map.
func TestTaskFlowServiceMySQLLineageAndPolicyCAS(t *testing.T) {
	db, _ := compensationMySQL(t)
	if err := db.AutoMigrate(&models.TaskFlow{}); err != nil {
		t.Fatal(err)
	}
	s := taskFlowService{db: db}
	ctx := context.Background()
	p := FlowPolicy{Stages: map[string]string{"stage": "lane"}, Lanes: map[string]FlowLane{"lane": {Mode: "depth", Parallel: 1}}}
	if err := s.create(ctx, "run", "site", p); err != nil {
		t.Fatal(err)
	}
	root := (&Task{Site: "site", Stage: "stage", URL: "https://example.invalid/root"}).toModel()
	root, err := s.register(ctx, "run", 0, root)
	if err != nil {
		t.Fatal(err)
	}
	if ok, wait, err := s.claim(ctx, "run", root.ID); err != nil || !ok || wait {
		t.Fatalf("root claim: %v %v %v", ok, wait, err)
	}
	child := (&Task{Site: "site", Stage: "stage", URL: "https://example.invalid/child"}).toModel()
	child, err = s.register(ctx, "run", root.ID, child)
	if err != nil || child.FlowBranch != root.ID || child.PID != root.ID {
		t.Fatalf("lineage: %+v %v", child, err)
	}
	if ok, wait, err := s.claim(ctx, "run", child.ID); err != nil || ok || !wait {
		t.Fatalf("child claim: %v %v %v", ok, wait, err)
	}
	p.Lanes["lane"] = FlowLane{Mode: "breadth", Parallel: 2}
	if err := s.update(ctx, "run", 1, p); err != nil {
		t.Fatal(err)
	}
	if err := s.update(ctx, "run", 1, p); !errors.Is(err, ErrFlowConflict) {
		t.Fatalf("stale revision: %v", err)
	}
	view, err := s.view(ctx, "run")
	if err != nil || view.Revision != 2 || view.ActiveCount != 2 {
		t.Fatalf("view: %+v %v", view, err)
	}
	// A rejected relationship must not create any partial task or lineage record.
	bad := (&Task{Site: "site", Stage: "stage", URL: "https://example.invalid/invalid"}).toModel()
	if _, err := s.register(ctx, "run", child.ID, bad); !errors.Is(err, ErrFlowConflict) {
		t.Fatalf("pending parent accepted: %v", err)
	}
	var count int64
	if err := db.Model(&models.CrawlerTask{}).Where("flow_id = ?", "run").Count(&count).Error; err != nil || count != 2 {
		t.Fatalf("partial registration leaked: %d %v", count, err)
	}
}

func TestTaskFlowServiceMySQLRegistrationCancellationRollsBack(t *testing.T) {
	db, _ := compensationMySQL(t)
	if err := db.AutoMigrate(&models.TaskFlow{}); err != nil {
		t.Fatal(err)
	}
	s := taskFlowService{db: db}
	p := FlowPolicy{Stages: map[string]string{"stage": "lane"}, Lanes: map[string]FlowLane{"lane": {Mode: "depth", Parallel: 1}}}
	if err := s.create(context.Background(), "cancel-run", "site", p); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const callback = "test:cancel_lineage_update"
	if err := db.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if _, ok := tx.Statement.Model.(*models.CrawlerTask); ok {
			cancel()
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Update().Remove(callback) })
	candidate := (&Task{Site: "site", Stage: "stage", URL: "https://example.invalid/canceled"}).toModel()
	_, err := s.register(ctx, "cancel-run", 0, candidate)
	if err == nil {
		t.Fatal("registration committed despite cancellation before lineage update")
	}
	var count int64
	if err := db.Model(&models.CrawlerTask{}).Where("flow_id = ?", "cancel-run").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("canceled lineage left a partial task: count=%d err=%v", count, err)
	}
}
