package analyze

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/internal/gormfake"
	"github.com/ydtg1993/papa/v3/obs"
	"gorm.io/gorm"
)

// TestTaskBucketsQueryShape 只断言 SQL 形状（DryRun 不连库）：老化分桶的 CASE 与分组维度
// 一旦写错，积压段的分桶口径就静默变了，所以这里按列名钉住。
// 老化基准：pending 用 created_at（排队等了多久），processing 用 updated_at（距上次更新多久）。
func TestTaskBucketsQueryShape(t *testing.T) {
	db := gormfake.MustOpen(gormfake.New())
	now := time.Date(2026, 10, 29, 0, 30, 0, 0, time.Local)
	q := taskBucketsQuery(db, now)
	stmt := q.Session(&gorm.Session{DryRun: true}).Find(&[]TaskBucket{}).Statement
	sql := strings.ToLower(stmt.SQL.String())
	for _, want := range []string{"crawler_tasks", "site", "stage", "status", "repeatable", "created_at", "updated_at", "age_bucket"} {
		if !strings.Contains(sql, want) {
			t.Fatalf("taskBucketsQuery SQL 缺 %q：%s", want, stmt.SQL.String())
		}
	}
	// pending/processing 必须真的分两套基准，而不是只把 updated_at 换成 created_at。
	if !strings.Contains(sql, "when status = ? then case") {
		t.Fatalf("老化 CASE 应按 status 分 pending/processing 两套基准：%s", stmt.SQL.String())
	}
}

// fakeQuerier 是 Querier 的假实现：EngineSection 的所有 DB 取数都能在不起库的情况下驱动。
type fakeQuerier struct {
	sites  []obs.SiteState
	old    int64
	bucket []TaskBucket
	err    error
}

func (f *fakeQuerier) SiteStates(context.Context) ([]obs.SiteState, error) { return f.sites, f.err }
func (f *fakeQuerier) TaskBuckets(context.Context, time.Time) ([]TaskBucket, error) {
	return f.bucket, f.err
}
func (f *fakeQuerier) OldProcessing(context.Context, time.Time) (int64, error) {
	return f.old, f.err
}
