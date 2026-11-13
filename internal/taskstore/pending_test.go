package taskstore

import (
	"context"
	"github.com/ydtg1993/papa/v4/models"
	"testing"
	"time"
)

func TestPendingWindowOwnsDueCompensationAndPaging(t *testing.T) {
	var rows []models.CrawlerTask
	q := PendingWindow(context.Background(), dry(t), "site", "stage", 17, time.Minute, 8).Find(&rows)
	requireSQL(t, q, "site = ? AND stage = ? AND status = ? AND id > ?", "TIMESTAMPADD(MICROSECOND, ?, UTC_TIMESTAMP(3))", "compensation_mode IN", "ORDER BY id ASC", "LIMIT ?")
	if q.Statement.Context == nil {
		t.Fatal("missing context")
	}
}
