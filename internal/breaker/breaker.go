// Package breaker 是框架级的熔断闸门：滑动窗口统计**终态失败**数，达到阈值就把所有阶段的
// worker 闸住不再取任务，等人处理完再手动放行。
//
// 它由两件事组成：
//   - 计数：窗口内「任务最终失败」的次数（不是每次尝试失败 —— 见 RecordFailure）；
//   - 闸门（Gate）：worker 每次取任务前探一次，暂停时 worker 就停在原地（见 workerpool.Gate）。
//
// 它**不 import workerpool**：池子那边声明了自己需要的最小接口，本包在结构上满足它即可。
// 这样池子不认识熔断，熔断也不认识池子。
package breaker

import (
	"sync"
	"time"

	"github.com/ydtg1993/papa/v2/core"
)

// bucketCount 滑动窗口切成多少个等宽桶。求和时只算还在窗口内的桶，所以窗口是「近似」的，
// 粒度 = window/bucketCount。
const bucketCount = 60

// defaultWindow 未配置窗口时的默认值。
const defaultWindow = 5 * time.Minute

// Config 熔断配置（调用方已把默认值填好的版本）。
type Config struct {
	Enabled   bool
	Window    time.Duration // <= 0 用默认 5m
	Threshold int           // <= 0 视为「不熔断」（只统计）
}

// Breaker 熔断器：滑动窗口计数 + 一道暂停闸门。
// 所有方法都容忍 nil 接收者（引擎没配熔断时就是一个 nil 指针），省得调用处到处判空。
type Breaker struct {
	enabled     bool
	threshold   int
	window      time.Duration
	bucketWidth time.Duration
	now         func() time.Time // 测试可注入

	// 滑动窗口。counts[i] 计的是槽位 slots[i] 里的失败数；槽位过期（比当前槽位早 bucketCount 个
	// 以上）即作废。存「槽位号」而不是时间戳，是为了让 Add 只碰一个桶、Sum 只读 60 个 int64。
	mu     sync.Mutex
	counts [bucketCount]int64
	slots  [bucketCount]int64

	// 闸门。Pause 时**换一个新 channel**、Resume 时 close 掉它 —— 被关掉的那个不可能再被复用，
	// 所以「暂停 → 恢复 → 再暂停」不会出现「恢复信号早就关着了、第二次暂停立刻被放行」。
	gateMu   sync.RWMutex
	paused   bool
	resumeCh chan struct{}

	// 触发现场：Pause 那一刻的快照，**不随窗口滑动改写** —— 排查时要看的是"当时为什么断的"。
	reason    string
	stage     string
	failures  int
	pausedAt  time.Time
	resumedAt time.Time

	onTrip func(core.BreakerStatus)
}

// New 构造熔断器。onTrip 在**自动触发**时回调一次（可为 nil），宿主用它发告警。
func New(cfg Config, onTrip func(core.BreakerStatus)) *Breaker {
	if cfg.Window <= 0 {
		cfg.Window = defaultWindow
	}
	return &Breaker{
		enabled:     cfg.Enabled,
		threshold:   cfg.Threshold,
		window:      cfg.Window,
		bucketWidth: cfg.Window / bucketCount,
		now:         time.Now,
		resumeCh:    make(chan struct{}),
		onTrip:      onTrip,
	}
}

// Enabled 报告熔断是否启用。
func (b *Breaker) Enabled() bool { return b != nil && b.enabled }

// RecordFailure 记一次**终态失败**（任务重试耗尽、或不可重试），返回是否因此触发了熔断。
//
// 为什么只数终态失败：一个任务重试 3 次会走 3 次 attempt，若按 attempt 计，几个烂 URL
// 就能把窗口灌满、熔断误触发 —— 那会把整个爬虫停掉，代价远大于放着几条烂任务不管。
func (b *Breaker) RecordFailure(stage string) bool {
	if b == nil || !b.enabled {
		return false
	}
	n := b.add(1)
	if b.threshold <= 0 || n < b.threshold {
		return false
	}
	if !b.pause("窗口内终态失败数达到阈值", stage, n) {
		return false
	}
	if b.onTrip != nil {
		b.onTrip(b.Status())
	}
	return true
}

