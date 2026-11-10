package runtime

import (
	"context"
	"sort"
	"sync"

	"github.com/ydtg1993/papa/v4/pkg/browser/identity"
)

type MemoryStore struct {
	mu       sync.Mutex
	profiles map[identity.Reference]identity.Profile
	bindings map[Subject]Binding
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{profiles: map[identity.Reference]identity.Profile{}, bindings: map[Subject]Binding{}}
}
func (s *MemoryStore) AppendProfile(ctx context.Context, p identity.Profile, expected uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.Validate(); err != nil {
		return err
	}
	if p.Version != expected+1 || p.Version == 0 {
		return ErrConflict
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var latest uint64
	for r := range s.profiles {
		if r.Site == p.Site && r.ID == p.ID && r.Version > latest {
			latest = r.Version
		}
	}
	if latest != expected {
		return ErrConflict
	}
	s.profiles[p.Reference()] = p.Clone()
	return nil
}
func (s *MemoryStore) GetProfile(ctx context.Context, r identity.Reference) (identity.Profile, error) {
	if err := ctx.Err(); err != nil {
		return identity.Profile{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.profiles[r]
	if !ok {
		return p, ErrNotFound
	}
	return p.Clone(), nil
}
func (s *MemoryStore) ListProfiles(ctx context.Context, site string) ([]identity.Profile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []identity.Profile{}
	for _, p := range s.profiles {
		if site == "" || p.Site == site {
			out = append(out, p.Clone())
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Site != out[j].Site {
			return out[i].Site < out[j].Site
		}
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return out[i].Version < out[j].Version
	})
	return out, nil
}
func (s *MemoryStore) GetBinding(ctx context.Context, k Subject) (Binding, error) {
	if err := ctx.Err(); err != nil {
		return Binding{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.bindings[k]
	if !ok {
		return b, ErrNotFound
	}
	return b.Clone(), b.Validate()
}
func (s *MemoryStore) ListBindings(ctx context.Context, site string) ([]Binding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Binding{}
	for _, b := range s.bindings {
		if err := b.Validate(); err != nil {
			return nil, err
		}
		if site == "" || b.Subject.Site == site {
			out = append(out, b.Clone())
		}
	}
	sortBindings(out)
	return out, nil
}
func sortBindings(out []Binding) {
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Subject, out[j].Subject
		if a.Site != b.Site {
			return a.Site < b.Site
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Key < b.Key
	})
}
func (s *MemoryStore) SaveBindingCAS(ctx context.Context, b Binding, expected uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := b.Validate(); err != nil {
		return err
	}
	if b.Revision != expected+1 {
		return ErrConflict
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bindings[b.Subject].Revision != expected {
		return ErrConflict
	}
	for _, r := range []*identity.Reference{b.Active, b.Pending} {
		if r != nil {
			if _, ok := s.profiles[*r]; !ok {
				return ErrNotFound
			}
		}
	}
	s.bindings[b.Subject] = b.Clone()
	return nil
}
