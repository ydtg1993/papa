package engine

import (
	"errors"
	"sync"
	"time"

	"github.com/ydtg1993/papa/v4/models"
)

// SchedulerLimits bounds task references, not the variable-size payload bytes.
// Configure before ApplyRegisterStage. Zero fields use the finite defaults.
type SchedulerLimits struct {
	Global, Site, Stage int
	RefillBatch         int
	DelayWindow         time.Duration
}

var ErrSchedulerCapacity = errors.New("scheduler resident task capacity exhausted")
var ErrSchedulerStarted = errors.New("scheduler limits must be configured before scheduling work")

func normalizeSchedulerLimits(l SchedulerLimits) SchedulerLimits {
	if l.Global <= 0 {
		l.Global = 4096
	}
	if l.Site <= 0 {
		l.Site = 2048
	}
	if l.Stage <= 0 {
		l.Stage = 1024
	}
	if l.RefillBatch <= 0 {
		l.RefillBatch = 64
	}
	if l.DelayWindow <= 0 {
		l.DelayWindow = time.Minute
	}
	return l
}

// residentSet owns admission and transfers. Reservation never waits; a worker
// submitting children therefore cannot wait for its own worker slot.
type residentSet struct {
	mu            sync.Mutex
	limits        SchedulerLimits
	tasks         map[*Task]string
	bound         map[*Task]int
	ids           map[int]int
	sites, stages map[string]int
}

func newResidentSet(l SchedulerLimits) *residentSet {
	return &residentSet{limits: normalizeSchedulerLimits(l), tasks: map[*Task]string{}, bound: map[*Task]int{}, ids: map[int]int{}, sites: map[string]int{}, stages: map[string]int{}}
}
func (r *residentSet) admit(t *Task, state string) bool {
	if r == nil {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tasks[t]; ok {
		if r.bound[t] == 0 && t.ID != 0 {
			r.bound[t] = t.ID
			r.ids[t.ID]++
		}
		r.tasks[t] = state
		return true
	}
	key := stageKeyOf(t)
	if len(r.tasks) >= r.limits.Global || r.sites[t.Site] >= r.limits.Site || r.stages[key] >= r.limits.Stage {
		return false
	}
	r.tasks[t] = state
	r.bound[t] = t.ID
	if t.ID != 0 {
		r.ids[t.ID]++
	}
	r.sites[t.Site]++
	r.stages[key]++
	return true
}
func (r *residentSet) move(t *Task, state string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tasks[t]; ok {
		if r.bound[t] == 0 && t.ID != 0 {
			r.bound[t] = t.ID
			r.ids[t.ID]++
		}
		r.tasks[t] = state
	}
}
func (r *residentSet) release(t *Task, only string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state, ok := r.tasks[t]
	if !ok || (only != "" && state != only) {
		return
	}
	delete(r.tasks, t)
	id := r.bound[t]
	delete(r.bound, t)
	if id != 0 {
		r.ids[id]--
		if r.ids[id] == 0 {
			delete(r.ids, id)
		}
	}
	r.sites[t.Site]--
	r.stages[stageKeyOf(t)]--
}
func (r *residentSet) containsID(id int) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ids[id] > 0
}
func (r *residentSet) count() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.tasks)
}
func (e *Engine) SetSchedulerLimits(l SchedulerLimits) error {
	if e.schedulerStarted || (e.residents != nil && e.residents.count() != 0) {
		return ErrSchedulerStarted
	}
	e.residents = newResidentSet(l)
	return nil
}

// SchedulerResidentCount includes queued, running and detached work.
func (e *Engine) SchedulerResidentCount() int { return e.residents.count() }
func (e *Engine) schedulerLimits() SchedulerLimits {
	if e.residents != nil {
		return e.residents.limits
	}
	return normalizeSchedulerLimits(SchedulerLimits{})
}

func (r *residentSet) state(t *Task) string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tasks[t]
}

func (r *residentSet) available(site, stage string) bool {
	if r == nil {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.tasks) < r.limits.Global && r.sites[site] < r.limits.Site && r.stages[stage] < r.limits.Stage
}

func (r *residentSet) capacity(site, stage string) int {
	if r == nil {
		return 64
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return max(0, min(r.limits.Global-len(r.tasks), r.limits.Site-r.sites[site], r.limits.Stage-r.stages[stage]))
}

func (t *Task) staleDelivery(row models.CrawlerTask) bool {
	return t.deliveryVersion != nil && (t.deliveryVersion.Repeat != row.Repeat || t.deliveryVersion.Reprocess != row.Reprocess)
}
