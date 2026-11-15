package browser

import "context"

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
	stop := context.AfterFunc(ctx, p.broadcast)
	defer stop()
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.instances) > 0 || p.proxyCreating > 0 || p.directCreating > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		p.cond.Wait()
	}
	return nil
}
