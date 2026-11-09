package temporary

import (
	"context"
	"errors"
)

var ErrInvalidCleanup = errors.New("temporary: invalid cleanup request")

type CleanupRequest struct{ ID, Actor string }

// CleanupStore keeps business-owned state and row deletion out of the framework.
// BeginCleanup persists intent before returning; done means cleanup committed.
// FinishCleanup atomically deletes owned data and marks completion. PendingCleanup
// returns only previously authorized, unfinished deletions, never running work.
type CleanupStore interface {
	BeginCleanup(context.Context, string, string) (done bool, err error)
	FinishCleanup(context.Context, string) error
	PendingCleanup(context.Context) ([]CleanupRequest, error)
}

// Cleaner is synchronous; callers must drain work and serialize access for an ID.
// It never approves publication, interrupts jobs, or starts collection.
type Cleaner struct {
	Files Files
	Store CleanupStore
}

func (c Cleaner) Cleanup(ctx context.Context, id, actor string) error {
	if c.Store == nil || !ValidID(id) || actor == "" {
		return ErrInvalidCleanup
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	done, err := c.Store.BeginCleanup(ctx, id, actor)
	if err != nil || done {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = c.Files.Remove(id); err != nil {
		return err
	}
	return c.Store.FinishCleanup(ctx, id)
}

func (c Cleaner) Recover(ctx context.Context) error {
	if c.Store == nil {
		return ErrInvalidCleanup
	}
	rows, err := c.Store.PendingCleanup(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err = c.Cleanup(ctx, row.ID, row.Actor); err != nil {
			return err
		}
	}
	return nil
}