// Pause 手动闸住（同样走这套闸门，后台/业务都能用）。已在暂停态返回 false。
func (b *Breaker) Pause(reason string) bool {
	if b == nil {
		return false
	}
	return b.pause(reason, "", b.Sum())
}

// Resume 放行。本来就没暂停返回 false。
func (b *Breaker) Resume() bool {
	if b == nil {
		return false
	}
	b.gateMu.Lock()
	defer b.gateMu.Unlock()
	if !b.paused {
		return false
	}
	b.paused = false
	b.resumedAt = b.now()
	close(b.resumeCh)
	return true
}

// Paused 只读探询，不阻塞。实现 workerpool.Gate。
func (b *Breaker) Paused() bool {
	if b == nil {
		return false
	}
	b.gateMu.RLock()
	defer b.gateMu.RUnlock()
	return b.paused
}

// Wait 阻塞到放行或 stop 关闭；返回 true = 继续干活，false = 该停机了。实现 workerpool.Gate。
func (b *Breaker) Wait(stop <-chan struct{}) bool {
	if b == nil {
		return true
	}
	b.gateMu.RLock()
	if !b.paused {
		b.gateMu.RUnlock()
		return true
	}
	ch := b.resumeCh
	b.gateMu.RUnlock()

	select {
	case <-ch:
		return true
	case <-stop:
		return false
	}
}

// Sum 返回当前窗口内的终态失败数。
func (b *Breaker) Sum() int {
	if b == nil || !b.enabled {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return int(b.sumLocked())
}

// Status 返回状态快照（含触发时留下的现场）。
func (b *Breaker) Status() core.BreakerStatus {
	if b == nil {
		return core.BreakerStatus{}
	}
	b.gateMu.RLock()
	st := core.BreakerStatus{
		Enabled:   b.enabled,
		Window:    b.window,
		Threshold: b.threshold,
		Paused:    b.paused,
		PausedAt:  b.pausedAt,
		ResumedAt: b.resumedAt,
		Reason:    b.reason,
		Stage:     b.stage,
		Failures:  b.failures,
	}
	b.gateMu.RUnlock()
	st.InWindow = b.Sum()
	return st
}

// pause 置为暂停态；已在暂停态返回 false。调用方负责触发回调。
func (b *Breaker) pause(reason, stage string, failures int) bool {
	b.gateMu.Lock()
	defer b.gateMu.Unlock()
	if b.paused {
		return false
	}
	b.paused = true
	b.resumeCh = make(chan struct{}) // 换新的：下次 Resume 关的是它
	b.reason = reason
	b.stage = stage
	b.failures = failures
	b.pausedAt = b.now()
	return true
}

// add 把当前槽位的计数加 n，返回加完之后窗口内的总数。调用方须已确认 enabled。
func (b *Breaker) add(n int64) int {
	slot := b.now().UnixNano() / int64(b.bucketWidth)

	b.mu.Lock()
	defer b.mu.Unlock()
	i := int(slot % bucketCount)
	if b.slots[i] != slot {
		b.slots[i] = slot // 这个桶属于上一个窗口周期了：清掉再用
		b.counts[i] = 0
	}
	b.counts[i] += n
	return int(b.sumLocked())
}

// sumLocked 求和当前窗口内的计数；调用方须已持锁。
func (b *Breaker) sumLocked() int64 {
	slot := b.now().UnixNano() / int64(b.bucketWidth)
	var total int64
	for i := 0; i < bucketCount; i++ {
		// 只认 (slot-bucketCount, slot] 这个区间：早一个整窗的直接不算（窗口是左开右闭的）
		if s := b.slots[i]; s > slot-bucketCount && s <= slot {
			total += b.counts[i]
		}
	}
	return total
}
