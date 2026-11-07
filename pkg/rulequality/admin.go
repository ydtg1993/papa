package rulequality

import (
	"context"
	"database/sql"
	"time"

	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/gorm"
)

const AdminPageSize = 50
const AdminSummaryLimit = 1000

// AdminFilter reads bounded candidate pages, then matches exact optional fields.
// Scanned counts validated candidates, not database work or a site's total.
// Cutoff bounds sequences, not visibility across transactions: late commits and
// retention can change subsequent pages. Summary uses its own consistent read.
type AdminFilter struct {
	Site, Stage, RuleID, Version string
	From, Until                  time.Time
	Before, Cutoff               uint64
	BeforeReport                 string
}
type ObservationPage struct {
	Items   []Observation `json:"items"`
	Scanned int           `json:"scanned"`
	Cutoff  uint64        `json:"cutoff"`
	Next    uint64        `json:"next"`
}
type ReportPage struct {
	Items   []Report `json:"items"`
	Scanned int      `json:"scanned"`
	Next    string   `json:"next"`
}

func (f AdminFilter) Validate() error {
	for _, s := range []string{f.Site, f.Stage, f.RuleID, f.Version} {
		if s != "" && !validText(s, 128) {
			return ErrInvalid
		}
	}
	if f.From.IsZero() || f.Until.IsZero() || !f.From.Before(f.Until) || f.Until.Sub(f.From) > 7*24*time.Hour {
		return ErrInvalid
	}
	if f.Before > 0 && (f.Cutoff == 0 || f.Before > f.Cutoff) {
		return ErrInvalid
	}
	if f.BeforeReport != "" && !validCode(f.BeforeReport, 128) {
		return ErrInvalid
	}
	return nil
}
func (f AdminFilter) matches(s Scope) bool {
	return (f.Site == "" || f.Site == s.Site) && (f.Stage == "" || f.Stage == s.Stage) && (f.RuleID == "" || f.RuleID == s.RuleID) && (f.Version == "" || f.Version == s.Version)
}

func adminObservationQuery(tx *gorm.DB, f AdminFilter, cutoff uint64) *gorm.DB {
	q := tx.Where("sequence <= ? AND created_at >= ? AND created_at < ?", cutoff, f.From.UTC(), f.Until.UTC())
	if f.Before > 0 {
		q = q.Where("sequence < ?", f.Before)
	}
	return q.Order("sequence DESC").Limit(AdminPageSize + 1)
}
func adminReportQuery(tx *gorm.DB, f AdminFilter) *gorm.DB {
	q := tx.Where("created_at >= ? AND created_at < ?", f.From.UTC(), f.Until.UTC())
	if f.BeforeReport != "" {
		q = q.Where("id < ?", f.BeforeReport)
	}
	return q.Order("id DESC").Limit(AdminPageSize + 1)
}

func (s *MySQLStore) Observations(ctx context.Context, f AdminFilter) (ObservationPage, error) {
	out := ObservationPage{Items: []Observation{}}
	if err := f.Validate(); err != nil {
		return out, err
	}
	err := s.transaction(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}, func(tx *gorm.DB) error {
		out.Cutoff = f.Cutoff
		if out.Cutoff == 0 {
			if err := tx.Model(&models.RuleObservation{}).Select("COALESCE(MAX(sequence),0)").Scan(&out.Cutoff).Error; err != nil {
				return err
			}
		}
		var rows []models.RuleObservation
		if err := adminObservationQuery(tx, f, out.Cutoff).Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) > AdminPageSize {
			rows = rows[:AdminPageSize]
			out.Next = rows[len(rows)-1].Sequence
		}
		out.Scanned = len(rows)
		for _, row := range rows {
			o, err := readObservation(row)
			if err != nil {
				return err
			}
			if f.matches(o.Scope) {
				out.Items = append(out.Items, o)
			}
		}
		return nil
	})
	return out, err
}

// Reports is a live keyset listing, not a frozen cross-page snapshot. Report IDs
// are opaque binary keys; a concurrent insertion may require a fresh listing.
func (s *MySQLStore) Reports(ctx context.Context, f AdminFilter) (ReportPage, error) {
	out := ReportPage{Items: []Report{}}
	if ctx == nil {
		return out, ErrInvalid
	}
	if err := f.Validate(); err != nil {
		return out, err
	}
	var rows []models.RuleQualityReport
	if err := adminReportQuery(s.db.WithContext(ctx), f).Find(&rows).Error; err != nil {
		return out, qualityDBError(err)
	}
	if len(rows) > AdminPageSize {
		rows = rows[:AdminPageSize]
		out.Next = rows[len(rows)-1].ID
	}
	out.Scanned = len(rows)
	for _, row := range rows {
		r, err := readReport(row)
		if err != nil {
			return out, err
		}
		if f.matches(r.Query.Scope) {
			out.Items = append(out.Items, r)
		}
	}
	return out, nil
}
func (s *MySQLStore) Observation(ctx context.Context, id, reportID string) (Observation, error) {
	if err := checkedKey(ctx, id); err != nil {
		return Observation{}, err
	}
	var report *Report
	if reportID != "" {
		r, err := s.Report(ctx, reportID)
		if err != nil {
			return Observation{}, err
		}
		report = &r
	}
	var row models.RuleObservation
	if err := s.db.WithContext(ctx).Where("id = ?", id).Take(&row).Error; err != nil {
		return Observation{}, qualityDBError(err)
	}
	o, err := readObservation(row)
	if err != nil {
		return Observation{}, err
	}
	if report != nil {
		match := false
		for _, ref := range report.Evidence {
			if ref.ObservationID == id && ref.EvidenceID == o.EvidenceID && ref.BodySHA256 == o.BodySHA256 {
				match = true
				break
			}
		}
		if !match || report.Query.Scope != o.Scope || o.Sequence > report.Cutoff || o.CreatedAt.Before(report.Query.From) || !o.CreatedAt.Before(report.Query.Until) {
			return Observation{}, ErrEvidence
		}
	}
	return o, nil
}

// Summary is ephemeral: no report persistence or model invocation occurs.
func (s *MySQLStore) Summary(ctx context.Context, q Query) (Report, error) {
	if q.Limit != AdminSummaryLimit || q.LostObservations != 0 || q.Until.Sub(q.From) > 7*24*time.Hour {
		return Report{}, ErrInvalid
	}
	w, err := s.Query(ctx, q)
	if err != nil {
		return Report{}, err
	}
	r, err := Aggregate(w, DefaultPolicy(), nil)
	r.ID = "" // no durable identity is promised for an on-demand view
	return r, err
}
