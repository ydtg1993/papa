package governance

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// These experiments call the real controller. Candidate smoothing exists only
// in this file; it is not a second production controller or a new configuration.
func TestAlgorithmCPUReplay(t *testing.T) {
	for _, scenario := range []string{"sustained", "isolated_spikes", "threshold_noise"} {
		for _, alpha := range []float64{0, .2, .5} {
			t.Run(fmt.Sprintf("%s/alpha_%g", scenario, alpha), func(t *testing.T) {
				cfg := DefaultConfig()
				cfg.Resources.Enabled = true
				clock := newClock()
				c := controller(t, cfg, clock)
				filtered := 40.0
				first, recovered, changes, previous := -1, -1, 0, 32
				for i := 0; i < 600; i++ {
					raw := 40.0
					if i >= 10 && i < 30 {
						switch scenario {
						case "sustained":
							raw = 95
						case "isolated_spikes":
							if i%4 == 0 {
								raw = 95
							}
						case "threshold_noise":
							raw = 84
							if i%2 == 0 {
								raw = 86
							}
						}
					}
					value := raw
					if alpha != 0 {
						filtered += alpha * (raw - filtered)
						value = filtered
					}
					clock.advance(2 * time.Second)
					c.Sample(Metrics{CPUPercent: value, MemoryPercent: 40, AvailableMemoryBytes: 1 << 30, Valid: true, Source: "synthetic", SampledAt: clock.Now()})
					limit := c.Snapshot().Task.Limit
					if limit < 1 || limit > 32 {
						t.Fatalf("invalid limit %d", limit)
					}
					if limit != previous {
						changes++
						previous = limit
					}
					if limit < 32 && first == -1 {
						first = (i - 10 + 1) * 2
					}
					if i >= 30 && first != -1 && limit == 32 && recovered == -1 {
						recovered = (i - 30 + 1) * 2
					}
				}
				if scenario == "sustained" && (first < 6 || recovered < 0) {
					t.Fatalf("pressure/recovery not exercised: %d %d", first, recovered)
				}
				if scenario == "isolated_spikes" && changes != 0 {
					t.Fatalf("isolated spike changed capacity: %d", changes)
				}
				t.Logf("first_shrink_s=%d full_recovery_after_pressure_s=%d limit_changes=%d", first, recovered, changes)
			})
		}
	}
}

// A closed-loop, discrete fluid model, not OS CPU telemetry: arrivals=28 tasks
// per 2s, each permit completes one task/tick; CPU=min(100, busy*4). Production
// Sample controls the next tick's capacity. Backlog is an integer task count.
func TestAlgorithmCPUClosedLoop(t *testing.T) {
	for _, alpha := range []float64{0, .2, .5} {
		t.Run(fmt.Sprintf("alpha_%g", alpha), func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Resources.Enabled = true
			clock := newClock()
			c := controller(t, cfg, clock)
			filtered := 40.0
			backlog, peak, done, changes, previous, high := 0, 0, 0, 0, 32, 0
			for i := 0; i < 900; i++ {
				arrivals := 28
				if i < 10 {
					arrivals = 10
				}
				if i >= 150 {
					arrivals = 0
				}
				backlog += arrivals
				busy := min(backlog, c.Snapshot().Task.Limit)
				backlog -= busy
				done += busy
				peak = max(peak, backlog)
				raw := min(100.0, float64(busy)*4)
				if raw >= 85 {
					high++
				}
				value := raw
				if alpha != 0 {
					filtered += alpha * (raw - filtered)
					value = filtered
				}
				clock.advance(2 * time.Second)
				c.Sample(Metrics{CPUPercent: value, MemoryPercent: 40, AvailableMemoryBytes: 1 << 30, Valid: true, SampledAt: clock.Now()})
				limit := c.Snapshot().Task.Limit
				if limit != previous {
					changes++
					previous = limit
				}
			}
			if done != 4020 || backlog != 0 {
				t.Fatalf("task conservation: completed=%d backlog=%d", done, backlog)
			}
			t.Logf("completed=%d peak_backlog=%d high_cpu_ticks=%d changes=%d", done, peak, high, changes)
		})
	}
}

