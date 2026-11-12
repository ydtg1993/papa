package accountpools

import (
	"context"
	"errors"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/engine"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type runtimeStub struct {
	previewCalls int
	filter       engine.CompensationFilter
	releaseCalls int
}

func (*runtimeStub) AccountStatus() []account.Status                              { return nil }
func (*runtimeStub) Site(s string) (core.Site, bool)                              { return core.Site{Key: s}, s == "fixture" }
func (*runtimeStub) RuntimeContext(ctx context.Context, _ string) context.Context { return ctx }
func (s *runtimeStub) PreviewCompensation(_ context.Context, f engine.CompensationFilter) (engine.CompensationPreview, error) {
	s.previewCalls++
	s.filter = f
	return engine.CompensationPreview{}, nil
}
func (s *runtimeStub) ReleaseTaskCompensation(context.Context, uint, uint64) error {
	s.releaseCalls++
	return errors.New("stale")
}
func (*runtimeStub) BatchCompensate(context.Context, []engine.CompensationRequest) ([]engine.CompensationResult, error) {
	return nil, nil
}

type policyStub struct {
	calls    int
	key      account.PoolKey
	revision uint64
}

func (s *policyStub) DeletePool(_ context.Context, k account.PoolKey, r uint64) error {
	s.calls++
	s.key = k
	s.revision = r
	return account.ErrPoolReferenced
}
func TestPoolDeleteDelegatesToPolicyTransaction(t *testing.T) {
	p, err := account.NewPools(context.Background(), account.NewManager(htmlfetch.NewClient(htmlfetch.Config{})), account.NewMemoryPoolStore(), account.PoolOptions{ScanInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	policy := &policyStub{}
	h := &Handler{Pools: p, Runtime: &runtimeStub{}, Policy: policy}
	w := httptest.NewRecorder()
	h.PoolsAPI(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"action":"delete","revision":7,"key":{"site":"fixture","name":"primary"}}`)))
	if w.Code != 409 || policy.calls != 1 || policy.revision != 7 || policy.key.Name != "primary" || !strings.Contains(w.Body.String(), "仍被策略或任务引用") {
		t.Fatal(w.Code, w.Body.String(), policy)
	}
}
func TestCompensationPreviewValidatesLimitAndForwardsFilters(t *testing.T) {
	r := &runtimeStub{}
	h := &Handler{Runtime: r}
	for _, limit := range []string{"0", "201", "bad"} {
		w := httptest.NewRecorder()
		h.Compensation(w, httptest.NewRequest("GET", "/?limit="+limit, nil))
		if w.Code != 400 || r.previewCalls != 0 {
			t.Fatal(w.Code, r.previewCalls)
		}
	}
	w := httptest.NewRecorder()
	h.Compensation(w, httptest.NewRequest("GET", "/?limit=20&site=fixture&pool=primary&account=first", nil))
	if w.Code != 200 || r.previewCalls != 1 || r.filter.Limit != 20 || r.filter.Site != "fixture" || r.filter.AccountPool != "primary" || r.filter.AccountID != "first" {
		t.Fatal(w.Code, r.filter)
	}
}
func TestCompensationReleasePreservesConflictResponse(t *testing.T) {
	r := &runtimeStub{}
	h := &Handler{Runtime: r}
	w := httptest.NewRecorder()
	h.Compensation(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"action":"release","id":1,"version":4}`)))
	if w.Code != 409 || r.releaseCalls != 1 {
		t.Fatal(w.Code, r.releaseCalls)
	}
}
