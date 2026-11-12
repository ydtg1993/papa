package browser

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/proto"
	"github.com/ydtg1993/papa/v4/pkg/governance"
	"github.com/ysmood/gson"
)

// Exercise the same response observer and admission controller used by CDP
// interception, without requiring a Chromium binary or an external website.
func TestBrowserRetryAfterWithLocalRequestPolicyDisabled(t *testing.T) {
	c, err := governance.New(governance.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	g := &pageGovernance{controller: c, site: "fixture", active: map[proto.NetworkRequestID]*browserExchange{
		"limited": {started: time.Now(), release: func(governance.Feedback) {}},
	}}
	before := time.Now()
	g.observe("limited", &proto.NetworkResponse{Status: 429, Headers: proto.NetworkHeaders{"rEtRy-AfTeR": gson.New("60")}})
	if deadline := c.RetryAfterUntil("fixture"); deadline.Before(before.Add(60 * time.Second)) {
		t.Fatalf("browser lost remote deadline: %v", deadline)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if release, err := c.AcquireRequest(ctx, "fixture"); !errors.Is(err, context.DeadlineExceeded) {
		if release != nil {
			release(governance.Feedback{})
		}
		t.Fatalf("browser request admitted early: %v", err)
	}
	g.observe("limited", &proto.NetworkResponse{Status: 429})
	g.finish("limited", false)
	if snap := c.Snapshot().Sites[0]; snap.RateLimited != 1 || snap.Errors != 1 {
		t.Fatalf("duplicate browser response observation: %+v", snap)
	}
	if c.Effective("fixture").Request.AutoThrottle.Enabled {
		t.Fatal("AutoThrottle enabled implicitly")
	}
}
