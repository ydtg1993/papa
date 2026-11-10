package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/internal/breaker"
	"github.com/ydtg1993/papa/v4/obs/incident"
	"github.com/ydtg1993/papa/v4/pkg/events"
)

func TestBreakerIncidentFrozenAcrossResume(t *testing.T) {
	e := newTestEngine(t, newFakeTaskDB())
	col := &collectorEmitter{}
	e.SetEventsEmitter(col)
	e.SetSiteBreaker("a", breaker.Config{Enabled: true, Window: time.Minute, Threshold: 2})
	fail := func(msg string) {
		e.notifyFailure(context.Background(), &Task{Site: "a", Stage: "detail", ID: 1, URL: "https://example.test?token=hidden"}, errors.New(msg))
	}
	fail("HTTP 429")
	fail("HTTP 429")
	first := e.BreakerIncidents("a")
	if len(first) != 1 || len(first[0].Samples) != 2 || strings.Contains(first[0].Samples[0].URL, "hidden") {
		t.Fatalf("capture: %+v", first)
	}
	if !e.ResumeSite("a") {
		t.Fatal("resume")
	}
	fail("HTTP 403")
	fail("HTTP 403")
	all := e.BreakerIncidents("a")
	if len(all) != 2 || all[0].ID == all[1].ID || len(all[0].Samples) != 2 || all[0].ResumedAt.IsZero() {
		t.Fatalf("captures after resume: %+v", all)
	}
	if incident.Analyze(all[1]).Groups[0].Category != "HTTP 429" || incident.Analyze(all[0]).Groups[0].Category != "HTTP 403" {
		t.Fatal("old or new evidence mixed")
	}
	all[1].Samples[0].Error = "mutated"
	if e.BreakerIncidents("a")[1].Samples[0].Error == "mutated" {
		t.Fatal("mutable capture escaped")
	}
	for _, ev := range col.snapshot() {
		if ev.Type == events.TypeBreakerTripped && (ev.Attrs["window_ns"] == "" || ev.Attrs["window_from"] == "" || ev.Attrs["incident_id"] == "") {
			t.Fatal("historical event lacks window metadata")
		}
	}
	if len(e.BreakerIncidents("b")) != 0 {
		t.Fatal("cross-site evidence")
	}
}

func TestBreakerEvidenceRetentionAndLimit(t *testing.T) {
	e := newTestEngine(t, newFakeTaskDB())
	e.SetSiteBreaker("a", breaker.Config{Enabled: true, Window: time.Hour, Threshold: incident.MaxSamples + 1})
	for i := 0; i <= incident.MaxSamples; i++ {
		e.recordBreakerFailure(&Task{Site: "a"}, errors.New("timeout"))
	}
	r := incident.Analyze(e.BreakerIncidents("a")[0])
	if !r.Limited || r.Complete || r.Observed != incident.MaxSamples {
		t.Fatalf("limit: %+v", r)
	}
	for i := 0; i < 40; i++ {
		site := fmt.Sprintf("s%d", i)
		e.SetSiteBreaker(site, breaker.Config{Enabled: true, Threshold: 1})
		e.recordBreakerFailure(&Task{Site: site}, errors.New("synthetic"))
	}
	if len(e.breakerIncidents) != 32 || len(e.BreakerIncidents("a")) != 0 {
		t.Fatal("global retention not bounded")
	}
	e.SetSiteBreaker("last", breaker.Config{Enabled: true, Threshold: 1})
	for i := 0; i < 8; i++ {
		e.recordBreakerFailure(&Task{Site: "last"}, errors.New("synthetic"))
		e.ResumeSite("last")
	}
	if len(e.BreakerIncidents("last")) != 4 {
		t.Fatal("per-site retention not bounded")
	}
}

func TestBreakerAnalysisConcurrentReadResumeFailure(t *testing.T) {
	e := newTestEngine(t, newFakeTaskDB())
	e.SetSiteBreaker("a", breaker.Config{Enabled: true, Threshold: 2})
	var wg sync.WaitGroup
	for worker := 0; worker < 3; worker++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				switch w {
				case 0:
					e.recordBreakerFailure(&Task{Site: "a"}, errors.New("timeout"))
				case 1:
					e.ResumeSite("a")
				case 2:
					for _, c := range e.BreakerIncidents("a") {
						incident.Analyze(c)
					}
				}
			}
		}(worker)
	}
	wg.Wait()
}
