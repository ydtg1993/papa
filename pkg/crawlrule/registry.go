package crawlrule

import (
	"errors"
	"sync"
)

// ErrUnverifiedRule means the registered version has never passed activation
// fixtures in this registry.
var ErrUnverifiedRule = errors.New("crawlrule: version has not passed activation fixtures")

// Registry stores immutable compiled versions and an activation history per ID.
// Its zero value is ready for use. State is in-memory and local to this process;
// there is no disk publication, distributed coordination, or network activity.
// Register/Activate/Rollback are safe for concurrent use. Each successful
// activation takes effect atomically after all its explicit fixtures pass.
type Registry struct {
	mu       sync.RWMutex
	versions map[string]map[string]*Compiled
	history  map[string][]string
	verified map[string]map[string]bool
}

func NewRegistry() *Registry { return &Registry{} }

// Register compiles a new ID/version without activating it. Even an identical
// repeat registration is a conflict; callers must choose a new version.
func (r *Registry) Register(rule Rule) error {
	compiled, err := Compile(rule)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.versions == nil {
		r.versions = make(map[string]map[string]*Compiled)
		r.history = make(map[string][]string)
	}
	versions := r.versions[compiled.rule.ID]
	if versions == nil {
		versions = make(map[string]*Compiled)
		r.versions[compiled.rule.ID] = versions
	}
	if _, exists := versions[compiled.rule.Version]; exists {
		return ErrVersionConflict
	}
	versions[compiled.rule.Version] = compiled
	return nil
}

// Get returns an immutable compiled version, including versions not yet verified.
// Use Active or Verified when evaluating production data. Call Rule for an
// editable copy.
func (r *Registry) Get(id, version string) (*Compiled, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c := r.versions[id][version]
	if c == nil {
		return nil, ErrVersionNotFound
	}
	return c, nil
}

// Verified returns a version that has passed a successful Activate call. Unlike
// Active, it accepts a pinned version and remains available after rollback or a
// later failed activation, so already submitted tasks keep their verified rule.
// Direct calls to Compiled.Verify do not publish a version to this registry.
func (r *Registry) Verified(id, version string) (*Compiled, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c := r.versions[id][version]
	if c == nil {
		return nil, ErrVersionNotFound
	}
	if !r.verified[id][version] {
		return nil, ErrUnverifiedRule
	}
	return c, nil
}

func (r *Registry) Active(id string) (*Compiled, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	history := r.history[id]
	if len(history) == 0 {
		return nil, ErrNoActiveRule
	}
	return r.versions[id][history[len(history)-1]], nil
}

// Activate requires fresh explicit fixtures, including when reactivating an old
// version. Failed verification does not change the active rule or its history.
// Concurrent activations are ordered by completion of verification and locking.
func (r *Registry) Activate(id, version string, fixtures []Fixture) (FixtureReport, error) {
	c, err := r.Get(id, version)
	if err != nil {
		return FixtureReport{}, err
	}
	report, err := c.Verify(fixtures)
	if err != nil {
		return report, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.verified == nil {
		r.verified = make(map[string]map[string]bool)
	}
	if r.verified[id] == nil {
		r.verified[id] = make(map[string]bool)
	}
	r.verified[id][version] = true
	history := r.history[id]
	if len(history) == 0 || history[len(history)-1] != version {
		r.history[id] = append(history, version)
	}
	return report, nil
}

// Rollback returns to the preceding successfully activated version. It consumes
// one history entry, so repeated calls walk backward and never toggle forward to
// a rejected version. A new activation after rollback starts a new history tail.
func (r *Registry) Rollback(id string) (*Compiled, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	history := r.history[id]
	if len(history) < 2 {
		return nil, ErrNoRollback
	}
	history = history[:len(history)-1]
	r.history[id] = history
	return r.versions[id][history[len(history)-1]], nil
}
