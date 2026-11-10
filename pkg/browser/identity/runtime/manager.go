package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/browser/identity"
)

type subjectState struct {
	binding   Binding
	refs      int
	verifying bool
	resolved  *identity.ResolvedProfile
	uncertain bool
}
type Manager struct {
	mu          sync.Mutex
	lifecycleMu sync.Mutex
	active      int
	closed      bool
	drained     chan struct{}
	store       Store
	options     Options
	subjects    map[Subject]*subjectState
	changed     chan struct{}
	ctx         context.Context
	cancel      context.CancelFunc
}

func NewManager(store Store, options Options) (*Manager, error) {
	if store == nil || options.Resolve == nil || options.Verify == nil {
		return nil, errors.New("identity store, resolver and verifier required")
	}
	if options.VerifyTimeout <= 0 {
		options.VerifyTimeout = 30 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{store: store, options: options, subjects: map[Subject]*subjectState{}, changed: make(chan struct{}), drained: make(chan struct{}), ctx: ctx, cancel: cancel}, nil
}
func (m *Manager) Store() Store { return m.store }

// Close rejects new operations and cancels existing ones. Shutdown additionally
// waits for their scopes, callbacks and persistence cleanup to finish.
func (m *Manager) Close() {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if !m.closed {
		m.closed = true
		m.cancel()
		if m.active == 0 {
			close(m.drained)
		}
	}
}

// Shutdown reports whether all owned use has drained before ctx expires. A
// false result means dependencies must stay open; callers may wait again later.
// Lifecycle accounting uses its own short-held mutex so an uncooperative store
// or callback holding the subject mutex cannot exceed this shutdown deadline.
func (m *Manager) Shutdown(ctx context.Context) bool {
	m.Close()
	select {
	case <-m.drained:
		return true
	default:
	}
	select {
	case <-m.drained:
		return true
	case <-ctx.Done():
		return false
	}
}
func (m *Manager) beginOperation(ctx context.Context) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return ctx, nil, err
	}
	m.lifecycleMu.Lock()
	if m.closed {
		m.lifecycleMu.Unlock()
		return ctx, nil, ErrClosed
	}
	m.active++
	m.lifecycleMu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(m.ctx, cancel)
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			stop()
			cancel()
			m.lifecycleMu.Lock()
			m.active--
			if m.closed && m.active == 0 {
				close(m.drained)
			}
			m.lifecycleMu.Unlock()
		})
	}, nil
}
func (m *Manager) signal() { close(m.changed); m.changed = make(chan struct{}) }
func (m *Manager) state(ctx context.Context, s Subject) (*subjectState, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if m.ctx.Err() != nil {
		return nil, ErrClosed
	}
	if v := m.subjects[s]; v != nil {
		return v, nil
	}
	b, err := m.store.GetBinding(ctx, s)
	if errors.Is(err, ErrNotFound) {
		b = Binding{Subject: s, State: "unbound"}
	} else if err != nil {
		return nil, err
	}
	for _, r := range []*identity.Reference{b.Active, b.Pending} {
		if r != nil {
			if _, err = m.store.GetProfile(ctx, *r); err != nil {
				return nil, err
			}
		}
	}
	v := &subjectState{binding: b}
	m.subjects[s] = v
	return v, nil
}
func (m *Manager) Stage(ctx context.Context, s Subject, ref identity.Reference, expected uint64) (Binding, error) {
	ctx, finish, err := m.beginOperation(ctx)
	if err != nil {
		return Binding{}, err
	}
	defer finish()
	if s.Validate() != nil || ref.Site != s.Site {
		return Binding{}, errors.New("identity subject/profile site mismatch")
	}
	if _, err := m.store.GetProfile(ctx, ref); err != nil {
		return Binding{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	v, err := m.state(ctx, s)
	if err != nil {
		return Binding{}, err
	}
	if v.verifying || v.uncertain || v.binding.Revision != expected {
		return Binding{}, ErrConflict
	}
	b := v.binding.Clone()
	b.Pending = &ref
	b.State = "pending"
	b.Reason = ""
	b.Revision++
	if err = m.store.SaveBindingCAS(ctx, b, expected); err != nil {
		v.uncertain = true
		m.signal()
		return Binding{}, err
	}
	v.binding = b
	m.signal()
	return b.Clone(), nil
}
func (m *Manager) Cancel(ctx context.Context, s Subject, expected uint64) (Binding, error) {
	ctx, finish, err := m.beginOperation(ctx)
	if err != nil {
		return Binding{}, err
	}
	defer finish()
	m.mu.Lock()
	defer m.mu.Unlock()
	v, err := m.state(ctx, s)
	if err != nil {
		return Binding{}, err
	}
	if v.verifying || v.uncertain || v.binding.Revision != expected {
		return Binding{}, ErrConflict
	}
	if v.binding.Pending == nil {
		return Binding{}, ErrConflict
	}
	b := v.binding.Clone()
	b.Pending = nil
	b.Reason = ""
	b.State = "active"
	if b.Active == nil {
		b.State = "unbound"
	}
	b.Revision++
	if err = m.store.SaveBindingCAS(ctx, b, expected); err != nil {
		v.uncertain = true
		m.signal()
		return Binding{}, err
	}
	v.binding = b
	// Staging never replaces the active snapshot. Cancel can immediately resume
	// that already verified identity while its older scopes are still in flight.
	// A restarted or uncertain subject has no cached snapshot to reuse.
	if b.Active == nil || (v.resolved != nil && v.resolved.Reference != *b.Active) {
		v.resolved = nil
	}
	m.signal()
	return b.Clone(), nil
}

// Reverify waits for existing scopes without holding a lock or resource permit.
// Pending changes never automatically bypass authentication after process restart.
func (m *Manager) Reverify(ctx context.Context, s Subject, expected uint64) (out Binding, err error) {
	ctx, finish, err := m.beginOperation(ctx)
	if err != nil {
		return out, err
	}
	defer finish()
	ctx, cancel := context.WithTimeout(ctx, m.options.VerifyTimeout)
	defer cancel()
	stop := context.AfterFunc(m.ctx, cancel)
	defer stop()
	m.mu.Lock()
	v, err := m.state(ctx, s)
	if err != nil {
		m.mu.Unlock()
		return out, err
	}
	if v.uncertain {
		b, readErr := m.store.GetBinding(ctx, s)
		if readErr != nil {
			m.mu.Unlock()
			return out, readErr
		}
		v.binding = b
		v.resolved = nil
		v.uncertain = false
	}
	if v.binding.Revision != expected || v.verifying {
		m.mu.Unlock()
		return out, ErrConflict
	}
	if v.binding.Active == nil && v.binding.Pending == nil {
		m.mu.Unlock()
		return out, ErrNotFound
	}
	v.verifying = true
	m.signal()
	m.mu.Unlock()
	defer func() {
		panicked := recover()
		m.mu.Lock()
		if panicked != nil {
			v.uncertain = true
			v.resolved = nil
			err = fmt.Errorf("%w: identity callback panic", ErrVerification)
			out = v.binding.Clone()
		}
		v.verifying = false
		m.signal()
		m.mu.Unlock()
	}()
	for {
		m.mu.Lock()
		refs := v.refs
		changed := m.changed
		m.mu.Unlock()
		if refs == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-changed:
		}
	}
	m.mu.Lock()
	b := v.binding.Clone()
	m.mu.Unlock()
	ref := b.Active
	if b.Pending != nil {
		ref = b.Pending
	}
	p, err := m.store.GetProfile(ctx, *ref)
	var resolved *identity.ResolvedProfile
	var publish func() error
	if err == nil {
		resolved, err = m.options.Resolve(ctx, s, p.Clone())
	}
	if err == nil && (resolved == nil || resolved.Reference != *ref) {
		err = ErrCorrupt
	}
	if err == nil {
		err = resolved.Validate()
	}
	if err == nil {
		publish, err = m.options.Verify(ctx, s, resolved.Clone())
	}
	if err == nil {
		err = ctx.Err()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		if b.Pending != nil {
			next := b.Clone()
			next.State = "failed"
			next.Reason = "verification-failed"
			next.Revision++
			persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			saveErr := m.store.SaveBindingCAS(persistCtx, next, b.Revision)
			persistCancel()
			if saveErr != nil {
				v.uncertain = true
				return out, errors.Join(err, saveErr)
			}
			v.binding = next
		}
		// A failed detached candidate leaves the active credentials and snapshot
		// intact. Only a failed check of the active identity invalidates its cache.
		if b.Pending == nil {
			v.resolved = nil
		}
		return v.binding.Clone(), fmt.Errorf("%w: %w", ErrVerification, err)
	}
	if b.Pending != nil {
		next := b.Clone()
		next.Active = next.Pending
		next.Pending = nil
		next.State = "active"
		next.Reason = ""
		next.Revision++
		if err = m.store.SaveBindingCAS(ctx, next, b.Revision); err != nil {
			v.uncertain = true
			v.resolved = nil
			return out, err
		}
		v.binding = next
	}
	if publish != nil {
		if err = publish(); err != nil {
			v.uncertain = true
			v.resolved = nil
			return v.binding.Clone(), fmt.Errorf("%w: %w", ErrVerification, err)
		}
	}
	v.resolved = resolved.Clone()
	return v.binding.Clone(), nil
}

type Scope struct {
	manager  *Manager
	subject  Subject
	binding  Binding
	resolved *identity.ResolvedProfile
	once     sync.Once
	mu       sync.Mutex
	closed   bool
	ctx      context.Context
	finish   func()
}

func (s *Scope) Snapshot() *identity.ResolvedProfile {
	if s == nil {
		return nil
	}
	return s.resolved.Clone()
}
func (s *Scope) Binding() Binding {
	if s == nil {
		return Binding{}
	}
	return s.binding.Clone()
}
func (s *Scope) Close() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		if s.finish != nil {
			defer s.finish()
		}
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		if s.manager != nil {
			s.manager.mu.Lock()
			s.manager.subjects[s.subject].refs--
			s.manager.signal()
			s.manager.mu.Unlock()
		}
	})
}
func (m *Manager) Acquire(ctx context.Context, s Subject) (*Scope, error) {
	ctx, finish, err := m.beginOperation(ctx)
	if err != nil {
		return nil, err
	}
	transferred := false
	defer func() {
		if !transferred {
			finish()
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		m.mu.Lock()
		v, err := m.state(ctx, s)
		if err != nil {
			m.mu.Unlock()
			return nil, err
		}
		if v.uncertain {
			m.mu.Unlock()
			return nil, ErrUncertain
		}
		if v.binding.Pending == nil && !v.verifying {
			if v.binding.Active == nil {
				v.refs++
				scope := &Scope{manager: m, subject: s, binding: v.binding.Clone(), ctx: ctx, finish: finish}
				transferred = true
				m.mu.Unlock()
				return scope, nil
			}
			if v.resolved != nil {
				v.refs++
				scope := &Scope{manager: m, subject: s, binding: v.binding.Clone(), resolved: v.resolved.Clone(), ctx: ctx, finish: finish}
				transferred = true
				m.mu.Unlock()
				return scope, nil
			}
			revision := v.binding.Revision
			m.mu.Unlock()
			if _, err = m.Reverify(ctx, s, revision); err != nil && !errors.Is(err, ErrConflict) {
				return nil, err
			}
			continue
		}
		changed := m.changed
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-m.ctx.Done():
			return nil, ErrClosed
		case <-changed:
		}
	}
}

