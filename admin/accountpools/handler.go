package accountpools

import (
	"context"
	_ "embed"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/engine"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"net/http"
)

//go:embed static/account_pools.js
var Script string

type Policy interface {
	DeletePool(context.Context, account.PoolKey, uint64) error
}
type Runtime interface {
	AccountStatus() []account.Status
	Site(string) (core.Site, bool)
	RuntimeContext(context.Context, string) context.Context
	PreviewCompensation(context.Context, engine.CompensationFilter) (engine.CompensationPreview, error)
	ReleaseTaskCompensation(context.Context, uint, uint64) error
	BatchCompensate(context.Context, []engine.CompensationRequest) ([]engine.CompensationResult, error)
}
type Handler struct {
	Pools        *account.Pools
	Policy       Policy
	Runtime      Runtime
	IdentityView func(context.Context, string) (map[string]any, error)
	Audit        func(*http.Request, string, string, string, error)
}

func (a *Handler) runtimeAudit(r *http.Request, t, action, id string, err error) {
	if a.Audit != nil {
		a.Audit(r, t, action, id, err)
	}
}
