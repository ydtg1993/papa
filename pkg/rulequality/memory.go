package rulequality

import (
	"context"
	"sort"
	"sync"
	"time"
)

// MemoryStore implements the same immutable/CAS contract for deterministic tests.
// It is process-local; use MySQLStore for durable request reservations.
type MemoryStore struct {
	mu           sync.Mutex
	sequence     uint64
	observations map[string]Observation
	reports      map[string]Report
	requests     map[string]Request
	operations   map[string]string
	runs         map[string]string
	now          func() time.Time
}

var _ Store = (*MemoryStore)(nil)

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{observations: map[string]Observation{}, reports: map[string]Report{}, requests: map[string]Request{}, operations: map[string]string{}, runs: map[string]string{}, now: func() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }}
}

func sameObservation(in, stored Observation) bool {
	if in.Sequence == 0 {
		in.Sequence = stored.Sequence
	}
	if in.CreatedAt.IsZero() {
		in.CreatedAt = stored.CreatedAt
	}
	a, _, e := encodePayload(in)
	b, _, f := encodePayload(stored)
	return e == nil && f == nil && a == b
}
func sameReport(in, stored Report) bool {
	if in.CreatedAt.IsZero() {
		in.CreatedAt = stored.CreatedAt
	}
	a, _, e := encodePayload(in)
	b, _, f := encodePayload(stored)
	return e == nil && f == nil && a == b
}

