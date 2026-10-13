// Package breaker 只做一件事：**滑动窗口统计终态失败数，判断有没有达到阈值**。
//
// 它的职责边界是刻意收窄的（改过一次）：
//   - 它**不再持有暂停状态**，也不再是实现 `workerpool.Gate` 的那道闸门 ——
//     暂停的唯一定义在 `engine` 的暂停信号里（人工暂停与熔断触发写的是同一张表）。
//     它只把"达到阈值了、当时窗口内多少次失败"**返回**给引擎，由引擎去写信号、发告警。
//   - 所以它既**不 import workerpool**，也**不 import core**：不认识池子，也不认识监控 DTO。
//     （有一条扫源码守卫钉着"不 import core"这个契约。）
package breaker

import (
	"fmt"
	"sync"
	"time"
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
	Threshold int           // 启用时必填且必须 > 0，否则 New 直接 panic
}

// Breaker 熔断器：一个滑动窗口计数器。
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
}

// Validate 检查一份（默认值已填好的）配置是否合法：**New 会 panic 的那几条规则都在这儿**，
// 差别只是返回 error 而不是 panic —— 给"不用起服务先验一遍声明"那种场景用（`papa check`）。
// New 自己也调它，所以这条判据只有一份，不会两处漂。
func Validate(cfg Config) error {
	if cfg.Enabled && cfg.Threshold <= 0 {
		return fmt.Errorf("crawler.breaker.enabled=true 时 threshold 必须 > 0（没写与写 <= 0 都算没给），实得 %d；"+
			"要关掉熔断请把 enabled 改成 false", cfg.Threshold)
	}
	return nil
}

// New 构造熔断器。
//
// **启用熔断却不给正阈值是非法配置，直接 panic** —— 判据与 App.RegisterSites 对阶段
// 配置那几行同一路数。这里不兜默认值：threshold <= 0 时"多少条才算熔断"没有答案，
// 补一个数（原来补的是 50）只会让人以为开着、数的却是另一回事。
// 调用点在 engine.NewEngine，所以这是**启动时**失败，不是跑到某条任务才炸。
func New(cfg Config) *Breaker {
	if err := Validate(cfg); err != nil {
		panic(err)
	}
	if cfg.Window <= 0 {
		cfg.Window = defaultWindow
	}
	return &Breaker{
		enabled:   cfg.Enabled,
		threshold: cfg.Threshold,
		window:    cfg.Window,
		// 桶宽至少 1ns：add 与 sumLocked 都拿它做除数（now().UnixNano() / bucketWidth），
		// 而 window/bucketCount 是整数纳秒除法 —— window < bucketCount(60ns) 时结果是 0，整数除零。
		// 两个除零点的症状还不同，这是它难查的原因：add 走 RecordFailure，在 worker 的 handler
		// 调用栈里，被 workerpool 的 runHandler 接住 → 表现成"每个任务都失败"；sumLocked 走
		// Status()，在 HTTP handler 里，被 net/http 接住 → 后台熔断那块读不出来。都不崩进程。
		// 只兜"会崩"这一档，不动 window 的语义（配 10ns 还是 10ns，只是桶宽不再为 0）。
		bucketWidth: max(cfg.Window/bucketCount, time.Nanosecond),
		now:         time.Now,
	}
}

// Enabled 报告熔断是否启用。
func (b *Breaker) Enabled() bool { return b != nil && b.enabled }

// Window 生效的统计窗口（后台 payload 的 window 字段取它）。
func (b *Breaker) Window() time.Duration {
	if b == nil {
		return 0
	}
	return b.window
}

// Threshold 阈值（后台 payload 的 threshold 字段取它）。
func (b *Breaker) Threshold() int {
	if b == nil {
		return 0
	}
	return b.threshold
}

// RecordFailure 记一次**终态失败**（任务重试耗尽、或不可重试），
// 返回是否**达到阈值**与此刻窗口内的失败数。
//
// 为什么只数终态失败：一个任务重试 3 次会走 3 次 attempt，若按 attempt 计，几个烂 URL
// 就能把窗口灌满、熔断误触发 —— 那会把整个爬虫停掉，代价远大于放着几条烂任务不管。
//
// **达到阈值只是"该停了"，停不停、怎么写现场、要不要发告警都是引擎的事**
// （它要先把那条暂停写进暂停信号表，写成功了才发告警 —— 从而保住"一次触发只报一次"）。
// 所以这里会**反复**返回 tripped=true（每多一次失败就越过阈值一格），调用方去判重。
func (b *Breaker) RecordFailure() (tripped bool, failures int) {
	if b == nil || !b.enabled {
		return false, 0
	}
	n := b.add(1)
	return n >= b.threshold, n // threshold 必然 > 0（New 已校验）
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
