package engine

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/core"
)

func TestSiteDeclarationSnapshotsOwnMutableValues(t *testing.T) {
	e := &Engine{}
	s := core.Site{Key: "fixture", Headers: map[string]string{"X-Test": "original"}, RestrictedKeywords: []string{"blocked"}, StartPaused: true}
	e.SetSite(s)
	s.Headers["X-Test"] = "input-mutated"
	s.RestrictedKeywords[0] = "input-mutated"
	got, ok := e.Site(s.Key)
	if !ok || got.Headers["X-Test"] != "original" || got.RestrictedKeywords[0] != "blocked" {
		t.Fatalf("registration retained caller-owned values: %+v", got)
	}
	got.Headers["X-Test"] = "output-mutated"
	got.RestrictedKeywords[0] = "output-mutated"
	h := e.siteHeaders(s.Key)
	h["X-Test"] = "request-mutated"
	again, _ := e.Site(s.Key)
	if again.Headers["X-Test"] != "original" || again.RestrictedKeywords[0] != "blocked" {
		t.Fatalf("snapshot leaked catalog ownership: %+v", again)
	}
	if keys := e.startPausedSites(); len(keys) != 1 || keys[0] != s.Key {
		t.Fatalf("startup pause projection: %v", keys)
	}
}

func TestSiteDeclarationConcurrentPublication(t *testing.T) {
	e := &Engine{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprint(i)
			for n := 0; n < 50; n++ {
				e.SetSite(core.Site{Key: key, Headers: map[string]string{"X-Test": key}})
				got, ok := e.Site(key)
				if !ok || got.Headers["X-Test"] != key {
					t.Errorf("inconsistent declaration: %+v", got)
				}
				e.siteKeys()
			}
		}(i)
	}
	wg.Wait()
	if len(e.siteKeys()) != 8 {
		t.Fatal("declaration lost")
	}
}

func TestAccountAlertGatePreservesWindowAndBound(t *testing.T) {
	var gate accountAlertGate
	now := time.Unix(1000, 0)
	if !gate.allow("failure", now) || gate.allow("failure", now.Add(time.Second)) || !gate.allow("recovery", now.Add(time.Second)) || !gate.allow("failure", now.Add(time.Minute)) {
		t.Fatal("per-kind notification window changed")
	}
	for i := 0; i < 2050; i++ {
		gate.allow(fmt.Sprint(i), now.Add(time.Duration(i+120)*time.Second))
	}
	if len(gate.last) != 2048 {
		t.Fatalf("unbounded gate: %d", len(gate.last))
	}
	if !gate.allow("recovery", now.Add(time.Hour)) {
		t.Fatal("expired key retained")
	}
}
