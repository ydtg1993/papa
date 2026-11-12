package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ydtg1993/papa/v4/internal/taskstore"
	"strings"
	"time"

	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/redact"
	"gorm.io/gorm"
)

var ErrCompensationBlocked = errors.New("task compensation is blocked")

// compensationScope is shared by claims and every durable requeue. Readiness
// is checked without side effects before requeue; Acquire rechecks it before
// execution. The SQL predicate protects time/manual/permanent state against
// concurrent release, stale queue snapshots and process recovery.
func compensationScope(db *gorm.DB) *gorm.DB { return taskstore.CompensationScope(db) }

func compensationVersionScope(db *gorm.DB, t *models.CrawlerTask) *gorm.DB {
	return compensationScope(db).Where("compensation_version = ?", t.CompensationVersion)
}

func boundedCompensationReason(reason string) string {
	r := []rune(redact.Text(reason))
	if len(r) > 512 {
		r = r[:512]
	}
	return string(r)
}

func failureCompensation(err error) (string, core.CompensationPolicy) {
	kind := core.ErrorKind(err)
	policy, explicit := core.CompensationOf(err)
	if explicit {
		// Normalize policies supplied by business error implementations as well.
		wrapped := core.WithCompensation(err, policy)
		policy, _ = core.CompensationOf(wrapped)
	} else if account.IsAttemptFailure(err) {
		policy.Mode = core.CompensationAccountReady
		switch {
		case errors.Is(err, account.ErrNeedsVerify):
			kind = "account_needs_verify"
		case errors.Is(err, account.ErrChallenge):
			kind = "account_manual_verification"
		case errors.Is(err, account.ErrFixedProxyUnavailable):
			kind = "account_proxy_unavailable"
		case errors.Is(err, account.ErrExpired):
			kind = "account_expired"
		case errors.Is(err, account.ErrBudget):
			kind = "account_usage_wait"
		default:
			kind = "account_unavailable"
		}
	}
	if policy.Reason == "" && err != nil {
		policy.Reason = err.Error()
	}
	policy.Reason = boundedCompensationReason(policy.Reason)
	if value := []rune(redact.Text(kind)); len(value) > 64 {
		kind = string(value[:64])
	} else {
		kind = string(value)
	}
	return kind, policy
}

func compensationStatusUpdates(status models.TaskStatus, cause error) map[string]any {
	u := map[string]any{}
	if status != models.TaskStatusFailed && status != models.TaskStatusSuccess {
		return u
	}
	u["compensation_version"] = gorm.Expr("compensation_version + 1")
	u["failure_kind"], u["compensation_mode"], u["compensation_reason"] = "", "", ""
	u["next_compensation_at"] = nil
	if status == models.TaskStatusFailed {
		kind, policy := failureCompensation(cause)
		u["failure_kind"], u["compensation_mode"], u["compensation_reason"] = kind, string(policy.Mode), policy.Reason
		if !policy.NotBefore.IsZero() {
			u["next_compensation_at"] = policy.NotBefore
		}
	}
	return u
}

// compensationBlockedReason does not log in, select a replacement account or
// consume a reprocess generation. Pending startup recovery may still enqueue
// an account-bound task: its normal pre-claim Acquire waits without a permit.
func (e *Engine) compensationBlockedReason(t *models.CrawlerTask, now time.Time) string {
	if reason := compensationConditionReason(t, now); reason != "" {
		return reason
	}
	if t.AccountPool != "" {
		m := e.GetAccountManager()
		if m == nil || m.Pools() == nil {
			return "account pools are not configured"
		}
		if ready, reason := m.Pools().Readiness(account.PoolKey{Site: t.Site, Name: t.AccountPool}); !ready {
			if reason == "" {
				reason = "account pool is not ready"
			}
			return boundedCompensationReason(reason)
		}
	}
	return ""
}

