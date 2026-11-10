package account

import (
	"context"
	"errors"
	"sort"
)

// ProtectReferences serializes task INSERTs with pool deletion in this process.
// fn must only persist references: never acquire accounts, enqueue tasks or call
// another pool mutation from it. The embedding application owns the task schema.
func (p *Pools) ProtectReferences(ctx context.Context, keys []PoolKey, fn func() error) error {
	if fn == nil {
		return errors.New("account pool reference callback required")
	}
	if err := p.writes.LockContext(ctx); err != nil {
		return err
	}
	defer p.writes.Unlock()
	unique := map[PoolKey]bool{}
	for _, key := range keys {
		unique[key] = true
	}
	keys = make([]PoolKey, 0, len(unique))
	for key := range unique {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Site == keys[j].Site {
			return keys[i].Name < keys[j].Name
		}
		return keys[i].Site < keys[j].Site
	})
	p.mu.Lock()
	for _, key := range keys {
		q := p.pools[key]
		if q == nil || q.state.Deleted {
			p.mu.Unlock()
			return ErrPoolUnknown
		}
		if q.state.Disabled {
			p.mu.Unlock()
			return ErrPoolDisabled
		}
	}
	p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}

// GetIncludingDeleted exposes a tombstone's revision for a deliberate recreation.
// Normal Get/Acquire still treat it as absent, and Snapshot excludes tombstones.
func (p *Pools) GetIncludingDeleted(key PoolKey) (PoolSnapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	q := p.pools[key]
	if q == nil {
		return PoolSnapshot{}, ErrPoolUnknown
	}
	return p.snapshotLocked(q), nil
}
