package execution

import (
	"context"
	"errors"
	"time"

	"github.com/ydtg1993/papa/v4/internal/taskstore"
	"gorm.io/gorm"
)

// ConfirmSuccess preserves completed business work when cancellation races its
// idempotent, version-guarded persistence. Normal retries keep the caller's
// lifetime; only cancellation grants one bounded final attempt. This must not
// wrap business handlers, additive writes, or failed/cancelled business work.
func ConfirmSuccess(ctx context.Context, persist func(context.Context) error) error {
	if ctx.Err() == nil {
		err := persist(ctx)
		if err == nil || ctx.Err() == nil || errors.Is(err, taskstore.ErrChanged) || errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	return persist(finishCtx)
}