func compensationConditionReason(t *models.CrawlerTask, now time.Time) string {
	switch core.CompensationMode(t.CompensationMode) {
	case core.CompensationManual:
		return "manual intervention required"
	case core.CompensationPermanent:
		return "permanently terminated"
	case core.CompensationAutomatic, core.CompensationRetryAfter, core.CompensationAccountReady:
	default:
		return "unknown compensation condition"
	}
	if t.NextCompensationAt != nil && t.NextCompensationAt.After(now) {
		return "retry-after deadline has not elapsed"
	}
	if t.CompensationMode == string(core.CompensationAccountReady) && t.AccountPool == "" {
		return "account-ready compensation requires task pool ownership"
	}
	return ""
}

// AccountAttemptRecord is credential-free and retained for the latest eight
// claimed task rounds. At marks the beginning of that round, not a full audit.
type AccountAttemptRecord struct {
	AccountID         string    `json:"account_id"`
	SelectionVersion  uint64    `json:"selection_version"`
	CredentialVersion uint64    `json:"credential_version"`
	At                time.Time `json:"at"`
	Repeat            int       `json:"repeat"`
	Reprocess         int       `json:"reprocess"`
}

const AccountAttemptHistoryLimit = 8

func (e *Engine) persistAccountAttempt(ctx context.Context, task *Task, version taskRunVersion) error {
	a, ok := account.AttemptFromContext(ctx)
	if !ok || task.ID == 0 {
		return nil
	}
	var row models.CrawlerTask
	if err := e.retryTaskState(ctx, task, "read account association", func() error {
		return e.db.WithContext(ctx).Select("account_attempts").Where("id = ?", task.ID).Take(&row).Error
	}); err != nil {
		return err
	}
	var history []AccountAttemptRecord
	if len(row.AccountAttempts) > 0 && json.Unmarshal(row.AccountAttempts, &history) != nil {
		return errors.New("invalid persisted account association history")
	}
	entry := AccountAttemptRecord{AccountID: a.Account.ID, SelectionVersion: a.SelectionVersion, CredentialVersion: a.CredentialVersion, At: time.Now().UTC(), Repeat: version.Repeat, Reprocess: version.Reprocess}
	history = append(history, entry)
	if len(history) > AccountAttemptHistoryLimit {
		history = history[len(history)-AccountAttemptHistoryLimit:]
	}
	raw, err := json.Marshal(history)
	if err != nil {
		return err
	}
	return e.retryTaskState(ctx, task, "persist account association", func() error {
		res := terminalScope(e.db.WithContext(ctx), task, version).Updates(map[string]any{
			"last_account_id": a.Account.ID, "last_account_selection_version": a.SelectionVersion, "account_attempts": string(raw),
		})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrTaskChanged
		}
		return nil
	})
}

type CompensationFilter struct {
	Site        string `json:"site"`
	AccountPool string `json:"account_pool"`
	AccountID   string `json:"account_id"`
	Limit       int    `json:"limit"`
}

type CompensationPreviewItem struct {
	ID                 uint                   `json:"id"`
	Site               string                 `json:"site"`
	Stage              string                 `json:"stage"`
	AccountPool        string                 `json:"account_pool"`
	Status             models.TaskStatus      `json:"status"`
	FailureKind        string                 `json:"failure_kind"`
	Mode               string                 `json:"mode"`
	Reason             string                 `json:"reason"`
	NextCompensationAt *time.Time             `json:"next_compensation_at"`
	Version            uint64                 `json:"version"`
	Reprocess          int                    `json:"reprocess"`
	Eligible           bool                   `json:"eligible"`
	BlockedReason      string                 `json:"blocked_reason,omitempty"`
	AccountAttempts    []AccountAttemptRecord `json:"account_attempts"`
}

type CompensationPreview struct {
	Items        []CompensationPreviewItem `json:"items"`
	Eligible     int                       `json:"eligible"`
	Blocked      int                       `json:"blocked"`
	Truncated    bool                      `json:"truncated"`
	HistoryLimit int                       `json:"history_limit"`
}

