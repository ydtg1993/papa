package workerpool

import (
	"context"
	"time"
)

// SubmitContext 等待常规队列的实际容量，不创建等待 goroutine。
// nil 表示池已接收任务；取消或停止返回错误，任务仍归调用方负责。
func (p *WorkerPool[T]) SubmitContext(ctx context.Context, task T) error {
	return p.submitContext(ctx, task, false)
}

// SubmitUrgentContext 优先进入有界快车道；快车道满时可进入常规队列。
// 两条队列都满才等待，等待可被 ctx 或 Stop 解除。
func (p *WorkerPool[T]) SubmitUrgentContext(ctx context.Context, task T) error {
	return p.submitContext(ctx, task, true)
}

// WaitingSubmitters 包含正在尝试或等待的可取消提交，空闲回收不能忽略它们。
func (p *WorkerPool[T]) WaitingSubmitters() int64 { return p.waiting.Load() }

// StopIfIdle atomically closes admission only when no submitted work or waiting
// producer exists. A separate Stats/Stop pair allows a sender to enter between
// the idle check and shutdown, leaving an active worker outside the registry.
func (p *WorkerPool[T]) StopIfIdle(timeout time.Duration) bool {
	p.mu.Lock()
	if p.stopped || p.waiting.Load() != 0 || p.submitted.Load()-p.completed.Load()-p.failed.Load() != 0 {
		p.mu.Unlock()
		return false
	}
	p.stopped = true
	close(p.stopCh)
	p.mu.Unlock()
	p.Stop(timeout)
	return true
}

// Promote moves one already queued task to the urgent lane without acquiring a
// second delivery or changing submission counters. The predicate must not call
// pool methods. A full urgent lane or an already consumed task returns false.
func (p *WorkerPool[T]) Promote(match func(T) bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped || len(p.urgentQueue) == cap(p.urgentQueue) {
		return false
	}
	// Admission is excluded by mu; consumers can only create more room. Restore
	// nonmatching tasks in their original order before allowing another sender.
	remaining := make([]T, 0, len(p.taskQueue))
	var target T
	found := false
	for {
		select {
		case task := <-p.taskQueue:
			if !found && match(task) {
				target, found = task, true
			} else {
				remaining = append(remaining, task)
			}
		default:
			for _, task := range remaining {
				p.taskQueue <- task
			}
			if found {
				p.urgentQueue <- target
				close(p.spaceChanged)
				p.spaceChanged = make(chan struct{})
			}
			return found
		}
	}
}

func (p *WorkerPool[T]) submitContext(ctx context.Context, task T, urgent bool) error {
	p.waiting.Add(1)
	defer p.waiting.Add(-1)
	for {
		p.mu.Lock()
		if err := ctx.Err(); err != nil {
			p.mu.Unlock()
			return err
		}
		if p.stopped {
			p.mu.Unlock()
			return ErrStopped
		}
		accepted := false
		if urgent {
			select {
			case p.urgentQueue <- task:
				accepted = true
			default:
			}
		}
		if !accepted {
			select {
			case p.taskQueue <- task:
				accepted = true
			default:
			}
		}
		if accepted {
			p.submitted.Add(1)
			p.mu.Unlock()
			return nil
		}
		changed := p.spaceChanged
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.stopCh:
			return ErrStopped
		case <-changed:
		}
	}
}
