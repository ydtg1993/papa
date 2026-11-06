package rulequality

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func exerciseQualityResultBounds(t *testing.T, s Store, prefix string) {
	t.Helper()
	ctx := context.Background()
	report, err := s.SaveReport(ctx, storeReport(prefix+"-report", storeObservation(prefix).Scope))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		result string
	}{
		{"escaped_payload", `{"value":"` + strings.Repeat("<", MaxResultBytes-12) + `"}`},
		{"invalid_utf8", "{\"value\":\"\xff\"}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := s.CreateRequest(ctx, storeRequest(prefix+"-"+tc.name, report.ID))
			if err != nil {
				t.Fatal(err)
			}
			r, err = s.StartRequest(ctx, r.ID, r.Revision, strings.Repeat("a", 64))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.FinishRequest(ctx, r.ID, r.Revision, Failed, tc.result); !errors.Is(err, ErrInvalid) {
				t.Errorf("invalid result accepted: %v", err)
			}
			read, err := s.Request(ctx, r.ID)
			if err != nil || !reflect.DeepEqual(read, r) {
				t.Errorf("invalid result changed reservation: status=%s revision=%d error=%v", read.Status, read.Revision, err)
			}
		})
	}
}

func TestQualityMemoryRejectsUnpersistableResults(t *testing.T) {
	exerciseQualityResultBounds(t, NewMemoryStore(), "result-bounds")
}

func TestQualityMySQLRejectsUnpersistableResults(t *testing.T) {
	s, _, prefix := qualityMySQL(t)
	exerciseQualityResultBounds(t, s, prefix)
}

func exerciseQualityFinalReportBound(t *testing.T, s Store, prefix string) {
	t.Helper()
	r := storeReport(prefix, storeObservation(prefix).Scope)
	r.Signals = make([]Signal, MaxFields*8)
	for i := range r.Signals {
		r.Signals[i] = Signal{Code: "bounded", Field: strings.Repeat("x", 128)}
	}
	raw, _, err := encodePayload(r)
	if err != nil {
		t.Fatal(err)
	}
	// Each escaped '<' adds five bytes without exceeding the field-name bound.
	remaining := (MaxPayloadBytes - len(raw)) / 5
	for i := range r.Signals {
		n := min(remaining, 128)
		r.Signals[i].Field = strings.Repeat("<", n) + strings.Repeat("x", 128-n)
		remaining -= n
	}
	if raw, _, err = encodePayload(r); err != nil || len(raw) < MaxPayloadBytes-4 {
		t.Fatalf("bad boundary fixture: bytes=%d error=%v", len(raw), err)
	}
	saved, err := s.SaveReport(context.Background(), r)
	if err == nil {
		// An exact whole-second timestamp has the same encoded length as zero.
		// Otherwise a successful write must still obey the final payload cap.
		if _, _, err := encodePayload(saved); err != nil {
			t.Error("store timestamp pushed persisted report beyond payload cap")
		}
		return
	}
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("report payload bound: %v", err)
	}
	if _, err := s.Report(context.Background(), r.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("oversized report was persisted: %v", err)
	}
}

func TestQualityMemoryBoundsReportAfterAssigningTimestamp(t *testing.T) {
	s := NewMemoryStore()
	s.now = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC) }
	exerciseQualityFinalReportBound(t, s, "report-bound")
}

func TestQualityMySQLBoundsReportAfterAssigningTimestamp(t *testing.T) {
	s, _, prefix := qualityMySQL(t)
	exerciseQualityFinalReportBound(t, s, prefix)
}
