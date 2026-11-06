package rulequality

import (
	"context"
	"database/sql"
	"errors"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MySQLStore owns transactions and never creates tables or resumes requests.
type MySQLStore struct{ db *gorm.DB }

var _ Store = (*MySQLStore)(nil)

func New(db *gorm.DB) (*MySQLStore, error) {
	if db == nil || db.Dialector == nil || db.Dialector.Name() != "mysql" || db.DryRun || db.Error != nil {
		return nil, ErrInvalid
	}
	if _, ok := db.ConnPool.(gorm.TxCommitter); ok {
		return nil, ErrInvalid
	}
	if db.Statement != nil {
		if _, ok := db.Statement.ConnPool.(gorm.TxCommitter); ok {
			return nil, ErrInvalid
		}
	}
	return &MySQLStore{db: db.Session(&gorm.Session{NewDB: true, Initialized: true, Context: context.Background(), SkipDefaultTransaction: true})}, nil
}
func qualityDBError(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	var e *driver.MySQLError
	if errors.Is(err, gorm.ErrDuplicatedKey) || errors.As(err, &e) && e.Number == 1062 {
		return ErrConflict
	}
	return err
}
func checkedQualityWrite(db *gorm.DB) error {
	if db.Error != nil {
		return qualityDBError(db.Error)
	}
	if db.RowsAffected != 1 {
		return ErrConflict
	}
	return nil
}
func qualityNow(tx *gorm.DB) (time.Time, error) {
	var now time.Time
	err := tx.Raw("SELECT CURRENT_TIMESTAMP(6)").Row().Scan(&now)
	return now.UTC(), err
}

// A COMMIT error never returns a successful reservation, even if the server
// actually committed. A later read is reconciliation only, not permission to run.
func (s *MySQLStore) transaction(ctx context.Context, opts *sql.TxOptions, fn func(*gorm.DB) error) error {
	if ctx == nil {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	tx := s.db.WithContext(ctx).Begin(opts)
	if tx.Error != nil {
		return qualityDBError(tx.Error)
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return qualityDBError(err)
	}
	if err := tx.Commit().Error; err != nil {
		return errors.Join(ErrUnconfirmed, err)
	}
	return nil
}
func observationRow(o Observation) (models.RuleObservation, error) {
	raw, sha, err := encodePayload(o)
	return models.RuleObservation{Sequence: o.Sequence, ID: o.ID, ScopeSHA256: scopeDigest(o.Scope), PayloadSHA256: sha, PayloadJSON: raw, CreatedAt: o.CreatedAt}, err
}
func readObservation(row models.RuleObservation) (Observation, error) {
	var o Observation
	if decodePayload(row.PayloadJSON, row.PayloadSHA256, &o) != nil || validateObservation(o) != nil || o.ID != row.ID || o.Sequence == 0 || o.Sequence != row.Sequence || o.CreatedAt.IsZero() || !o.CreatedAt.Equal(row.CreatedAt) || scopeDigest(o.Scope) != row.ScopeSHA256 {
		return Observation{}, ErrCorrupt
	}
	return o, nil
}
func reportRow(r Report) (models.RuleQualityReport, error) {
	raw, sha, err := encodePayload(r)
	return models.RuleQualityReport{ID: r.ID, ScopeSHA256: scopeDigest(r.Query.Scope), InputSHA256: r.InputSHA256, Cutoff: r.Cutoff, PayloadSHA256: sha, PayloadJSON: raw, CreatedAt: r.CreatedAt}, err
}
func readReport(row models.RuleQualityReport) (Report, error) {
	var r Report
	if decodePayload(row.PayloadJSON, row.PayloadSHA256, &r) != nil || validateReport(r) != nil || r.ID != row.ID || r.CreatedAt.IsZero() || !r.CreatedAt.Equal(row.CreatedAt) || scopeDigest(r.Query.Scope) != row.ScopeSHA256 || r.InputSHA256 != row.InputSHA256 || r.Cutoff != row.Cutoff {
		return Report{}, ErrCorrupt
	}
	return r, nil
}
func requestRow(r Request) (models.RuleReanalysisRequest, error) {
	raw, sha, err := encodePayload(r)
	return models.RuleReanalysisRequest{ID: r.ID, OperationID: r.OperationID, ReportID: r.ReportID, RunID: r.RunID, Status: r.Status, Revision: r.Revision, PayloadSHA256: sha, PayloadJSON: raw, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}, err
}
func readRequest(row models.RuleReanalysisRequest) (Request, error) {
	var r Request
	if decodePayload(row.PayloadJSON, row.PayloadSHA256, &r) != nil || validateStoredRequest(r) != nil || r.ID != row.ID || r.OperationID != row.OperationID || r.ReportID != row.ReportID || r.RunID != row.RunID || r.Status != row.Status || r.Revision != row.Revision || !r.CreatedAt.Equal(row.CreatedAt) || !r.UpdatedAt.Equal(row.UpdatedAt) {
		return Request{}, ErrCorrupt
	}
	return r, nil
}
func (s *MySQLStore) Record(ctx context.Context, o Observation) (Observation, error) {
	if err := checkedKey(ctx, o.ID); err != nil {
		return Observation{}, err
	}
	if err := validateObservation(o); err != nil {
		return Observation{}, err
	}
	if _, _, err := encodePayload(o); err != nil {
		return Observation{}, err
	}
	if o.Sequence != 0 || !o.CreatedAt.IsZero() {
		var row models.RuleObservation
		if err := s.db.WithContext(ctx).Where("id = ?", o.ID).Take(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return Observation{}, ErrInvalid
			}
			return Observation{}, err
		}
		old, err := readObservation(row)
		if err != nil {
			return Observation{}, err
		}
		if !sameObservation(o, old) {
			return Observation{}, ErrConflict
		}
		return old, nil
	}
	var out Observation
	err := s.transaction(ctx, nil, func(tx *gorm.DB) error {
		now, err := qualityNow(tx)
		if err != nil {
			return err
		}
		o.CreatedAt = now
		row, err := observationRow(o)
		if err != nil {
			return err
		}
		if err := checkedQualityWrite(tx.Create(&row)); err != nil {
			return err
		}
		o.Sequence = row.Sequence
		row, err = observationRow(o)
		if err != nil {
			return err
		}
		if err := checkedQualityWrite(tx.Model(&models.RuleObservation{}).Where("sequence = ?", o.Sequence).Updates(map[string]any{"payload_json": row.PayloadJSON, "payload_sha256": row.PayloadSHA256})); err != nil {
			return err
		}
		out = o
		return nil
	})
	if errors.Is(err, ErrConflict) {
		// Release INSERT duplicate-key locks before reading. Converting their
		// shared index locks to FOR UPDATE deadlocks concurrent identical writes.
		var row models.RuleObservation
		if readErr := s.db.WithContext(ctx).Where("id = ?", o.ID).Take(&row).Error; readErr != nil {
			return Observation{}, qualityDBError(readErr)
		}
		old, readErr := readObservation(row)
		if readErr != nil {
			return Observation{}, readErr
		}
		o.Sequence = 0
		o.CreatedAt = time.Time{}
		if !sameObservation(o, old) {
			return Observation{}, ErrConflict
		}
		return old, nil
	}
	if err != nil {
		return Observation{}, err
	}
	return detached(out), nil
}
func qualityScope(tx *gorm.DB, q Query) *gorm.DB {
	return tx.Where("scope_sha256 = ? AND created_at >= ? AND created_at < ?", scopeDigest(q.Scope), q.From, q.Until)
}
func (s *MySQLStore) Query(ctx context.Context, q Query) (Window, error) {
	q, err := normalizeQuery(q)
	if err != nil {
		return Window{}, err
	}
	out := Window{Query: q, Observations: []Observation{}}
	err = s.transaction(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}, func(tx *gorm.DB) error {
		if err := qualityScope(tx.Model(&models.RuleObservation{}), q).Select("COALESCE(MAX(sequence), 0)").Scan(&out.Cutoff).Error; err != nil {
			return err
		}
		var rows []models.RuleObservation
		if err := qualityScope(tx, q).Where("sequence <= ?", out.Cutoff).Order("sequence ASC").Limit(q.Limit + 1).Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			o, err := readObservation(row)
			if err != nil {
				return err
			}
			if o.Scope != q.Scope || o.CreatedAt.Before(q.From) || !o.CreatedAt.Before(q.Until) || o.Sequence > out.Cutoff {
				return ErrCorrupt
			}
			out.Observations = append(out.Observations, o)
		}
		if len(out.Observations) > q.Limit {
			out.Truncated = true
			out.Observations = out.Observations[:q.Limit]
		}
		return nil
	})
	if err != nil {
		return Window{}, err
	}
	return out, nil
}
func (s *MySQLStore) SaveReport(ctx context.Context, r Report) (Report, error) {
	if err := checkedKey(ctx, r.ID); err != nil {
		return Report{}, err
	}
	r, err := prepareReport(r)
	if err != nil {
		return Report{}, err
	}
	if !r.CreatedAt.IsZero() {
		old, err := s.Report(ctx, r.ID)
		if errors.Is(err, ErrNotFound) {
			return Report{}, ErrInvalid
		}
		if err != nil {
			return Report{}, err
		}
		if !sameReport(r, old) {
			return Report{}, ErrConflict
		}
		return old, nil
	}
	var out Report
	err = s.transaction(ctx, nil, func(tx *gorm.DB) error {
		now, err := qualityNow(tx)
		if err != nil {
			return err
		}
		r.CreatedAt = now
		row, err := reportRow(r)
		if err != nil {
			return err
		}
		if err := checkedQualityWrite(tx.Create(&row)); err != nil {
			return err
		}
		out = r
		return nil
	})
	if errors.Is(err, ErrConflict) {
		old, readErr := s.Report(ctx, r.ID)
		if readErr != nil {
			return Report{}, readErr
		}
		r.CreatedAt = time.Time{}
		if !sameReport(r, old) {
			return Report{}, ErrConflict
		}
		return old, nil
	}
	if err != nil {
		return Report{}, err
	}
	return detached(out), nil
}
func (s *MySQLStore) Report(ctx context.Context, id string) (Report, error) {
	if err := checkedKey(ctx, id); err != nil {
		return Report{}, err
	}
	var row models.RuleQualityReport
	if err := s.db.WithContext(ctx).Where("id = ?", id).Take(&row).Error; err != nil {
		return Report{}, qualityDBError(err)
	}
	return readReport(row)
}
func (s *MySQLStore) CreateRequest(ctx context.Context, r Request) (Request, error) {
	if err := checkedKey(ctx, r.ID); err != nil {
		return Request{}, err
	}
	r, err := prepareRequest(r)
	if err != nil {
		return Request{}, err
	}
	var out Request
	err = s.transaction(ctx, nil, func(tx *gorm.DB) error {
		var prior models.RuleReanalysisRequest
		priorErr := tx.Where("operation_id = ?", r.OperationID).Take(&prior).Error
		if priorErr == nil {
			existing, err := readRequest(prior)
			if err != nil {
				return err
			}
			if !requestEquivalent(r, existing) {
				return ErrConflict
			}
			out = existing
			return nil
		}
		if !errors.Is(priorErr, gorm.ErrRecordNotFound) {
			return priorErr
		}
		var report models.RuleQualityReport
		if err := tx.Where("id = ?", r.ReportID).Take(&report).Error; err != nil {
			return err
		}
		if _, err := readReport(report); err != nil {
			return err
		}
		now, err := qualityNow(tx)
		if err != nil {
			return err
		}
		r.CreatedAt = now
		r.UpdatedAt = now
		row, err := requestRow(r)
		if err != nil {
			return err
		}
		if err := checkedQualityWrite(tx.Create(&row)); err != nil {
			return err
		}
		out = r
		return nil
	})
	if errors.Is(err, ErrConflict) {
		old, readErr := s.RequestByOperation(ctx, r.OperationID)
		if errors.Is(readErr, ErrNotFound) {
			return Request{}, ErrConflict
		}
		if readErr != nil {
			return Request{}, readErr
		}
		if !requestEquivalent(r, old) {
			return Request{}, ErrConflict
		}
		return old, nil
	}
	if err != nil {
		return Request{}, err
	}
	return out, nil
}
func (s *MySQLStore) Request(ctx context.Context, id string) (Request, error) {
	return s.findRequest(ctx, "id", id)
}
func (s *MySQLStore) RequestByOperation(ctx context.Context, id string) (Request, error) {
	return s.findRequest(ctx, "operation_id", id)
}
func (s *MySQLStore) findRequest(ctx context.Context, column, id string) (Request, error) {
	if err := checkedKey(ctx, id); err != nil {
		return Request{}, err
	}
	var row models.RuleReanalysisRequest
	if err := s.db.WithContext(ctx).Where(clause.Eq{Column: column, Value: id}).Take(&row).Error; err != nil {
		return Request{}, qualityDBError(err)
	}
	return readRequest(row)
}
func (s *MySQLStore) mutateRequest(ctx context.Context, id string, rev uint64, start bool, status, input string) (Request, error) {
	if err := checkedKey(ctx, id); err != nil {
		return Request{}, err
	}
	var out Request
	err := s.transaction(ctx, nil, func(tx *gorm.DB) error {
		var row models.RuleReanalysisRequest
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).Take(&row).Error; err != nil {
			return err
		}
		old, err := readRequest(row)
		if err != nil {
			return err
		}
		now, err := qualityNow(tx)
		if err != nil {
			return err
		}
		out, err = changeRequest(old, rev, start, status, input, now)
		if err != nil {
			return err
		}
		row, err = requestRow(out)
		if err != nil {
			return err
		}
		return checkedQualityWrite(tx.Model(&models.RuleReanalysisRequest{}).Where("id = ? AND revision = ? AND status = ?", id, rev, old.Status).Updates(map[string]any{"status": out.Status, "revision": out.Revision, "updated_at": out.UpdatedAt, "payload_json": row.PayloadJSON, "payload_sha256": row.PayloadSHA256}))
	})
	if err != nil {
		return Request{}, err
	}
	return out, nil
}
func (s *MySQLStore) StartRequest(ctx context.Context, id string, rev uint64, inputSHA string) (Request, error) {
	return s.mutateRequest(ctx, id, rev, true, "", inputSHA)
}

// Interrupted requires the host to have confirmed its previous worker stopped.
// A read after an uncertain StartRequest never authorizes another model call.
func (s *MySQLStore) FinishRequest(ctx context.Context, id string, rev uint64, status, result string) (Request, error) {
	return s.mutateRequest(ctx, id, rev, false, status, result)
}
func (s *MySQLStore) PruneObservations(ctx context.Context, before time.Time, limit int) (int64, error) {
	if err := checkedPrune(ctx, before, limit); err != nil {
		return 0, err
	}
	var n int64
	err := s.transaction(ctx, nil, func(tx *gorm.DB) error {
		var rows []models.RuleObservation
		if err := tx.Select("sequence").Clauses(clause.Locking{Strength: "UPDATE"}).Where("created_at < ?", before.UTC()).Order("created_at ASC, sequence ASC").Limit(limit).Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		ids := make([]uint64, len(rows))
		for i, r := range rows {
			ids[i] = r.Sequence
		}
		res := tx.Where("sequence IN ?", ids).Delete(&models.RuleObservation{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != int64(len(ids)) {
			return ErrConflict
		}
		n = res.RowsAffected
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}
