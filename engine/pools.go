package engine

import (
	"sync"
	"time"

	"github.com/ydtg1993/papa/v3/internal/workerpool"
)

// poolRegistry 活池登记表：stage 名 -> 当前活着的池。所有投递、关停、错误通道都从它取池，
// 不再散落在 stageInfo.workerPool 上（那是裸指针，池一旦懒建/回收就会并发读写）。
//
// key 就是 stage 名：e.stages 是 map[string]*stageInfo，stage 名全局唯一、自带 config.Site，
// 不需要把 site / 队列类型再塞进 key（见 docs/SCHEDULER_REDESIGN.md §4）。
//
// lastSubmit 记每个池「最后一次收到任务」的时刻：空闲回收（reapIdle）据此判断池闲置了多久。
// 只在投递时刷新（getOrCreate），任务跑多久都不影响 —— 反正有「队列空 + 无在途」这道安全守卫兜底。
type poolRegistry struct {
	mu         sync.RWMutex
	pools      map[string]*workerpool.WorkerPool[*Task]
	lastSubmit map[string]time.Time
}

func newPoolRegistry() *poolRegistry {
	return &poolRegistry{
		pools:      make(map[string]*workerpool.WorkerPool[*Task]),
		lastSubmit: make(map[string]time.Time),
	}
}

// set 登记一个池（测试与历史路径用；生产走 getOrCreate）。
func (r *poolRegistry) set(stage string, p *workerpool.WorkerPool[*Task]) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pools[stage] = p
	r.lastSubmit[stage] = time.Now()
}

// delete 撤下一个池。已在 reapIdle 里内联（它持写锁，调带锁的 delete 会死锁），这里不另设方法。

// get 取某个阶段的活池；没有返回 nil。
func (r *poolRegistry) get(stage string) *workerpool.WorkerPool[*Task] {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.pools[stage]
}

// getOrCreate 取池，没有就建，并**刷新 lastSubmit**（每次投递都走它，所以「最后一次收到任务」是准的）。
// create 在持有写锁时调用：建池只在「首次 submit」发生一次，之后全是 map 命中；短暂持锁换来
// 「并发下只建一份」的保证，值。
func (r *poolRegistry) getOrCreate(stage string, create func() *workerpool.WorkerPool[*Task]) *workerpool.WorkerPool[*Task] {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.pools[stage]; ok {
		r.lastSubmit[stage] = now
		return p
	}
	p := create()
	r.pools[stage] = p
	r.lastSubmit[stage] = now
	return p
}

// reapIdle 回收「连续 idle 时长没有新任务、且此刻空着（队列空 + 无在途）」的池，返回被回收的 stage → 池。
// 池要带出来：调用方清 stats 时用它做身份校验（回收与清 stats 之间，同阶段可能已被重新提交建了新池）。
//
// 安全守卫（队列空 + 无在途）不能省：池可能抱着一个长任务（大文件 / 整部剧）跑了几分钟没收到新任务，
// 光看 lastSubmit 早就超了 idle —— 但此刻 Stop 它等于把在途任务腰斩。判空用 pool.Stats() 的
// inProgress 与 queueLen，二者都为 0 才真回收。
func (r *poolRegistry) reapIdle(idle, stopTimeout time.Duration) map[string]*workerpool.WorkerPool[*Task] {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	reaped := make(map[string]*workerpool.WorkerPool[*Task])
	for stage, pool := range r.pools {
		if now.Sub(r.lastSubmit[stage]) < idle {
			continue
		}
		if _, _, _, inProgress, queueLen := pool.Stats(); inProgress != 0 || queueLen != 0 {
			continue
		}
		pool.Stop(stopTimeout)
		delete(r.pools, stage)
		delete(r.lastSubmit, stage)
		reaped[stage] = pool
	}
	return reaped
}

// snapshot 活池快照（Stop / Errors 用）。nil 接收者返回 nil：手工构造的 Engine 可能没有这张表。
func (r *poolRegistry) snapshot() map[string]*workerpool.WorkerPool[*Task] {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]*workerpool.WorkerPool[*Task], len(r.pools))
	for k, v := range r.pools {
		out[k] = v
	}
	return out
}
