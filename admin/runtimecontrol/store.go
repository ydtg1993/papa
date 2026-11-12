package runtimecontrol

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/ydtg1993/papa/v4/admin/configstore"
	domain "github.com/ydtg1993/papa/v4/internal/runtimecontrol"
	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/gorm"
)

// StoreAdapter keeps the existing public configstore API out of the service.
type StoreAdapter struct{ Store *configstore.Store }

func (a StoreAdapter) Load(ctx context.Context) (json.RawMessage, uint64, error) {
	return a.Store.LoadVersioned(ctx, configstore.KeyRuntimePolicy)
}
func (a StoreAdapter) Save(ctx context.Context, d Document, r uint64, operator string) (uint64, error) {
	v, err := a.Store.SaveCAS(ctx, configstore.KeyRuntimePolicy, d, r, operator)
	if errors.Is(err, configstore.ErrConflict) {
		err = domain.ErrConflict
	}
	return v, err
}
func (a StoreAdapter) History(ctx context.Context) ([]models.RuntimePolicyHistory, error) {
	return a.Store.History(ctx, configstore.KeyRuntimePolicy)
}
func (a StoreAdapter) HistoryRevision(ctx context.Context, r uint64) (models.RuntimePolicyHistory, error) {
	v, err := a.Store.HistoryRevision(ctx, configstore.KeyRuntimePolicy, r)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err = domain.ErrNotFound
	}
	return v, err
}
