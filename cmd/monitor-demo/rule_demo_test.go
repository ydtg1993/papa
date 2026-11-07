package main

import (
	"context"
	"errors"
	"github.com/ydtg1993/papa/v4/pkg/rulestore"
	"sync"
	"sync/atomic"
	"testing"
)

func TestDemoRuleOperationsAndIsolation(t *testing.T) {
	d := &demoRules{}
	ctx := context.Background()
	p := rulestore.RulePlan{Kind: "publish", Site: "shop", Generation: "1", Bindings: map[string]rulestore.Reference{"list": {ID: "demo-catalog", Version: "v2"}}}
	preview, err := d.Preview(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	actor := rulestore.Actor{Operator: "test", Source: "oa"}
	request := rulestore.RuleChangeRequest{RulePlan: p, OperationID: "test", VerificationIDs: preview.VerificationIDs}
	var wg sync.WaitGroup
	var passed atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := d.Change(ctx, request, actor)
			if err == nil && r.Generation == "2" {
				passed.Add(1)
			} else {
				t.Errorf("idempotent demo write: %+v %v", r, err)
			}
		}()
	}
	wg.Wait()
	if passed.Load() != 8 {
		t.Fatal("demo retry failed")
	}
	page, _ := d.Releases(ctx, "shop", 0)
	if len(page.Items) != 2 {
		t.Fatal("duplicate demo release")
	}
	page.Current.Bindings["bad"] = rulestore.Reference{ID: "bad", Version: "1"}
	again, _ := d.Releases(ctx, "shop", 0)
	if len(again.Current.Bindings) != 1 {
		t.Fatal("demo leaked mutable map")
	}
	request.Generation = "1"
	request.OperationID = "stale"
	if _, err := d.Change(ctx, request, actor); !errors.Is(err, rulestore.ErrGenerationConflict) {
		t.Fatal(err)
	}
	back, err := d.Change(ctx, rulestore.RuleChangeRequest{RulePlan: rulestore.RulePlan{Kind: "rollback", Site: "shop", Generation: "2"}, OperationID: "back"}, actor)
	if err != nil || back.Bindings["list"].Version != "v1" || back.PreviousReleaseID != "" {
		t.Fatalf("rollback: %+v %v", back, err)
	}
}
