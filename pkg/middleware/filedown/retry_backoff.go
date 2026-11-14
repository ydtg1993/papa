package filedown

import (
	"time"

	"github.com/ydtg1993/papa/v4/internal/backoff"
)

// chunkBackoff retains the first retry's 2*base exponent and the 10s cap.
// Equal Jitter keeps at least half of that capped delay.
func chunkBackoff(attempt, base int) time.Duration {
	if base <= 0 {
		base = 1
	}
	return backoff.Delay(backoff.Seconds(base), attempt, 10*time.Second)
}
