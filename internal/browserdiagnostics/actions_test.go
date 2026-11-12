package browserdiagnostics

import (
	"context"
	"errors"
	identityruntime "github.com/ydtg1993/papa/v4/pkg/browser/identity/runtime"
	"testing"
)

func TestIdentityActionsValidateAndCASWithoutHTTP(t *testing.T) {
	s := &Service{}
	s.Configure(Options{HasSite: func(site string) bool { return site == "fixture" }})
	if err := s.Initialize(identityruntime.NewMemoryStore()); err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown(context.Background())
	ctx := context.Background()
	p := diagnosticTestProfile()
	result, err := s.Apply(ctx, Input{Action: "profile", Profile: p})
	if err != nil || !result.Attempted {
		t.Fatalf("profile append: %v", err)
	}
	result, err = s.Apply(ctx, Input{Action: "profile", Profile: p})
	if !errors.Is(err, identityruntime.ErrConflict) || !result.Attempted {
		t.Fatalf("stale profile append: %v", err)
	}
	subject := identityruntime.Subject{Site: "fixture", Kind: "session", Key: "stable"}
	if _, err := s.Apply(ctx, Input{Action: "stage", Subject: subject, Reference: p.Reference()}); err != nil {
		t.Fatal(err)
	}
	bad := subject
	bad.Site = "other"
	result, err = s.Apply(ctx, Input{Action: "diagnose", Subject: bad, Reference: p.Reference()})
	var invalid *InvalidInput
	if !errors.As(err, &invalid) || result.Attempted {
		t.Fatalf("cross-site request reached execution: %v", err)
	}
}