type contextKey struct{}

func FromContext(ctx context.Context) (*Scope, bool) {
	s, ok := ctx.Value(contextKey{}).(*Scope)
	if !ok || s == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s, !s.closed
}
func (m *Manager) Pin(ctx context.Context, s Subject) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return ctx, nil, err
	}
	if m.ctx.Err() != nil {
		return ctx, nil, ErrClosed
	}
	if old, ok := FromContext(ctx); ok {
		if old.subject != s || old.manager != m {
			return ctx, nil, errors.New("identity scope subject mismatch")
		}
		return ctx, func() {}, nil
	}
	scope, err := m.Acquire(ctx, s)
	if err != nil {
		return ctx, nil, err
	}
	return context.WithValue(scope.ctx, contextKey{}, scope), scope.Close, nil
}
func (m *Manager) WithSession(ctx context.Context, site, key string, fn func(context.Context, *identity.ResolvedProfile) error) error {
	if fn == nil {
		return errors.New("identity scope callback required")
	}
	ctx, release, err := m.Pin(ctx, Subject{Site: site, Kind: "session", Key: key})
	if err != nil {
		return err
	}
	defer release()
	scope, _ := FromContext(ctx)
	return fn(ctx, scope.Snapshot())
}

// WithProfile is an explicit one-operation path. It never creates a session binding.
func (m *Manager) WithProfile(ctx context.Context, p identity.Profile, fn func(context.Context, *identity.ResolvedProfile) error) error {
	ctx, finish, err := m.beginOperation(ctx)
	if err != nil {
		return err
	}
	defer finish()
	if fn == nil {
		return errors.New("identity scope callback required")
	}
	if err := p.Validate(); err != nil {
		return err
	}
	r, err := m.options.Resolve(ctx, Subject{Site: p.Site}, p.Clone())
	if err != nil {
		return err
	}
	if r == nil {
		return ErrCorrupt
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(ctx, r.Clone())
}
func (m *Manager) Snapshots(ctx context.Context, site string) ([]Snapshot, error) {
	ctx, finish, err := m.beginOperation(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()
	bindings, err := m.store.ListBindings(ctx, site)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Snapshot, 0, len(bindings))
	for _, b := range bindings {
		snap := Snapshot{Binding: b.Clone()}
		if v := m.subjects[b.Subject]; v != nil {
			snap.InUse = v.refs
			snap.Verifying = v.verifying
			snap.Verified = v.resolved != nil && !v.uncertain
		}
		out = append(out, snap)
	}
	return out, nil
}
