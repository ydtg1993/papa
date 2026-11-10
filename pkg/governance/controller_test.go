package governance

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"
)

type fakeTimer struct {
	at time.Time
	ch chan time.Time
}
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []fakeTimer
}

func newClock() *fakeClock          { return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)} }
func (f *fakeClock) Now() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now }
func (f *fakeClock) After(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- f.now
	} else {
		f.timers = append(f.timers, fakeTimer{f.now.Add(d), ch})
	}
	return ch
}
func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	keep := f.timers[:0]
	for _, timer := range f.timers {
		if !timer.at.After(f.now) {
			timer.ch <- f.now
		} else {
			keep = append(keep, timer)
		}
	}
	f.timers = keep
}
func eventually(t *testing.T, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !pred() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not become true")
		}
		runtime.Gosched()
	}
}
func controller(t *testing.T, config Config, clock Clock) *Controller {
	t.Helper()
	c, err := New(config, WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func requestConfig() Config { c := DefaultConfig(); c.Defaults.Request.Enabled = true; return c }
func resourceConfig() Config {
	c := DefaultConfig()
	c.Resources.Enabled = true
	c.Resources.TaskMax = 1
	c.Resources.BrowserMax = 1
	return c
}
func ptr[T any](v T) *T { return &v }

type requestResult struct {
	release func(Feedback)
	err     error
}

func asyncRequest(c *Controller, ctx context.Context, site string) <-chan requestResult {
	ch := make(chan requestResult, 1)
	go func() { r, e := c.AcquireRequest(ctx, site); ch <- requestResult{r, e} }()
	return ch
}
func takeRequest(t *testing.T, ch <-chan requestResult) func(Feedback) {
	t.Helper()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatal(r.err)
		}
		return r.release
	case <-time.After(3 * time.Second):
		t.Fatal("request never admitted")
		return nil
	}
}
func assertRequestBlocked(t *testing.T, ch <-chan requestResult) {
	t.Helper()
	select {
	case r := <-ch:
		if r.release != nil {
			r.release(Feedback{})
		}
		t.Fatalf("unexpected admission: %v", r.err)
	default:
	}
}

func TestExplicitOverridesAndSnapshotIsolation(t *testing.T) {
	cfg := requestConfig()
	cfg.Defaults.Request.ManualDelay = time.Second
	cfg.Defaults.Stages = map[string]StagePolicy{"fetch": {MaxConcurrent: 4, Delay: time.Second}}
	cfg.Sites = map[string]PolicyPatch{"a": {Request: &RequestPatch{Enabled: ptr(false), ManualDelay: ptr(time.Duration(0))}, Stages: map[string]StagePatch{"fetch": {Delay: ptr(time.Duration(0))}}}}
	c := controller(t, cfg, newClock())
	effective := c.Effective("a")
	if effective.Request.Enabled || effective.Request.ManualDelay != 0 || effective.Stages["fetch"].Delay != 0 || effective.Stages["fetch"].MaxConcurrent != 4 {
		t.Fatalf("explicit zero/false lost: %+v", effective)
	}
	packed := PatchOfPolicy(effective)
	if !reflect.DeepEqual(MergePolicy(Policy{}, packed), effective) {
		t.Fatal("patch round trip changed effective values")
	}
	*cfg.Sites["a"].Request.Enabled = true
	cfg.Defaults.Stages["fetch"] = StagePolicy{MaxConcurrent: 99}
	snap := c.Snapshot()
	snap.Sites[0].Policy.Stages["fetch"] = StagePolicy{MaxConcurrent: 88}
	if got := c.Effective("a"); got.Request.Enabled || got.Stages["fetch"].MaxConcurrent != 4 {
		t.Fatal("caller mutated active policy")
	}
	before := len(c.sites)
	c.Snapshot()
	c.Snapshot()
	if len(c.sites) != before {
		t.Fatal("snapshot created runtime states")
	}
}

func TestValidationRejectsUnsafeNumbersAndHysteresis(t *testing.T) {
	for _, modify := range []func(*Config){
		func(c *Config) { c.Defaults.Request.RequestsPerSecond = math.NaN() },
		func(c *Config) { c.Defaults.Request.AutoThrottle.TargetConcurrency = math.Inf(1) },
		func(c *Config) { c.Resources.Enabled = true; c.Resources.CPURecover = c.Resources.CPUHigh },
		func(c *Config) { c.Resources.Enabled = true; c.Resources.ShrinkFactor = 1 },
		func(c *Config) { c.Defaults.Request.Enabled = true; c.Defaults.Request.RetryAfterDefault = 0 },
		func(c *Config) { c.Defaults.Request.RequestsPerSecond = 1; c.Defaults.Request.Burst = 0 },
		func(c *Config) { c.Defaults.Stages = map[string]StagePolicy{"x": {Delay: -1}} },
	} {
		cfg := DefaultConfig()
		modify(&cfg)
		if _, err := New(cfg); err == nil {
			t.Fatal("accepted invalid policy")
		}
	}
	if _, err := New(Config{}); err != nil {
		t.Fatalf("disabled legacy defaults invalid: %v", err)
	}
}

