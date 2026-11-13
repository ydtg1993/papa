package account

import (
	"context"
	"sync"
)

// MemoryPoolStore is useful for deterministic embedded tests and transient apps.
// Production restart recovery requires a durable PoolStore such as gormstore.GORMPoolStore.
type MemoryPoolStore struct {
	mu         sync.Mutex
	pools      map[PoolKey]PoolState
	management map[Key]ManagementState
}

func NewMemoryPoolStore() *MemoryPoolStore {
	return &MemoryPoolStore{pools: map[PoolKey]PoolState{}, management: map[Key]ManagementState{}}
}
func (s *MemoryPoolStore) LoadPools(ctx context.Context) ([]PoolState, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PoolState, 0, len(s.pools))
	for _, v := range s.pools {
		out = append(out, clonePool(v))
	}
	return out, nil
}
func (s *MemoryPoolStore) LoadManagement(ctx context.Context) ([]ManagementState, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ManagementState, 0, len(s.management))
	for _, v := range s.management {
		out = append(out, cloneManagement(v))
	}
	return out, nil
}
func (s *MemoryPoolStore) SavePoolCAS(ctx context.Context, v PoolState, expected uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pools[v.Key].Revision != expected || v.Revision <= expected {
		return ErrPoolConflict
	}
	s.pools[v.Key] = clonePool(v)
	return nil
}
func (s *MemoryPoolStore) SaveManagementCAS(ctx context.Context, v ManagementState, expected uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.management[v.Key].Version != expected || v.Version <= expected {
		return ErrPoolConflict
	}
	s.management[v.Key] = cloneManagement(v)
	return nil
}
