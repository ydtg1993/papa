package engine

import (
	"context"
	"errors"

	"github.com/go-rod/rod"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

// AccountPoolReferences counts every task that can execute again. Successful,
// non-polling history does not prevent deleting a disabled pool.
func (e *Engine) AccountPoolReferences(ctx context.Context, key account.PoolKey) (int64, error) {
	var n int64
	// Permanent failures cannot execute again, including through polling.
	err := e.db.WithContext(ctx).Model(&models.CrawlerTask{}).Where("site = ? AND account_pool = ?", key.Site, key.Name).
		Where("status IN ? OR repeatable = ?", []models.TaskStatus{models.TaskStatusPending, models.TaskStatusProcessing, models.TaskStatusFailed}, models.RepeatableYes).
		Where("compensation_mode <> ?", string(core.CompensationPermanent)).Count(&n).Error
	return n, err
}

// PrepareAccount waits before task claim/global execution admission. It holds no
// account permit; actual exchanges reuse Manager's existing concurrency budget.
func (e *Engine) PrepareAccount(ctx context.Context, site, pool string) (context.Context, error) {
	return e.fetchResources.PrepareAccount(e.RuntimeContext(ctx, site), site, pool)
}

func (e *Engine) FetchPoolHTTP(ctx context.Context, key account.PoolKey, req htmlfetch.Request) (*htmlfetch.Response, error) {
	ctx = e.RuntimeContext(ctx, key.Site)
	m := e.GetAccountManager()
	if m == nil {
		return nil, errors.New("account manager is not configured")
	}
	r, err := m.DoPool(ctx, key, req)
	data := map[string]any{"account_pool": key.Name}
	if a, ok := account.AttemptFromContext(ctx); ok {
		data["account_id"] = a.Account.ID
		data["account_selection_version"] = a.SelectionVersion
	}
	if r != nil {
		data["status_code"] = r.StatusCode
		data["body_bytes"] = len(r.Body)
	}
	if err != nil {
		traceFromCtx(ctx).Fail("账号池 HTTP 请求", errors.New("account pool exchange failed"), data)
	} else {
		traceFromCtx(ctx).Step("账号池 HTTP 请求", data)
	}
	return r, err
}
func (e *Engine) FetchPoolHTML(ctx context.Context, key account.PoolKey, rawURL string) (*htmlfetch.Page, error) {
	ctx = e.RuntimeContext(ctx, key.Site)
	m := e.GetAccountManager()
	if m == nil {
		return nil, errors.New("account manager is not configured")
	}
	return m.FetchPoolHTML(ctx, key, rawURL)
}
func (e *Engine) WithPoolBrowser(ctx context.Context, key account.PoolKey, fn func(*rod.Page) error) error {
	ctx = e.RuntimeContext(ctx, key.Site)
	m := e.GetAccountManager()
	if m == nil {
		return errors.New("account manager is not configured")
	}
	return m.WithPoolBrowser(ctx, key, fn)
}
