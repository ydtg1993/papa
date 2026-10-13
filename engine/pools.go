package engine

import (
	"sync"
	"time"

	"github.com/ydtg1993/papa/v3/internal/workerpool"
)

// poolRegistry 活池登记表：阶段键 -> 当前活着的池。所有投递、关停、错误通道都从它取池，
// 不再散落在 stageInfo.workerPool 上（那是裸指针，池一旦懒建/回收就会并发读写）。
//
// key 是 **`core.StageKey(site, stage)`**（与 e.stages 的键同一套）：阶段名只在本站内唯一，
// 两个站的 catalog 是两个池子，所以键里必须带站点 —— 一份"每站每阶段"的池子只能这样表达。
//
// lastSubmit 记每个池「最后一次收到任务」的时刻：空闲回收（reapIdle）据此判断池闲置了多久。
// 只在投递时刷新（getOrCreate），任务跑多久都不影响 —— 反正有「队列空 + 无在途」这道安全守卫兜底。
type poolRegistry struct {
	mu         sync.RWMutex
	pools      map[string]*workerpool.WorkerPool[*Task]
	lastSubmit map[string]time.Time
	// stopped 停机位：Engine.Stop 置上之后 getOrCreate 一律拒绝建池（见 shutdown）。
	// 它是**单向**的，与 e.cancel() 一样不可逆 —— 引擎本来就不能重启。
	stopped bool
}

func newPoolRegistry() *poolRegistry {
	return &poolRegistry{
		pools:      make(map[string]*workerpool.WorkerPool[*Task]),
		lastSubmit: make(map[string]time.Time),
	}
}

// set 登记一个池（测试与历史路径用；生产走 getOrCreate）。
func (r *poolRegistry) set(key string, p *workerpool.WorkerPool[*Task]) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pools[key] = p
	r.lastSubmit[key] = time.Now()
}

// delete 撤下一个池。已在 reapIdle 里内联（它持写锁，调带锁的 delete 会死锁），这里不另设方法。

// get 取某个阶段的活池；没有返回 nil。
func (r *poolRegistry) get(key string) *workerpool.WorkerPool[*Task] {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.pools[key]
}

// getOrCreate 取池，没有就建，并**刷新 lastSubmit**（每次投递都走它，所以「最后一次收到任务」是准的）。
// create 在持有写锁时调用：建池只在「首次 submit」发生一次，之后全是 map 命中；短暂持锁换来
// 「并发下只建一份」的保证，值。
func (r *poolRegistry) getOrCreate(key string, create func() *workerpool.WorkerPool[*Task]) *workerpool.WorkerPool[*Task] {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		// 停机之后不再建池：停机的语义是"没有新活"，新池会既不在 Stop 取的那份快照里、
		// 又带着一群活着的 worker 跑任务，而调用方（app.Run）已经按 drained=true 去关库了。
		// 返回 nil 由 submitToStage 翻成"先寄放"，任务留在待处理态等下次启动的启动恢复。
		return nil
	}
	if p, ok := r.pools[key]; ok {
		r.lastSubmit[key] = now
		return p
	}
	p := create()
	r.pools[key] = p
	r.lastSubmit[key] = now
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
	for key, pool := range r.pools {
		if now.Sub(r.lastSubmit[key]) < idle {
			continue
		}
		if _, _, _, inProgress, queueLen := pool.Stats(); inProgress != 0 || queueLen != 0 {
			continue
		}
		pool.Stop(stopTimeout)
		delete(r.pools, key)
		delete(r.lastSubmit, key)
		reaped[key] = pool
	}
	return reaped
}

// shutdown 置停机位，并在**同一把写锁里**返回活池快照 —— Engine.Stop 用它代替 snapshot()。
//
// 置位与取快照必须原子，这是这段的关键：分成两步的话，两步之间进来的 getOrCreate 会建出一个
// 既不在这份快照里、又已经不该存在的池 —— 就是"Stop 之后新池逃出关停"那条时序窗。
// 而框架自己的收尾协程恰好就在这个窗口里投递：延迟投递的 deliverDelayed 与溢出回灌的
// drainSpilled 都会走到 submitToStage → getOrCreatePool（它们只在 ctx 上 select，
// 被 cancel 唤起的那个 tick 照样会把手里这条投出去）。
//
// 原子之后只剩两种结局，没有第三种：要么池已在快照里（Stop 会停它、等它），
// 要么 getOrCreate 被拒（投递方按"先寄放"处理，行保持 pending，下次启动恢复重新入队）。
//
// nil 接收者返回 nil（与 snapshot 同一处理：手工构造的 Engine 可能没有这张表）。
func (r *poolRegistry) shutdown() map[string]*workerpool.WorkerPool[*Task] {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = true
	out := make(map[string]*workerpool.WorkerPool[*Task], len(r.pools))
	for k, v := range r.pools {
		out[k] = v
	}
	return out
}

// isStopped 停机位是否已置。submitToStage 用它把"引擎在停机"与"阶段没注册"分开报 ——
// 两者都会让 getOrCreatePool 返回 nil，但给运维的结论完全不同。
func (r *poolRegistry) isStopped() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.stopped
}
