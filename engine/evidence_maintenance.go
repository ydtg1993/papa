package engine

import (
	"context"
	"sync"
	"time"
)

// startEvidenceMaintenance registers before launching so Stop drains in-flight
// cleanup. Cancellation and the wait group remain owned by Engine.
func startEvidenceMaintenance(ctx context.Context, wg *sync.WaitGroup, interval time.Duration, cleanup func(context.Context)) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cleanup(ctx)
			}
		}
	}()
}