func TestAlgorithmThrottleReplay(t *testing.T) {
	for _, scenario := range []string{"fast_429", "slow_503", "alternating_success", "retry_after", "over_budget"} {
		for _, adaptive := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/adaptive_%t", scenario, adaptive), func(t *testing.T) {
				cfg := requestConfig()
				cfg.Defaults.Request.AutoThrottle.Enabled = true
				clock := newClock()
				c := controller(t, cfg, clock)
				changes := 0
				previous := 100 * time.Millisecond
				recovery := -1
				var pressureDelay time.Duration
				for i := 0; i < 40; i++ {
					f := Feedback{StatusCode: 200, Latency: 100 * time.Millisecond}
					if i < 5 {
						switch scenario {
						case "fast_429":
							f = Feedback{StatusCode: 429, Latency: time.Millisecond}
						case "slow_503":
							f = Feedback{StatusCode: 503, Latency: 8 * time.Second}
						case "alternating_success":
							if i%2 == 0 {
								f.Latency = 8 * time.Second
							}
						case "retry_after":
							f = Feedback{StatusCode: 503, Latency: time.Millisecond, RetryAfter: "7"}
						case "over_budget":
							f = Feedback{StatusCode: 429, RetryAfter: "7200"}
						}
					}
					// Trial: directional alpha, speed .8 for increasing delay, .2
					// for recovery. No error-window or policy update side effects.
					if adaptive {
						c.mu.Lock()
						a := &c.defaults.Request.AutoThrottle
						target := clampDuration(secondsDuration(f.Latency.Seconds()/a.TargetConcurrency), a.MinDelay, a.MaxDelay)
						a.Smoothing = .2
						if target > c.stateLocked("fixture").autoDelay {
							a.Smoothing = .8
						}
						c.mu.Unlock()
					}
					clock.advance(time.Second)
					c.Observe("fixture", f)
					s := c.Snapshot().Sites[0]
					d := s.EffectiveDelay
					if d < 100*time.Millisecond || d > 30*time.Second {
						t.Fatal("unbounded delay", d)
					}
					if d != previous {
						changes++
					}
					previous = d
					if i == 4 {
						pressureDelay = d
					}
					if i >= 5 && d <= 110*time.Millisecond && recovery < 0 {
						recovery = i - 4
					}
					if (scenario == "fast_429" || scenario == "retry_after" || scenario == "over_budget") && i < 5 && !s.CooldownUntil.After(clock.Now()) {
						t.Fatal("cooldown missing")
					}
					if scenario == "over_budget" && !s.RemotePaused {
						t.Fatal("remote pause lost")
					}
				}
				s := c.Snapshot().Sites[0]
				if s.Errors != map[bool]uint64{true: 0, false: 5}[scenario == "alternating_success"] {
					t.Fatal("feedback count", s.Errors)
				}
				if recovery < 0 {
					t.Fatal("recovery not reached")
				}
				t.Logf("delay_after_5=%s recovery_successes=%d adjustments=%d", pressureDelay, recovery, changes)
			})
		}
	}
}

func BenchmarkAlgorithmGovernance(b *testing.B) {
	b.Run("resource_sample", func(b *testing.B) {
		cfg := DefaultConfig()
		cfg.Resources.Enabled = true
		clock := newClock()
		c, err := New(cfg, WithClock(clock))
		if err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			clock.advance(2 * time.Second)
			c.Sample(Metrics{CPUPercent: 70, MemoryPercent: 40, AvailableMemoryBytes: 1 << 30, Valid: true, SampledAt: clock.Now()})
		}
	})
	for _, sites := range []int{1, 16, 128} {
		for _, mode := range []string{"feedback", "request_permit"} {
			b.Run(fmt.Sprintf("%s/sites_%d", mode, sites), func(b *testing.B) {
				cfg := requestConfig()
				cfg.Defaults.Request.AutoThrottle.Enabled = mode == "feedback"
				c, err := New(cfg)
				if err != nil {
					b.Fatal(err)
				}
				names := make([]string, sites)
				for i := range names {
					names[i] = fmt.Sprint("synthetic-", i)
					c.Observe(names[i], Feedback{StatusCode: 200, Latency: time.Millisecond})
				}
				var sequence atomic.Uint64
				b.ReportAllocs()
				b.ResetTimer()
				b.RunParallel(func(pb *testing.PB) {
					site := names[int(sequence.Add(1))%sites]
					for pb.Next() {
						f := Feedback{StatusCode: 200, Latency: time.Millisecond}
						if mode == "feedback" {
							c.Observe(site, f)
						} else {
							release, err := c.AcquireRequest(context.Background(), site)
							if err != nil {
								b.Error(err)
								return
							}
							release(f)
						}
					}
				})
			})
		}
	}
}
