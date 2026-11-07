package agentadmin

import (
	"context"
	"strings"

	"github.com/ydtg1993/papa/v3/pkg/rulequality"
)

// Dismiss withdraws an unstarted request by CAS. A concurrent Start can win,
// in which case dismissal fails and the caller must query the same request.
func (s *Service) Dismiss(ctx context.Context, in ControlInput, operator string) (RequestView, error) {
	if !in.Confirmed || s.cfg.Quality == nil {
		return RequestView{}, ErrInvalid
	}
	rev, err := revision(in.Revision)
	if err != nil {
		return RequestView{}, err
	}
	r, err := s.cfg.Quality.Request(ctx, in.ID)
	if err != nil {
		return RequestView{}, err
	}
	if r.Operator != operator || !strings.HasPrefix(r.Source, "oa-agent:") {
		return RequestView{}, ErrOwner
	}
	r, err = s.cfg.Quality.FinishRequest(ctx, r.ID, rev, rulequality.Dismissed, `{"reason":"operator_withdrew_pending_request"}`)
	if err != nil {
		return RequestView{}, err
	}
	return s.requestView(r), nil
}

func (s *Service) Start(ctx context.Context, in StartInput, operator string) (RequestView, error) {
	rev, err := revision(in.Revision)
	if err != nil {
		return RequestView{}, err
	}
	p, b, input, err := s.build(ctx, in.Selection, operator)
	if err != nil {
		return RequestView{}, err
	}
	if p.Digest != in.Digest {
		return RequestView{}, ErrReviewChanged
	}
	r, err := s.cfg.Quality.Request(ctx, in.ID)
	if err != nil {
		return RequestView{}, err
	}
	if r.Operator != operator || r.Source != source(s.profiles[in.ProfileID]) || r.ReportID != in.ReportID {
		return RequestView{}, ErrOwner
	}
	if r.Revision != rev || r.Status != rulequality.Pending {
		return RequestView{}, rulequality.ErrConflict
	}
	s.mu.Lock()
	_, recovering := s.recoveries[r.ID]
	if _, exists := s.jobs[r.ID]; exists || recovering || s.closing || len(s.jobs) >= s.cfg.MaxConcurrent {
		s.mu.Unlock()
		return RequestView{}, ErrBusy
	}
	// The reviewed duration covers snapshot import as well as Agent.Run.
	worker, cancel := context.WithTimeout(s.ctx, p.Plan.Budgets.MaxDuration)
	s.jobs[r.ID] = job{cancel, operator}
	delete(s.recent, r.ID)
	s.mu.Unlock()
	go func() {
		failure := ""
		defer func() {
			if recover() != nil {
				failure = "execution_unconfirmed"
			}
			cancel()
			s.mu.Lock()
			delete(s.jobs, r.ID)
			// Only stable local hints; authoritative status remains in the Store.
			if failure != "" {
				s.recent[r.ID] = failure
				s.order = append(s.order, r.ID)
				if len(s.order) > 64 {
					delete(s.recent, s.order[0])
					s.order = s.order[1:]
				}
			}
			if s.closing && len(s.jobs) == 0 && len(s.recoveries) == 0 {
				close(s.done)
			}
			s.mu.Unlock()
		}()
		_, runErr := b.Execute(worker, r.ID, rev, input)
		if runErr != nil {
			failure = "inspect_durable_request"
		}
	}()
	// Accepted locally, not a claim that StartRequest committed or a model ran.
	return s.requestView(r), nil
}
func (s *Service) Cancel(ctx context.Context, in ControlInput, operator string) (RequestView, error) {
	if !in.Confirmed || !textValue(operator, 128) || s.cfg.Quality == nil {
		return RequestView{}, ErrInvalid
	}
	rev, err := revision(in.Revision)
	if err != nil {
		return RequestView{}, err
	}
	r, err := s.cfg.Quality.Request(ctx, in.ID)
	if err != nil {
		return RequestView{}, err
	}
	if r.Operator != operator {
		return RequestView{}, ErrOwner
	}
	if r.Revision != rev {
		return RequestView{}, rulequality.ErrConflict
	}
	s.mu.Lock()
	j, exists := s.jobs[r.ID]
	if !exists {
		s.mu.Unlock()
		return RequestView{}, ErrStopped
	}
	j.cancel()
	s.mu.Unlock()
	return s.requestView(r), nil
}
func (s *Service) Reconcile(ctx context.Context, in ControlInput, operator string) (view RequestView, err error) {
	if ctx == nil || !in.Confirmed || s.cfg.WorkerStopped == nil || s.cfg.Quality == nil {
		return RequestView{}, ErrStopped
	}
	rev, err := revision(in.Revision)
	if err != nil {
		return RequestView{}, err
	}
	r, err := s.cfg.Quality.Request(ctx, in.ID)
	if err != nil {
		return RequestView{}, err
	}
	if r.Operator != operator || !strings.HasPrefix(r.Source, "oa-agent:") {
		return RequestView{}, ErrOwner
	}
	if r.Revision != rev || r.Status != rulequality.Running {
		return RequestView{}, rulequality.ErrConflict
	}
	// Reserve only this request; host callbacks and I/O must not hold the
	// service mutex. Close cancels and drains reservations, even after timeout.
	s.mu.Lock()
	_, recovering := s.recoveries[r.ID]
	if _, active := s.jobs[r.ID]; active || recovering || s.closing || len(s.recoveries) >= s.cfg.MaxConcurrent {
		s.mu.Unlock()
		return RequestView{}, ErrBusy
	}
	work, cancel := context.WithCancel(ctx)
	s.recoveries[r.ID] = cancel
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		delete(s.recoveries, r.ID)
		if s.closing && len(s.jobs) == 0 && len(s.recoveries) == 0 {
			close(s.done)
		}
		s.mu.Unlock()
		if recover() != nil {
			view, err = RequestView{}, ErrStopped
		}
	}()
	if !s.cfg.WorkerStopped(work, r) {
		return RequestView{}, ErrStopped
	}
	if err = work.Err(); err != nil {
		return RequestView{}, err
	}
	out, err := rulequality.ReconcileStopped(work, s.cfg.Quality, s.cfg.Audit, r.ID, rev, operator, r.Source)
	if err != nil {
		return RequestView{}, err
	}
	return s.requestView(out.Request), nil
}
