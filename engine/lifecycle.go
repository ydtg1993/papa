package engine

import (
	"context"
	"github.com/ydtg1993/papa/v4/core"
	"sync"
	"time"

	"github.com/ydtg1993/papa/v4/internal/workerpool"
)

// Stop 停止引擎：先取消引擎 ctx（让在途的退避等待尽快结束），再让各阶段工作池排空队列。
//
// 从这一句起，引擎**不再接新活**：活池登记表置上停机位（pools.shutdown），之后任何投递
// 都不会再懒建出新的池 —— 新池会既不在这次快照里、又带着活着的 worker 跑任务，而调用方
// 已经拿 drained=true 去关库了。投进来的任务按"先寄放"处理（行保持 pending，
// 下次启动的启动恢复重新入队），不判失败。
//
// 返回 drained 表示所有阶段与后台 ticker 都在 timeout 内停下，且没有未完成、溢出、
// 延迟或未确认的任务；没排空时 stats 是最后观察到的存留情况。ticker 同样会写库，
// 不等它们就关库，在途的那一跳会报 `sql: database is closed`。
//
// 各阶段与 ticker 从 Stop 入口共用同一个 deadline；不为后等待的对象重新计时。
//
// 拿到 drained=false 时，调用方**不该接着关数据库 / 关浏览器池**：worker goroutine 还活着
// 的可能性仍在（Go 杀不掉它，见 workerpool.Stop），关闭依赖可能使在途写入报 "sql: database is closed"。
func (e *Engine) Stop(timeout time.Duration) (drained bool, stats map[string]core.StopStats) {
	defer e.requestEventsClose()
	deadline := time.Now().Add(timeout)
	e.cancel()
	e.BindAccountHealthEvents(nil)
	tickersDone := make(chan struct{})
	go func() {
		e.tickerWG.Wait()
		close(tickersDone)
	}()
	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	stats = make(map[string]core.StopStats)
	// 遍历活池表（而不是 e.stages）：池子是懒建/回收的，e.stages 里「没建池」的阶段
	// 不在表里，天然被跳过 —— 关停这一步不该自己 panic 掉。
	//
	// 用 shutdown() 而不是 snapshot()：它在同一把锁里**置停机位 + 取快照**，于是"快照之后
	// 才建出来的池"在结构上不存在（后来者一律被拒，任务按"先寄放"处理）。见 pools.go 的注释。
	for name, pool := range e.pools.Shutdown() {
		wg.Add(1)
		go func(stage string, pool *workerpool.WorkerPool[*Task]) {
			defer wg.Done()
			d, inFlight := pool.Stop(max(0, time.Until(deadline)))
			if d {
				return
			}
			main, urgent := pool.QueueDepths()
			mu.Lock()
			stats[stage] = core.StopStats{Unfinished: inFlight, Queued: main + urgent}
			mu.Unlock()
		}(name, pool)
	}
	wg.Wait()
	drained = true
	// 动态 ticker（错误/恢复/每站的轮询队列）也要等在途的那一跳跑完：它们同样会写库。
	// 与阶段池共用同一个 timeout（并发计），等不到就如实说"没排空"。
	select {
	case <-tickersDone:
	default:
		select {
		case <-tickersDone:
		case <-time.After(max(0, time.Until(deadline))):
			drained = false
			e.loggerSet.Engine.Warn("dynamic tickers still running after timeout")
		}
	}
	// Tickers can retain or confirm work while Stop waits. After they exit the
	// snapshots are stable; on timeout report the latest observed retained work.
	// 溢出表里**寄放**的任务也算进存留：被暂停的站点，任务不在任何池子的队列里（那正是收口的语义），
	// 只报池子的存留就会得出 drained=true —— 调用方（app.Run）随即去关库，而几十上百条任务
	// 还在溢出表里躺着。它们的行是 pending，重启后由启动恢复捞，但那一刻**运维看不到任何告警**。
	// 这与池子队列里的任务同一性质（"这一轮没跑完"），所以并进同一个结构。
	for key, count := range e.overflow.Counts() {
		if count == 0 {
			continue
		}
		st := stats[key]
		st.Queued += count
		stats[key] = st
	}
	for _, key := range e.execution.Snapshot() {
		st := stats[key]
		st.Unfinished++
		stats[key] = st
	}
	for _, item := range e.delays.Snapshot() {
		key := stageKeyOf(item.task)
		st := stats[key]
		st.Queued++
		stats[key] = st
	}
	drained = e.durableStopBacklog(deadline, stats) && drained && len(stats) == 0
	if drained {
		e.loggerSet.Engine.Info("all workers stopped")
	}
	select {
	case <-tickersDone:
		e.breakers.recordBreakerStop(drained)
		flushCtx, flushCancel := context.WithDeadline(context.Background(), deadline)
		if err := e.breakers.flushBreakerReports(flushCtx); err != nil {
			drained = false
			e.loggerSet.Engine.Errorf("breaker state not persisted at shutdown: %v", err)
		}
		flushCancel()
	default:
		drained = false
	}
	return drained, stats
}

// Errors 返回引擎的常驻错误通道（单条）：所有池的错误都由各自的转发协程汇到这一条上，
// mdMsgListener 只读它。池是懒建的，逐个取池错误通道会漏掉「启动之后才建的池」——
// 所以这里是常驻一条，而不是启动时快照。
func (e *Engine) Errors() (errors []<-chan error) {
	if e.errorCh == nil {
		return nil
	}
	return []<-chan error{e.errorCh}
}
