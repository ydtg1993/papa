package crawlrule_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
)

func TestRegistryVerifiedRequiresSuccessfulActivation(t *testing.T) {
	var registry crawlrule.Registry
	rule := htmlRule()
	for _, pair := range [][2]string{{rule.ID, rule.Version}, {"missing", rule.Version}} {
		if c, err := registry.Verified(pair[0], pair[1]); c != nil || !errors.Is(err, crawlrule.ErrVersionNotFound) {
			t.Fatalf("missing version returned %v, %v", c, err)
		}
	}
	if err := registry.Register(rule); err != nil {
		t.Fatal(err)
	}
	candidate, err := registry.Get(rule.ID, rule.Version)
	if err != nil {
		t.Fatal(err)
	}
	assertUnverified := func() {
		t.Helper()
		if c, err := registry.Verified(rule.ID, rule.Version); c != nil || !errors.Is(err, crawlrule.ErrUnverifiedRule) {
			t.Fatalf("candidate became executable: %v, %v", c, err)
		}
	}
	assertUnverified()
	if _, err := candidate.Verify([]crawlrule.Fixture{fixture("A")}); err != nil {
		t.Fatal(err)
	}
	assertUnverified() // Local verification is not registry publication.
	if _, err := registry.Activate(rule.ID, rule.Version, nil); !errors.Is(err, crawlrule.ErrNoFixtures) {
		t.Fatalf("missing fixtures: %v", err)
	}
	assertUnverified()
	bad := fixture("A")
	bad.Expect.Fields[0].Value = "different"
	if _, err := registry.Activate(rule.ID, rule.Version, []crawlrule.Fixture{bad}); !errors.Is(err, crawlrule.ErrFixtureFailed) {
		t.Fatalf("failed fixture activation: %v", err)
	}
	assertUnverified()
	if _, err := registry.Activate(rule.ID, rule.Version, []crawlrule.Fixture{fixture("A")}); err != nil {
		t.Fatal(err)
	}
	if c, err := registry.Verified(rule.ID, rule.Version); err != nil || c != candidate {
		t.Fatalf("successful activation did not publish immutable version: %v, %v", c, err)
	}
	if c, err := registry.Verified(rule.ID, "missing"); c != nil || !errors.Is(err, crawlrule.ErrVersionNotFound) {
		t.Fatalf("unknown version returned %v, %v", c, err)
	}
}

func TestRegistryVerifiedRetainsPinnedVersionsAfterRollback(t *testing.T) {
	registry := crawlrule.NewRegistry()
	for _, version := range []string{"v1", "v2", "v3"} {
		rule := htmlRule()
		rule.Version = version
		if err := registry.Register(rule); err != nil {
			t.Fatal(err)
		}
		if _, err := registry.Activate(rule.ID, version, []crawlrule.Fixture{fixture("A")}); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{"v2", "v1"} {
		active, err := registry.Rollback("catalog")
		if err != nil || active.Rule().Version != want {
			t.Fatalf("rollback wanted %s: %v, %v", want, active, err)
		}
	}
	// A rejected attempt to reactivate a rolled-back version does not revoke its
	// earlier verification or modify the current activation history.
	if _, err := registry.Activate("catalog", "v3", nil); !errors.Is(err, crawlrule.ErrNoFixtures) {
		t.Fatalf("reactivation without fixtures: %v", err)
	}
	for _, version := range []string{"v1", "v2", "v3"} {
		c, err := registry.Verified("catalog", version)
		if err != nil || c.Rule().Version != version {
			t.Fatalf("pinned version %s unavailable after rollback: %v, %v", version, c, err)
		}
		out, err := c.Evaluate(response("<article><h2>A</h2></article>"))
		if err != nil || out.Records[0]["title"] != "A" {
			t.Fatalf("pinned version %s cannot execute: %+v, %v", version, out, err)
		}
	}
	active, err := registry.Active("catalog")
	if err != nil || active.Rule().Version != "v1" {
		t.Fatalf("failed reactivation changed active version: %v, %v", active, err)
	}
	if _, err := registry.Rollback("catalog"); !errors.Is(err, crawlrule.ErrNoRollback) {
		t.Fatalf("failed reactivation changed history: %v", err)
	}
}

func TestRegistryVerifiedConcurrentPublicationAndRollback(t *testing.T) {
	registry := crawlrule.NewRegistry()
	const workers = 12
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			rule := htmlRule()
			rule.Version = fmt.Sprintf("v%d", i)
			if err := registry.Register(rule); err != nil {
				t.Error(err)
				return
			}
			if _, err := registry.Verified(rule.ID, rule.Version); !errors.Is(err, crawlrule.ErrUnverifiedRule) {
				t.Errorf("unpublished version %s: %v", rule.Version, err)
				return
			}
			for range 20 {
				if _, err := registry.Activate(rule.ID, rule.Version, []crawlrule.Fixture{fixture("A")}); err != nil {
					t.Error(err)
					return
				}
				c, err := registry.Verified(rule.ID, rule.Version)
				if err != nil || c.Rule().Version != rule.Version {
					t.Errorf("published version %s unavailable: %v, %v", rule.Version, c, err)
					return
				}
				if _, err := registry.Rollback(rule.ID); err != nil && !errors.Is(err, crawlrule.ErrNoRollback) {
					t.Error(err)
					return
				}
			}
		}(i)
	}
	close(start)
	wg.Wait()
	for i := range workers {
		version := fmt.Sprintf("v%d", i)
		if c, err := registry.Verified("catalog", version); err != nil || c.Rule().Version != version {
			t.Fatalf("verified version %s lost during concurrent changes: %v, %v", version, c, err)
		}
	}
}
