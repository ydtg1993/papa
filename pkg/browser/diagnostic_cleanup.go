package browser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// removeDiagnosticProfile uses the process shutdown deadline, not a fresh
// timeout. Windows process termination and final cache handle release are not
// one event: retry only the OS errors which can result from that release window.
// The caller supplies the exact temporary profile it created and owns.
func removeDiagnosticProfile(ctx context.Context, dir string) error {
	var last error
	delay := 10 * time.Millisecond
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("diagnostic browser profile cleanup: %w", errors.Join(err, last))
		}
		last = os.RemoveAll(dir)
		if last == nil || !retryDiagnosticCleanup(last) {
			return last
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("diagnostic browser profile cleanup: %w", errors.Join(ctx.Err(), last))
		case <-timer.C:
		}
		delay = min(2*delay, 100*time.Millisecond)
	}
}
