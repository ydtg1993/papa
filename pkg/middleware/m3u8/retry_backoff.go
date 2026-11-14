package m3u8

import (
	"time"

	"github.com/ydtg1993/papa/v4/internal/backoff"
)

// Request retries start at base; the encrypted segment's outer retry starts
// at 2s. These are separate retry policies, both capped before Equal Jitter.
func requestBackoff(attempt, seconds int) time.Duration {
	if seconds <= 0 {
		seconds = 2
	}
	return backoff.Delay(backoff.Seconds(seconds), max(1, attempt)-1, 10*time.Second)
}

func segmentBackoff(attempt int) time.Duration {
	return backoff.Delay(time.Second, attempt, 10*time.Second)
}
