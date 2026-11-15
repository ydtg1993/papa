package browser

import (
	"context"
	"errors"
)

// Shutdown is the terminal process-exit operation. Unlike Close it interrupts
// borrowed/reserved browsers too, cancels startup, and waits for owned instances
// and in-flight creation to settle. A deadline error means cleanup is unfinished;
// late factories still close their results instead of handing them out.
func (p *Pool) Shutdown(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.stopAdmission()
	p.mu.Lock()
	instances := make([]*Browser, 0, len(p.instances))
	for b := range p.instances {
		instances = append(instances, b)
	}
	p.mu.Unlock()
	for _, b := range instances {
		go b.Close()
	}
	return p.waitCleanup(ctx)
}

func (p *Pool) waitCleanup(ctx context.Context) error {
	stop := context.AfterFunc(ctx, p.broadcast)
	defer stop()
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.instances) > 0 || p.proxyCreating > 0 || p.directCreating > 0 {
		if err := ctx.Err(); err != nil {
			return errors.Join(err, p.cleanupErr)
		}
		p.cond.Wait()
	}
	return p.cleanupErr
}

// Retain only the first cleanup error so repeated retirements cannot grow an
// unbounded error history. Zero live processes does not imply cleaned profiles.
func (p *Pool) recordCleanupError(err error) {
	if err == nil {
		return
	}
	p.mu.Lock()
	if p.cleanupErr == nil {
		p.cleanupErr = err
	}
	p.mu.Unlock()
}
