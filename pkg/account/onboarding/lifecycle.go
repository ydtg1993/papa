package onboarding

import "context"

// beginOperation reserves shutdown ownership before touching the transaction
// mutex or invoking external stores/providers. The separate lifecycle mutex
// protects only admission and WG.Add; a blocked transaction cannot hold it.
func (s *Service) beginOperation(parent context.Context) (context.Context, func(), error) {
	s.lifecycleMu.Lock()
	if s.closed.Load() {
		s.lifecycleMu.Unlock()
		return parent, nil, ErrGone
	}
	s.wg.Add(1)
	s.lifecycleMu.Unlock()
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(s.ctx, cancel)
	return ctx, func() {
		stop()
		cancel()
		s.wg.Done()
	}, nil
}

func (s *Service) Close() { _ = s.Shutdown(context.Background()) }

// BeginShutdown closes admission and cancels operations without waiting for
// providers or storage. It does not acquire the profile transaction mutex.
func (s *Service) BeginShutdown() {
	s.lifecycleMu.Lock()
	if !s.closed.Load() {
		s.closed.Store(true)
		s.cancel()
		go func() { s.wg.Wait(); close(s.drained) }()
	}
	s.lifecycleMu.Unlock()
}

// Shutdown waits for admitted calls, workflows and cleanup within the caller's
// budget. False completion is never inferred from cancellation alone; timeout
// leaves resource ownership pending and a later call can finish the drain.
func (s *Service) Shutdown(ctx context.Context) error {
	s.BeginShutdown()
	select {
	case <-s.drained:
		return nil
	default:
	}
	select {
	case <-s.drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
