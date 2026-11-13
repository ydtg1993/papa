package engine

import (
	"fmt"
	"sort"
	"sync"
)

// spillQueue owns membership and round-robin selection. Detached batches remain
// the caller's responsibility until delivered or restored; no lock spans SQL.
type spillQueue struct {
	mu      sync.Mutex
	buckets map[string][]*Task
	members map[string]map[string]*Task
	cursor  string
}

func spillIdentity(t *Task) string {
	if t.ID != 0 {
		if t.deliveryVersion != nil {
			return fmt.Sprintf("%d/%d/%d", t.ID, t.deliveryVersion.Repeat, t.deliveryVersion.Reprocess)
		}
		return fmt.Sprintf("%d/%d/%s", t.ID, t.Repeat, t.breakerRound)
	}
	return t.Unique()
}
func (q *spillQueue) Count() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for _, ts := range q.buckets {
		n += len(ts)
	}
	return n
}
func (q *spillQueue) Counts() map[string]int {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make(map[string]int, len(q.buckets))
	for k, ts := range q.buckets {
		out[k] = len(ts)
	}
	return out
}
func (q *spillQueue) putLocked(key string, t *Task) *Task {
	if q.buckets == nil {
		q.buckets = make(map[string][]*Task)
	}
	if q.members == nil {
		q.members = make(map[string]map[string]*Task)
	}
	if q.members[key] == nil {
		q.members[key] = make(map[string]*Task)
	}
	id := spillIdentity(t)
	if old, ok := q.members[key][id]; ok {
		return old
	}
	q.members[key][id] = t
	q.buckets[key] = append(q.buckets[key], t)
	return nil
}
func (q *spillQueue) Restore(key string, tasks []*Task) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, t := range tasks {
		q.putLocked(key, t)
	}
}
func (q *spillQueue) Put(t *Task) *Task {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.putLocked(stageKeyOf(t), t)
}
func (q *spillQueue) Take() map[string][]*Task {
	q.mu.Lock()
	defer q.mu.Unlock()
	all := q.buckets
	q.buckets = nil
	q.members = nil
	return all
}

// TakeBatch visits one bucket per call, in stable cyclic order. Keeping the
// remainder in place bounds detached memory even when producers keep arriving.
func (q *spillQueue) TakeBatch(limit int) (string, []*Task) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if limit <= 0 || len(q.buckets) == 0 {
		return "", nil
	}
	keys := make([]string, 0, len(q.buckets))
	for k, ts := range q.buckets {
		if len(ts) > 0 {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return "", nil
	}
	sort.Strings(keys)
	i := sort.SearchStrings(keys, q.cursor)
	for i < len(keys) && keys[i] <= q.cursor {
		i++
	}
	if i == len(keys) {
		i = 0
	}
	key := keys[i]
	q.cursor = key
	ts := q.buckets[key]
	n := min(limit, len(ts))
	out := append([]*Task(nil), ts[:n]...)
	for _, t := range out {
		delete(q.members[key], spillIdentity(t))
	}
	copy(ts, ts[n:])
	clear(ts[len(ts)-n:])
	ts = ts[:len(ts)-n]
	if len(ts) == 0 {
		delete(q.buckets, key)
		delete(q.members, key)
	} else {
		q.buckets[key] = ts
	}
	return key, out
}
