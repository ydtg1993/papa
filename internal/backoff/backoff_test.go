package backoff

import (
	"math"
	"math/big"
	"math/rand/v2"
	"sync"
	"testing"
	"time"
)

func TestExponentialSaturatesBeforeArithmetic(t *testing.T) {
	for _, tc := range []struct {
		base        time.Duration
		exponent    int
		limit, want time.Duration
	}{
		{time.Second, 0, time.Minute, time.Second},
		{time.Second, 2, time.Minute, 4 * time.Second},
		{time.Second, 6, time.Minute, time.Minute},
		{time.Second, math.MaxInt, time.Minute, time.Minute},
		{math.MaxInt64, 2, time.Minute, time.Minute},
		{1, 62, math.MaxInt64, 1 << 62},
		{1, 63, math.MaxInt64, math.MaxInt64},
		{1 << 62, 1, math.MaxInt64, math.MaxInt64},
		{math.MaxInt64, 0, math.MaxInt64, math.MaxInt64},
		{0, math.MaxInt, time.Minute, 0},
		{-1, 3, time.Minute, 0},
		{time.Second, -1, time.Minute, time.Second},
		{time.Second, 3, 0, 0},
		{time.Second, 3, -1, 0},
	} {
		if got := exponential(tc.base, tc.exponent, tc.limit); got != tc.want {
			t.Fatalf("exponential(%v, %d, %v)=%v want %v", tc.base, tc.exponent, tc.limit, got, tc.want)
		}
	}
}

func TestSecondsSaturatesBeforeConversion(t *testing.T) {
	for _, tc := range []struct {
		seconds int
		want    time.Duration
	}{
		{-1, 0}, {0, 0}, {1, time.Second}, {10, 10 * time.Second},
	} {
		if got := Seconds(tc.seconds); got != tc.want {
			t.Fatalf("seconds %d: %v", tc.seconds, got)
		}
	}
	// Architecture-independent assertion: int may be narrower than Duration.
	if uint64(math.MaxInt) > uint64(math.MaxInt64/int64(time.Second)) {
		if got := Seconds(math.MaxInt); got != math.MaxInt64 {
			t.Fatalf("overflow: %v", got)
		}
	}
}

func TestEqualJitterExactBoundsAndSpread(t *testing.T) {
	for _, d := range []time.Duration{1, 2, 3, time.Second, math.MaxInt64} {
		if got := equalJitter(d, func(int64) int64 { return 0 }); got != d-d/2 {
			t.Fatalf("lower(%v)=%v", d, got)
		}
		if got := equalJitter(d, func(n int64) int64 { return n - 1 }); got != d {
			t.Fatalf("upper(%v)=%v", d, got)
		}
	}
	for _, d := range []time.Duration{0, -1} {
		if got := equalJitter(d, func(int64) int64 { t.Fatal("sampled disabled delay"); return 0 }); got != 0 {
			t.Fatal(got)
		}
	}
	rng := rand.New(rand.NewPCG(17, 29))
	seen := map[time.Duration]bool{}
	for range 128 {
		d := equalJitter(time.Second, rng.Int64N)
		if d < 500*time.Millisecond || d > time.Second {
			t.Fatal(d)
		}
		seen[d] = true
	}
	if len(seen) < 100 {
		t.Fatalf("fixed-seed retries not dispersed: %d values", len(seen))
	}
}

func TestDelayConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 1000 {
				if d := Delay(math.MaxInt64, math.MaxInt, time.Minute); d < 30*time.Second || d > time.Minute {
					t.Errorf("delay %v", d)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestExponentialMatchesExactIntegerArithmetic(t *testing.T) {
	for _, base := range []time.Duration{1, 2, 3, 999999999, 1 << 32, math.MaxInt64 / 3, math.MaxInt64} {
		for _, limit := range []time.Duration{1, 3, time.Second, time.Minute, math.MaxInt64} {
			for exponent := 0; exponent <= 64; exponent++ {
				exact := new(big.Int).Lsh(big.NewInt(int64(base)), uint(exponent))
				want := limit
				if exact.Cmp(big.NewInt(int64(limit))) <= 0 {
					want = time.Duration(exact.Int64())
				}
				if got := exponential(base, exponent, limit); got != want {
					t.Fatalf("base=%d exp=%d limit=%d got=%d want=%d", base, exponent, limit, got, want)
				}
			}
		}
	}
}

func TestSecondsAtRepresentableBoundary(t *testing.T) {
	boundary := int64(math.MaxInt64) / int64(time.Second)
	if uint64(math.MaxInt) < uint64(boundary+1) {
		largest := math.MaxInt
		if got := Seconds(largest); got != time.Duration(largest)*time.Second {
			t.Fatal(got)
		}
		return
	}
	for _, seconds := range []int64{boundary - 1, boundary, boundary + 1} {
		exact := new(big.Int).Mul(big.NewInt(seconds), big.NewInt(int64(time.Second)))
		want := time.Duration(math.MaxInt64)
		if exact.IsInt64() {
			want = time.Duration(exact.Int64())
		}
		if got := Seconds(int(seconds)); got != want {
			t.Fatalf("seconds=%d got=%d want=%d", seconds, got, want)
		}
	}
}

func TestDelayUsesJitterAfterCapping(t *testing.T) {
	seen := map[time.Duration]bool{}
	for range 128 {
		got := Delay(math.MaxInt64, math.MaxInt, time.Minute)
		if got < 30*time.Second || got > time.Minute {
			t.Fatal(got)
		}
		seen[got] = true
	}
	// Range checks alone also accept a fixed cap or midpoint (no jitter).
	if len(seen) < 2 {
		t.Fatal("production Delay returned a constant instead of jitter")
	}
}