// Record permits store-assigned metadata only when retrying the exact stored value.
// New observations must leave Sequence and CreatedAt zero.
func (s *MemoryStore) Record(ctx context.Context, o Observation) (Observation, error) {
	if err := checkedKey(ctx, o.ID); err != nil {
		return Observation{}, err
	}
	if err := validateObservation(o); err != nil {
		return Observation{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Observation{}, err
	}
	if old, ok := s.observations[o.ID]; ok {
		if !sameObservation(o, old) {
			return Observation{}, ErrConflict
		}
		return detached(old), nil
	}
	if o.Sequence != 0 || !o.CreatedAt.IsZero() {
		return Observation{}, ErrInvalid
	}
	o.Sequence = s.sequence + 1
	o.CreatedAt = s.now()
	if _, _, err := encodePayload(o); err != nil {
		return Observation{}, err
	}
	s.sequence = o.Sequence
	s.observations[o.ID] = detached(o)
	return detached(o), nil
}
func (s *MemoryStore) Query(ctx context.Context, q Query) (Window, error) {
	if ctx == nil {
		return Window{}, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return Window{}, err
	}
	q, err := normalizeQuery(q)
	if err != nil {
		return Window{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Window{}, err
	}
	w := Window{Query: q, Observations: []Observation{}}
	for _, o := range s.observations {
		if o.Scope == q.Scope && !o.CreatedAt.Before(q.From) && o.CreatedAt.Before(q.Until) {
			w.Observations = append(w.Observations, detached(o))
			if o.Sequence > w.Cutoff {
				w.Cutoff = o.Sequence
			}
		}
	}
	sort.Slice(w.Observations, func(i, j int) bool { return w.Observations[i].Sequence < w.Observations[j].Sequence })
	if len(w.Observations) > q.Limit {
		w.Truncated = true
		w.Observations = w.Observations[:q.Limit]
	}
	return w, nil
}
func prepareReport(r Report) (Report, error) {
	q, err := normalizeQuery(r.Query)
	if err != nil {
		return Report{}, err
	}
	r.Query = q
	if err := validateReport(r); err != nil {
		return Report{}, err
	}
	if _, _, err := encodePayload(r); err != nil {
		return Report{}, err
	}
	return r, nil
}
func (s *MemoryStore) SaveReport(ctx context.Context, r Report) (Report, error) {
	if err := checkedKey(ctx, r.ID); err != nil {
		return Report{}, err
	}
	r, err := prepareReport(r)
	if err != nil {
		return Report{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	if old, ok := s.reports[r.ID]; ok {
		if !sameReport(r, old) {
			return Report{}, ErrConflict
		}
		return detached(old), nil
	}
	if !r.CreatedAt.IsZero() {
		return Report{}, ErrInvalid
	}
	r.CreatedAt = s.now()
	if _, _, err := encodePayload(r); err != nil {
		return Report{}, err
	}
	s.reports[r.ID] = detached(r)
	return detached(r), nil
}
func (s *MemoryStore) Report(ctx context.Context, id string) (Report, error) {
	if err := checkedKey(ctx, id); err != nil {
		return Report{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	r, ok := s.reports[id]
	if !ok {
		return Report{}, ErrNotFound
	}
	return detached(r), nil
}
func (s *MemoryStore) CreateRequest(ctx context.Context, r Request) (Request, error) {
	if err := checkedKey(ctx, r.ID); err != nil {
		return Request{}, err
	}
	r, err := prepareRequest(r)
	if err != nil {
		return Request{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Request{}, err
	}
	if id, ok := s.operations[r.OperationID]; ok {
		old := s.requests[id]
		if !requestEquivalent(r, old) {
			return Request{}, ErrConflict
		}
		return old, nil
	}
	if _, ok := s.requests[r.ID]; ok {
		return Request{}, ErrConflict
	}
	if _, ok := s.runs[r.RunID]; ok {
		return Request{}, ErrConflict
	}
	if _, ok := s.reports[r.ReportID]; !ok {
		return Request{}, ErrNotFound
	}
	r.CreatedAt = s.now()
	r.UpdatedAt = r.CreatedAt
	s.requests[r.ID] = r
	s.operations[r.OperationID] = r.ID
	s.runs[r.RunID] = r.ID
	return r, nil
}
func (s *MemoryStore) Request(ctx context.Context, id string) (Request, error) {
	if err := checkedKey(ctx, id); err != nil {
		return Request{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Request{}, err
	}
	r, ok := s.requests[id]
	if !ok {
		return Request{}, ErrNotFound
	}
	return r, nil
}
func (s *MemoryStore) RequestByOperation(ctx context.Context, id string) (Request, error) {
	if err := checkedKey(ctx, id); err != nil {
		return Request{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Request{}, err
	}
	key, ok := s.operations[id]
	if !ok {
		return Request{}, ErrNotFound
	}
	return s.requests[key], nil
}
func (s *MemoryStore) mutateRequest(ctx context.Context, id string, rev uint64, start bool, status, input string) (Request, error) {
	if err := checkedKey(ctx, id); err != nil {
		return Request{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Request{}, err
	}
	r, ok := s.requests[id]
	if !ok {
		return Request{}, ErrNotFound
	}
	next, err := changeRequest(r, rev, start, status, input, s.now())
	if err != nil {
		return Request{}, err
	}
	s.requests[id] = next
	return next, nil
}
func (s *MemoryStore) StartRequest(ctx context.Context, id string, rev uint64, inputSHA string) (Request, error) {
	return s.mutateRequest(ctx, id, rev, true, "", inputSHA)
}

// FinishRequest(Interrupted) is an explicit host assertion that the previous
// worker has stopped. It never takes over, resumes, or invokes an Agent.
func (s *MemoryStore) FinishRequest(ctx context.Context, id string, rev uint64, status, result string) (Request, error) {
	return s.mutateRequest(ctx, id, rev, false, status, result)
}
func (s *MemoryStore) PruneObservations(ctx context.Context, before time.Time, limit int) (int64, error) {
	if err := checkedPrune(ctx, before, limit); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var rows []Observation
	for _, o := range s.observations {
		if o.CreatedAt.Before(before) {
			rows = append(rows, o)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].Sequence < rows[j].Sequence
		}
		return rows[i].CreatedAt.Before(rows[j].CreatedAt)
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	for _, o := range rows {
		delete(s.observations, o.ID)
	}
	return int64(len(rows)), nil
}
