package engine

import (
	"time"

	"github.com/ydtg1993/papa/v4/internal/backoff"
)

// retryBackoff owns the Handler policy. attempt=1 is the first retry;
// the execution loop owns waiting, retry persistence and cancellation.
func retryBackoff(base time.Duration, attempt int) time.Duration {
	return backoff.Delay(base, max(1, attempt)-1, time.Minute)
}