// PreviewCompensation returns up to 200 failed tasks (default 50). Counts cover
// returned items only; Truncated indicates additional matching rows. AccountID
// matches only the latest eight persisted round associations, never full history.
func (e *Engine) PreviewCompensation(ctx context.Context, filter CompensationFilter) (CompensationPreview, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	q := e.db.WithContext(ctx).Model(&models.CrawlerTask{}).Where("status = ?", models.TaskStatusFailed)
	if filter.Site != "" {
		q = q.Where("site = ?", filter.Site)
	}
	if filter.AccountPool != "" {
		q = q.Where("account_pool = ?", filter.AccountPool)
	}
	if filter.AccountID != "" {
		q = q.Where("last_account_id = ? OR JSON_CONTAINS(JSON_EXTRACT(account_attempts, '$[*].account_id'), JSON_QUOTE(?))", filter.AccountID, filter.AccountID)
	}
	var rows []models.CrawlerTask
	if err := q.Order("id DESC").Limit(limit + 1).Find(&rows).Error; err != nil {
		return CompensationPreview{}, err
	}
	out := CompensationPreview{Items: make([]CompensationPreviewItem, 0, min(limit, len(rows))), HistoryLimit: AccountAttemptHistoryLimit, Truncated: len(rows) > limit}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	for _, row := range rows {
		reason := e.compensationBlockedReason(&row, time.Now())
		if reason == "" && e.stages[stageKeyOfRecord(&row)] == nil {
			reason = "task stage is not registered"
		}
		item := CompensationPreviewItem{ID: row.ID, Site: row.Site, Stage: row.Stage, AccountPool: row.AccountPool, Status: row.Status, FailureKind: row.FailureKind, Mode: row.CompensationMode, Reason: row.CompensationReason, NextCompensationAt: row.NextCompensationAt, Version: row.CompensationVersion, Reprocess: row.Reprocess, Eligible: reason == "", BlockedReason: reason, AccountAttempts: []AccountAttemptRecord{}}
		if len(row.AccountAttempts) > 0 {
			_ = json.Unmarshal(row.AccountAttempts, &item.AccountAttempts)
		}
		if item.Eligible {
			out.Eligible++
		} else {
			out.Blocked++
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

// ReleaseTaskCompensation releases only explicit manual waiting. It never
// revives permanent failures, clears the original error, or starts a task.
func (e *Engine) ReleaseTaskCompensation(ctx context.Context, id uint, wasVersion uint64) error {
	res := e.db.WithContext(ctx).Model(&models.CrawlerTask{}).
		Where("id = ? AND status = ? AND compensation_mode = ? AND compensation_version = ?", id, models.TaskStatusFailed, string(core.CompensationManual), wasVersion).
		Updates(map[string]any{"compensation_mode": "", "compensation_reason": "manual wait released", "compensation_version": gorm.Expr("compensation_version + 1")})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrTaskChanged
	}
	return nil
}

type CompensationRequest struct {
	ID      uint   `json:"id"`
	Version uint64 `json:"version"`
}
type CompensationResult struct {
	ID        uint   `json:"id"`
	Submitted bool   `json:"submitted"`
	Error     string `json:"error,omitempty"`
}

// BatchCompensate revalidates each preview version and returns individual results.
// At most 200 items are accepted; retries cannot bypass manual/time/readiness.
func (e *Engine) BatchCompensate(ctx context.Context, requests []CompensationRequest) ([]CompensationResult, error) {
	if len(requests) > 200 {
		return nil, errors.New("at most 200 compensation requests are allowed")
	}
	results := make([]CompensationResult, 0, len(requests))
	for _, req := range requests {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		row, err := e.loadTask(req.ID)
		if err == nil && (row.Status != models.TaskStatusFailed || row.CompensationVersion != req.Version) {
			err = ErrTaskChanged
		}
		if err == nil {
			err = e.retryTaskRecord(ctx, row, row.Reprocess, &req.Version)
		}
		result := CompensationResult{ID: req.ID, Submitted: err == nil}
		if err != nil {
			result.Error = boundedCompensationReason(err.Error())
		}
		results = append(results, result)
	}
	return results, nil
}

func blockedCompensationError(reason string) error {
	return fmt.Errorf("%w: %s", ErrCompensationBlocked, strings.TrimSpace(reason))
}
