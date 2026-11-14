// Package backoff computes bounded retry delays. Callers own retry counts,
// cancellation, waiting and settlement; this package has no lifecycle state.
package backoff

import (
	"math"
	"math/rand/v2"
	"time"
)

// Seconds converts a positive integer number of seconds without overflowing.
// Nonpositive inputs return zero; policy-specific defaults belong to callers.
func Seconds(seconds int) time.Duration {
	if seconds <= 0 {
		return 0
	}
	if uint64(seconds) > uint64(math.MaxInt64/int64(time.Second)) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(seconds) * time.Second
}

// Delay returns Equal Jitter in [ceil(E/2), E], where E is the capped
// exponential base*2^exponent. Negative exponents mean zero doublings;
// nonpositive base or limit disables waiting. The top-level random source is
// concurrency safe. No random state is shared through caller-owned objects.
func Delay(base time.Duration, exponent int, limit time.Duration) time.Duration {
	return equalJitter(exponential(base, exponent, limit), rand.Int64N)
}

func exponential(base time.Duration, exponent int, limit time.Duration) time.Duration {
	if base <= 0 || limit <= 0 {
		return 0
	}
	exponent = max(0, exponent)
	// Compare before shifting: capping an already overflowed duration is too late.
	if exponent >= 63 || base > limit>>uint(exponent) {
		return limit
	}
	return base << uint(exponent)
}

func equalJitter(delay time.Duration, sample func(int64) int64) time.Duration {
	if delay <= 0 {
		return 0
	}
	lower := delay - delay/2
	// The width is at most MaxInt64/2+1, even for the maximum Duration.
	return lower + time.Duration(sample(int64(delay/2)+1))
}
