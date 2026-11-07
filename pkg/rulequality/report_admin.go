package rulequality

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"time"

	"github.com/ydtg1993/papa/v3/models"
)

var ErrReportChanged = errors.New("rulequality: reviewed observation window changed")

// FreezeInput contains the exact query and digest reviewed by a human, never
// client-calculated metrics. OperationID is global, including across operators.
type FreezeInput struct {
	OperationID string `json:"operation_id"`
	Query       Query  `json:"query"`
	InputSHA256 string `json:"input_sha256"`
}

func reportOperationID(operation string) string {
	sum := sha256.Sum256([]byte(operation))
	return "oa-report-" + hex.EncodeToString(sum[:])
}
func ReportByOperation(ctx context.Context, store Store, operation string) (Report, error) {
	if ctx == nil || collectorNil(store) || !validCode(operation, 128) {
		return Report{}, ErrInvalid
	}
	r, err := store.Report(ctx, reportOperationID(operation))
	if err == nil && r.OperationID != operation {
		return Report{}, ErrConflict
	}
	return r, err
}
func FreezeReport(ctx context.Context, store Store, in FreezeInput, operator string) (Report, error) {
	if ctx == nil || collectorNil(store) || !validCode(in.OperationID, 128) || !validText(operator, 128) || !validSHA(in.InputSHA256, false) || in.Query.Limit != AdminSummaryLimit || in.Query.LostObservations != 0 || in.Query.Until.Sub(in.Query.From) > 7*24*time.Hour {
		return Report{}, ErrInvalid
	}
	q, err := normalizeQuery(in.Query)
	if err != nil {
		return Report{}, err
	}
	existing, err := ReportByOperation(ctx, store, in.OperationID)
	if err == nil {
		if existing.Operator != operator || existing.InputSHA256 != in.InputSHA256 || !reflect.DeepEqual(existing.Query, q) {
			return Report{}, ErrConflict
		}
		return existing, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Report{}, err
	}
	window, err := store.Query(ctx, q)
	if err != nil {
		return Report{}, err
	}
	report, err := Aggregate(window, DefaultPolicy(), nil)
	if err != nil {
		return Report{}, err
	}
	if report.InputSHA256 != in.InputSHA256 {
		// A concurrent retry may have committed the reviewed report after our
		// initial lookup. Its immutable result takes precedence over live data.
		existing, lookupErr := ReportByOperation(ctx, store, in.OperationID)
		if lookupErr == nil {
			if existing.Operator != operator || existing.InputSHA256 != in.InputSHA256 || !reflect.DeepEqual(existing.Query, q) {
				return Report{}, ErrConflict
			}
			return existing, nil
		}
		if !errors.Is(lookupErr, ErrNotFound) {
			return Report{}, lookupErr
		}
		return Report{}, ErrReportChanged
	}
	report.ID = reportOperationID(in.OperationID)
	report.OperationID = in.OperationID
	report.Operator = operator
	return store.SaveReport(ctx, report)
}
func (s *MySQLStore) Freeze(ctx context.Context, in FreezeInput, operator string) (Report, error) {
	return FreezeReport(ctx, s, in, operator)
}
func (s *MySQLStore) ReportOperation(ctx context.Context, id string) (Report, error) {
	return ReportByOperation(ctx, s, id)
}

type RequestPage struct {
	Items   []Request `json:"items"`
	Scanned int       `json:"scanned"`
	Next    string    `json:"next"`
}

func (s *MySQLStore) Requests(ctx context.Context, before, reportID string) (RequestPage, error) {
	out := RequestPage{Items: []Request{}}
	if ctx == nil || before != "" && !validCode(before, 128) || reportID != "" && !validCode(reportID, 128) {
		return out, ErrInvalid
	}
	q := s.db.WithContext(ctx)
	if before != "" {
		q = q.Where("id < ?", before)
	}
	if reportID != "" {
		q = q.Where("report_id = ?", reportID)
	}
	var rows []models.RuleReanalysisRequest
	if err := q.Order("id DESC").Limit(21).Find(&rows).Error; err != nil {
		return out, qualityDBError(err)
	}
	if len(rows) > 20 {
		rows = rows[:20]
		out.Next = rows[19].ID
	}
	out.Scanned = len(rows)
	for _, row := range rows {
		r, err := readRequest(row)
		if err != nil {
			return out, err
		}
		out.Items = append(out.Items, r)
	}
	return out, nil
}
