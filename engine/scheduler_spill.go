package engine

import "sync"

// spillQueue owns overflow membership and its lock. Drain takes ownership of
// detached buckets, then returns undelivered entries via Restore/Put. No lock
// crosses SQL or pool submission. The zero value is usable.
type spillQueue struct {
	mu      sync.Mutex
	buckets map[string][]*Task
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
	for key, ts := range q.buckets {
		out[key] = len(ts)
	}
	return out
}
func (q *spillQueue) Restore(key string, tasks []*Task) {
	if len(tasks) == 0 {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.buckets == nil {
		q.buckets = make(map[string][]*Task)
	}
	q.buckets[key] = append(q.buckets[key], tasks...)
}
func (q *spillQueue) Put(task *Task) {
	key := stageKeyOf(task)
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, t := range q.buckets[key] {
		if (task.ID != 0 && t.ID == task.ID) || (task.ID == 0 && t.Unique() == task.Unique()) {
			return
		}
	}
	if q.buckets == nil {
		q.buckets = make(map[string][]*Task)
	}
	q.buckets[key] = append(q.buckets[key], task)
}
func (q *spillQueue) Take() map[string][]*Task {
	q.mu.Lock()
	defer q.mu.Unlock()
	all := q.buckets
	q.buckets = nil
	return all
}
