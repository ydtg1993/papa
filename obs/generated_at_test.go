package obs

import (
	"context"
	"testing"
	"time"
)

// Generate 必须把 opts.Now 写进 GeneratedAt（定时/退出报告据此标注"最近一次更新时间"）。
func TestGenerateSetsGeneratedAtFromNow(t *testing.T) {
	now := time.Date(2026, 10, 28, 23, 30, 0, 0, time.Local)
	rep, err := Generate(context.Background(), Options{
		Now: now,
		Run: RunMeta{RunIDs: []string{"r-g"}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !rep.GeneratedAt.Equal(now) {
		t.Fatalf("GeneratedAt = %v, want %v", rep.GeneratedAt, now)
	}
}
