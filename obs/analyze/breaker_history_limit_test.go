package analyze

import (
	"context"
	"encoding/json"
	"github.com/ydtg1993/papa/v4/internal/gormfake"
	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/obs"
	"github.com/ydtg1993/papa/v4/obs/incident"
	"gorm.io/gorm"
	"testing"
	"time"
)

func TestBreakerTruncatedDailyHistoryMustNotInventFinalState(t *testing.T) {
	db := gormfake.MustOpen(gormfake.New())
	rows := make([]models.BreakerHistory, 10001)
	for i := range rows {
		state := "manual"
		if i == 10000 {
			state = "recovered"
		}
		at := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Second)
		payload, _ := json.Marshal(incident.Transition{At: at, Sequence: uint64(i + 1), To: state, Action: state})
		rows[i] = models.BreakerHistory{IncidentID: "same-day-recovered", Site: "synthetic", Sequence: uint64(i + 1), At: at, State: state, Payload: payload}
	}
	if err := db.Callback().Query().After("gorm:query").Register("review:history_rows", func(tx *gorm.DB) {
		if dst, ok := tx.Statement.Dest.(*[]models.BreakerHistory); ok {
			*dst = rows
			tx.RowsAffected = int64(len(rows))
			tx.Error = nil
		}
	}); err != nil {
		t.Fatal(err)
	}
	result := &obs.SectionResult{}
	addBreakerReports(context.Background(), &obs.Input{DB: db}, result)
	if len(result.Problems) != 1 || result.Problems[0].ID != "breaker-reports-truncated" {
		t.Fatal("missing explicit incomplete-history warning")
	}
	for _, p := range result.Problems {
		if p.ID == "breaker-active-same-day-recovered" {
			t.Fatalf("truncated prefix reported as final day state: %s", p.Title)
		}
	}
}