func TestRequestSharedConcurrencyCancellationAndShrink(t *testing.T) {
	cfg := requestConfig()
	cfg.Defaults.Request.MaxInflight = 2
	c := controller(t, cfg, newClock())
	r1, _ := c.AcquireRequest(context.Background(), "a")
	r2, _ := c.AcquireRequest(context.Background(), "a")
	cfg.Defaults.Request.MaxInflight = 1
	if err := c.Update(cfg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	waiting := asyncRequest(c, ctx, "a")
	eventually(t, func() bool { return c.Snapshot().Sites[0].Waiting == 1 })
	r1(Feedback{})
	assertRequestBlocked(t, waiting)
	cancel()
	if got := <-waiting; !errors.Is(got.err, context.Canceled) {
		t.Fatalf("cancel = %v", got.err)
	}
	if snap := c.Snapshot(); snap.Sites[0].Inflight != 1 || snap.Sites[0].Waiting != 0 {
		t.Fatalf("cancel/release leaked: %+v", snap.Sites[0])
	}
	r2(Feedback{})
	r2(Feedback{})
	next := takeRequest(t, asyncRequest(c, context.Background(), "a"))
	next(Feedback{})
	if c.Snapshot().Sites[0].Inflight != 0 {
		t.Fatal("idempotent release failed")
	}
}

func TestVirtualTimeRequestRateAndSpacing(t *testing.T) {
	clock := newClock()
	cfg := requestConfig()
	cfg.Defaults.Request.RequestsPerSecond = 2
	cfg.Defaults.Request.Burst = 2
	cfg.Defaults.Request.ManualDelay = time.Second
	c := controller(t, cfg, clock)
	r, _ := c.AcquireRequest(context.Background(), "a")
	r(Feedback{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	next := asyncRequest(c, ctx, "a")
	eventually(t, func() bool { clock.mu.Lock(); defer clock.mu.Unlock(); return len(clock.timers) > 0 })
	clock.advance(500 * time.Millisecond)
	assertRequestBlocked(t, next)
	clock.advance(500 * time.Millisecond)
	takeRequest(t, next)(Feedback{})
	// Disable manual spacing explicitly: accumulated tokens permit one burst,
	// but the next request must wait for the token bucket.
	cfg.Defaults.Request.ManualDelay = 0
	if err := c.Update(cfg); err != nil {
		t.Fatal(err)
	}
	r, _ = c.AcquireRequest(ctx, "a")
	r(Feedback{})
	third := asyncRequest(c, ctx, "a")
	eventually(t, func() bool { return c.Snapshot().Sites[0].Waiting == 1 })
	clock.advance(500 * time.Millisecond) // Wake state changes as well, avoiding fake-clock timer-registration races.
	c.Observe("a", Feedback{})
	takeRequest(t, third)(Feedback{})
}

func TestAutoThrottleFailureNeverAcceleratesAndManualStillApplies(t *testing.T) {
	clock := newClock()
	cfg := requestConfig()
	a := &cfg.Defaults.Request.AutoThrottle
	a.Enabled = true
	a.MinDelay = 100 * time.Millisecond
	a.MaxDelay = 10 * time.Second
	a.TargetConcurrency = 2
	a.Smoothing = .5
	c := controller(t, cfg, clock)
	c.Observe("a", Feedback{StatusCode: 200, Latency: 2 * time.Second})
	if got := c.Snapshot().Sites[0].EffectiveDelay; got != 550*time.Millisecond {
		t.Fatalf("smoothed delay = %v", got)
	}
	c.Observe("a", Feedback{StatusCode: 500, Latency: time.Millisecond})
	if got := c.Snapshot().Sites[0].EffectiveDelay; got != 550*time.Millisecond {
		t.Fatalf("failure accelerated to %v", got)
	}
	c.Observe("a", Feedback{StatusCode: 500, Latency: 4 * time.Second})
	if got := c.Snapshot().Sites[0].EffectiveDelay; got != 1275*time.Millisecond {
		t.Fatalf("slow failure did not slow down: %v", got)
	}
	cfg.Defaults.Request.AutoThrottle.Enabled = false
	cfg.Defaults.Request.ManualDelay = 3 * time.Second
	c.Update(cfg)
	if got := c.Snapshot().Sites[0].EffectiveDelay; got != 3*time.Second {
		t.Fatalf("manual interval lost when auto disabled: %v", got)
	}
}

func TestRetryAfterParsingAndBudgetPauseDoesNotShorten(t *testing.T) {
	clock := newClock()
	cfg := requestConfig()
	cfg.Defaults.Request.RetryAfterMax = time.Minute
	c := controller(t, cfg, clock)
	for _, tc := range []struct {
		header   string
		duration time.Duration
		ok       bool
	}{{"12", 12 * time.Second, true}, {clock.Now().Add(30 * time.Second).Format(http.TimeFormat), 30 * time.Second, true}, {"0", 0, true}, {"-1", 0, false}, {"garbage", 0, false}, {"9999999999999999999999999", time.Duration(math.MaxInt64), true}} {
		d, ok := RetryAfter(tc.header, clock.Now())
		if d != tc.duration || ok != tc.ok {
			t.Fatalf("RetryAfter(%q) = %v,%v", tc.header, d, ok)
		}
	}
	c.Observe("a", Feedback{StatusCode: 429, RetryAfter: "120"})
	_, err := c.AcquireRequest(context.Background(), "a")
	if !errors.Is(err, ErrRemoteWaitExceeded) {
		t.Fatalf("oversize remote wait = %v", err)
	}
	clock.advance(time.Minute)
	c.AcknowledgeRemote("a")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := asyncRequest(c, ctx, "a")
	eventually(t, func() bool { return c.Snapshot().Sites[0].Waiting == 1 })
	assertRequestBlocked(t, ch)
	clock.advance(time.Minute)
	c.Observe("a", Feedback{})
	takeRequest(t, ch)(Feedback{})
	// A later shorter instruction must not reduce the shared cooldown.
	c.Observe("a", Feedback{StatusCode: 503, RetryAfter: "50"})
	until := c.Snapshot().Sites[0].CooldownUntil
	c.Observe("a", Feedback{StatusCode: 429, RetryAfter: "1"})
	if !c.Snapshot().Sites[0].CooldownUntil.Equal(until) {
		t.Fatal("short response shortened existing cooldown")
	}
}

func TestResourceHysteresisInvalidSamplesAndNoInFlightCancellation(t *testing.T) {
	clock := newClock()
	cfg := resourceConfig()
	cfg.Resources.TaskMax = 8
	cfg.Resources.BrowserMax = 8
	cfg.Resources.HighWindows = 2
	c := controller(t, cfg, clock)
	var held []func()
	for range 8 {
		r, e := c.Acquire(context.Background(), "a", ClassTask)
		if e != nil {
			t.Fatal(e)
		}
		held = append(held, r)
	}
	sample := func(cpu, mem float64, valid bool) {
		clock.advance(time.Second)
		c.Sample(Metrics{CPUPercent: cpu, MemoryPercent: mem, Valid: valid, AvailableMemoryBytes: 1 << 30, Source: "fixture", SampledAt: clock.Now()})
	}
	sample(95, 40, true)
	if c.Snapshot().Task.Limit != 8 {
		t.Fatal("shrunk before sustained CPU window")
	}
	sample(95, 40, true)
	if snap := c.Snapshot(); snap.Task.Limit != 4 || snap.Task.Inflight != 8 || snap.Browser.Limit != 4 {
		t.Fatalf("shrink changed in-flight work: %+v", snap)
	}
	for _, release := range held {
		release()
	}
	sample(20, 95, true)
	if c.Snapshot().Task.Limit != 4 || c.Snapshot().Browser.Limit != 2 {
		t.Fatal("memory pressure should prioritize browser admission")
	}
	clock.advance(time.Minute)
	sample(0, 0, false)
	if c.Snapshot().Task.Limit != 4 || c.Snapshot().SampleValid {
		t.Fatal("invalid sample permitted recovery")
	}
	sample(75, 80, true)
	if c.Snapshot().Task.Limit != 4 {
		t.Fatal("hysteresis band permitted recovery")
	}
	sample(20, 40, true)
	if snap := c.Snapshot(); snap.Task.Limit != 5 || snap.Browser.Limit != 3 {
		t.Fatalf("additive recovery incorrect: %+v", snap)
	}
	clock.advance(10 * time.Second)
	if c.Snapshot().SampleValid {
		t.Fatal("stale sample represented as healthy")
	}
	c.Sample(Metrics{CPUPercent: math.NaN(), MemoryPercent: math.Inf(1), Valid: true})
	if _, err := json.Marshal(c.Snapshot()); err != nil {
		t.Fatalf("invalid metrics broke snapshot JSON: %v", err)
	}
}

type permitResult struct {
	site    string
	release func()
	err     error
}

func queuePermit(c *Controller, ctx context.Context, site string, out chan<- permitResult) {
	go func() { r, e := c.Acquire(ctx, site, ClassTask); out <- permitResult{site, r, e} }()
}
func takePermit(t *testing.T, ch <-chan permitResult) permitResult {
	t.Helper()
	select {
	case got := <-ch:
		if got.err != nil {
			t.Fatal(got.err)
		}
		return got
	case <-time.After(3 * time.Second):
		t.Fatal("resource never admitted")
		return permitResult{}
	}
}

func TestOnePermitWeightedFairnessAndCancellation(t *testing.T) {
	cfg := resourceConfig()
	cfg.Sites = map[string]PolicyPatch{"a": {Weight: ptr(3)}, "b": {Weight: ptr(1)}}
	c := controller(t, cfg, newClock())
	held, _ := c.Acquire(context.Background(), "held", ClassTask)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan permitResult, 40)
	for range 12 {
		queuePermit(c, ctx, "a", out)
		queuePermit(c, ctx, "b", out)
	}
	eventually(t, func() bool { return c.Snapshot().Task.Waiting == 24 })
	held()
	counts := map[string]int{}
	for range 8 {
		got := takePermit(t, out)
		counts[got.site]++
		got.release()
	}
	if counts["a"] != 6 || counts["b"] != 2 {
		t.Fatalf("weighted service = %v", counts)
	}
	cancel()
	for range 16 {
		got := <-out
		if got.release != nil {
			got.release()
		}
	}
	eventually(t, func() bool { return c.Snapshot().Task.Inflight == 0 && c.Snapshot().Task.Waiting == 0 })
}

func TestStageBlockedWorkDoesNotReserveGlobalOrBrowserCapacity(t *testing.T) {
	cfg := resourceConfig()
	cfg.Resources.TaskMax = 2
	cfg.Defaults.Stages = map[string]StagePolicy{"one": {MaxConcurrent: 1}}
	c := controller(t, cfg, newClock())
	held, err := c.AcquireStage(context.Background(), "a", "one", 8)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	blocked := make(chan permitResult, 1)
	go func() { r, e := c.AcquireStage(ctx, "a", "one", 8); blocked <- permitResult{"a", r, e} }()
	eventually(t, func() bool { return c.Snapshot().Task.Waiting == 1 })
	other, err := c.AcquireStage(ctx, "a", "two", 8)
	if err != nil {
		t.Fatal(err)
	}
	browser, err := c.Acquire(ctx, "a", ClassBrowser)
	if err != nil {
		t.Fatal(err)
	}
	if snap := c.Snapshot(); snap.Task.Inflight != 2 || snap.Browser.Inflight != 1 || snap.Task.Waiting != 1 {
		t.Fatalf("admission classes coupled: %+v", snap)
	}
	other()
	browser()
	held()
	takePermit(t, blocked).release()
}

func TestConcurrentAcquireObserveUpdateAndSnapshot(t *testing.T) {
	cfg := requestConfig()
	cfg.Defaults.Request.MaxInflight = 4
	c := controller(t, cfg, newClock())
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			site := string(rune('a' + worker%2))
			for range 80 {
				r, e := c.AcquireRequest(context.Background(), site)
				if e != nil {
					t.Error(e)
					return
				}
				c.Snapshot()
				r(Feedback{StatusCode: 200, Latency: time.Millisecond})
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 80 {
			if err := c.Update(cfg); err != nil {
				t.Error(err)
			}
		}
	}()
	wg.Wait()
	for _, s := range c.Snapshot().Sites {
		if s.Inflight != 0 || s.Waiting != 0 {
			t.Fatalf("leaked state %+v", s)
		}
	}
}

func TestWaitTaskReadyParksRemotePauseWithoutShorteningDeadline(t *testing.T) {
	clock := newClock()
	cfg := requestConfig()
	cfg.Defaults.Request.RetryAfterMax = time.Minute
	c := controller(t, cfg, clock)
	c.Observe("a", Feedback{StatusCode: 429, RetryAfter: "120"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.WaitTaskReady(ctx, "a") }()
	eventually(t, func() bool { return c.Snapshot().Sites[0].ReadinessWaiting == 1 })
	if c.Snapshot().Sites[0].TaskWaiting != 0 || c.Snapshot().Task.Inflight != 0 {
		t.Fatal("readiness wait consumed or masqueraded as resource admission")
	}
	select {
	case err := <-done:
		t.Fatalf("paused work should stay pending: %v", err)
	default:
	}
	clock.advance(time.Minute)
	c.AcknowledgeRemote("a")
	select {
	case err := <-done:
		t.Fatalf("acknowledgment shortened remote wait: %v", err)
	default:
	}
	clock.advance(time.Minute)
	c.Observe("a", Feedback{})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("elapsed deadline did not release work")
	}
	c.Observe("a", Feedback{StatusCode: 429, RetryAfter: "120"})
	go func() { done <- c.WaitTaskReady(ctx, "a") }()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("pause cancellation=%v", err)
	}
	if c.Snapshot().Sites[0].ReadinessWaiting != 0 {
		t.Fatal("cancelled readiness waiter leaked")
	}
}
