package breaker

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTest 造一个时钟可控的熔断器 —— 窗口滑动全靠时间，用真 sleep 测会又慢又脆。
func newTest(cfg Config) (b *Breaker, advance func(time.Duration)) {
	b = New(cfg)
	var mu sync.Mutex
	now := time.Unix(1700000000, 0)
	b.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	return b, func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}
}

// 达到阈值就报 tripped，并把当时的窗口失败数一起给出 —— **停不停、怎么写现场、发不发告警
// 都是引擎的事**（见 engine/pause.go）：本包只回答"到没到阈"。
func TestTripsAtThreshold(t *testing.T) {
	b, _ := newTest(Config{Enabled: true, Window: time.Minute, Threshold: 3})

	for i := 1; i <= 2; i++ {
		tripped, n := b.RecordFailure()
		if tripped {
			t.Fatalf("第 %d 次失败就报触发了，阈值是 3", i)
		}
		if n != i {
			t.Fatalf("第 %d 次失败后窗口内应当是 %d，实得 %d", i, i, n)
		}
	}
	tripped, failures := b.RecordFailure()
	if !tripped {
		t.Fatal("第 3 次失败应当报触发")
	}
	if failures != 3 {
		t.Fatalf("触发时的失败数 = %d, want 3（引擎把它写进暂停现场，后台要显示）", failures)
	}
	if got := b.Sum(); got != 3 {
		t.Fatalf("Sum = %d, want 3", got)
	}
	// 配置也要能读出来：后台 payload 的 window / threshold 两列取自这里
	if b.Threshold() != 3 || b.Window() != time.Minute {
		t.Fatalf("config 读不对：threshold=%d window=%v", b.Threshold(), b.Window())
	}
}

// 越过阈值之后**每次都还报 tripped**（因为计数还在涨）—— 判重是调用方的事：
// 引擎靠"写暂停信号成功没成功"来保证"一次触发只报一次告警"（已经停着时 setPause 返回 false）。
// 这条把那个分工钉住：熔断器不再自己记"报过了"。
func TestReportsTrippedEveryTimePastThreshold(t *testing.T) {
	b, _ := newTest(Config{Enabled: true, Window: time.Minute, Threshold: 2})
	for i := 1; i <= 5; i++ {
		tripped, n := b.RecordFailure()
		if want := i >= 2; tripped != want {
			t.Fatalf("第 %d 次失败：tripped = %v, want %v", i, tripped, want)
		}
		if n != i {
			t.Fatalf("第 %d 次失败：窗口内 %d", i, n)
		}
	}
}

// 窗口滑走之后老失败不再计入 —— 否则「几小时前失败过 50 次」会让熔断永远触发。
func TestFailuresFallOutOfWindow(t *testing.T) {
	b, advance := newTest(Config{Enabled: true, Window: time.Minute, Threshold: 3})

	b.RecordFailure()
	b.RecordFailure()
	advance(2 * time.Minute)

	if got := b.Sum(); got != 0 {
		t.Fatalf("窗口滑走后 Sum = %d, want 0", got)
	}
	if tripped, _ := b.RecordFailure(); tripped {
		t.Fatal("老失败不该把新失败顶到阈值")
	}
	if got := b.Sum(); got != 1 {
		t.Fatalf("滑走之后应当只剩新的一条：%d", got)
	}
}

// 未启用时任何调用都不改变计数：enabled=false 就是"没有自动触发这回事"。
func TestDisabledNeverTrips(t *testing.T) {
	b, _ := newTest(Config{Enabled: false, Window: time.Minute, Threshold: 1})

	for i := 0; i < 100; i++ {
		if tripped, n := b.RecordFailure(); tripped || n != 0 {
			t.Fatalf("未启用不该触发也不该计数：tripped=%v n=%d", tripped, n)
		}
	}
	if got := b.Sum(); got != 0 {
		t.Fatalf("未启用不该计数，实得 %d", got)
	}
	if b.Enabled() {
		t.Fatal("Enabled 应当报告未启用")
	}
}

// nil 接收者（引擎没配熔断、或测试里直接构造 Engine）任何方法都不能 panic ——
// 这样调用处（notifyFailure）就不必到处判空。
func TestNilReceiverIsSafe(t *testing.T) {
	var b *Breaker

	if b.Enabled() || b.Sum() != 0 || b.Window() != 0 || b.Threshold() != 0 {
		t.Fatal("nil 熔断器不该报告任何启用/计数/配置")
	}
	if tripped, n := b.RecordFailure(); tripped || n != 0 {
		t.Fatalf("nil 熔断器不该计数：tripped=%v n=%d", tripped, n)
	}
}

// 窗口小于 bucketCount（60ns）时，window/bucketCount 的整数纳秒除法算出 0，
// 而 add / sumLocked 都拿 bucketWidth 当除数 → 整数除零 panic。
//
// 两个除零点的症状不同，这是它难查的原因：add 走 RecordFailure，在 worker 的 handler
// 调用栈里、被 workerpool 的 runHandler 接住 → 表现成"每个任务都失败"；sumLocked 走
// Sum()，而 Sum 现在是**后台合成视图**（以及引擎判阈值）读的那条路，在 HTTP handler 里、
// 被 net/http 接住 → 后台熔断那块读不出来。都不崩进程。
//
// 所以这条用例不只要"不 panic"，还要求阈值判定与窗口计数照常工作 —— 兜底把桶宽压成
// 1ns 之后，窗口语义随之变成"至少 60ns"，那是这一档的既定代价（与 m3u8/filedown 的
// max(MaxConcurrent, 1) 同一路数：只补"会崩"的那一类，不动字段本身的语义）。
func TestTinyWindowDoesNotDivideByZero(t *testing.T) {
	// 1ns / 10ns / 59ns 都会算出 0；60ns 是边界（60/60 = 1ns），用来钉住兜底没有误伤它的邻域
	for _, w := range []time.Duration{
		time.Nanosecond,
		10 * time.Nanosecond,
		59 * time.Nanosecond,
		60 * time.Nanosecond,
	} {
		t.Run(w.String(), func(t *testing.T) {
			// 冻结时钟：bucketWidth 只有 1ns 时，真实时钟在两行之间就能滑走一整个窗口，
			// 用 newTest 的注入时钟才能让断言稳定（这里要测的是除零，不是滑动）。
			b, _ := newTest(Config{Enabled: true, Window: w, Threshold: 1})

			if tripped, n := b.RecordFailure(); !tripped || n != 1 { // ← 除零点 ①：add
				t.Fatalf("window=%v 阈值=1，第一条失败就该触发，实得 tripped=%v n=%d", w, tripped, n)
			}
			if got := b.Sum(); got != 1 { // ← 除零点 ②：sumLocked
				t.Fatalf("window=%v Sum() = %d, want 1", w, got)
			}
			if b.Window() != w || b.Threshold() != 1 {
				t.Fatalf("window=%v 配置应原样带出：%v / %d", w, b.Window(), b.Threshold())
			}
		})
	}
}

// 启用熔断却没给正阈值 = 非法配置，构造时直接 panic（调用点在 engine.NewEngine，所以是启动时失败）。
//
// 这里**不兜默认值**：threshold <= 0 时"多少条才算熔断"没有答案，补一个数只会让人以为
// 开着、数的却是另一回事 —— 而这种"看着开着、其实没开（或开在别的数上）"从配置和后台
// 页面都看不出来。宁可启动就炸，让人当场把话说明白。
func TestEnabledWithoutThresholdPanics(t *testing.T) {
	for _, c := range []struct {
		what string
		cfg  Config
	}{
		{"写 0", Config{Enabled: true, Threshold: 0}},
		{"写负数", Config{Enabled: true, Threshold: -1}},
		{"只写 enabled、阈值留空", Config{Enabled: true}},
	} {
		t.Run(c.what, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("%+v 应当 panic", c.cfg)
				}
				// 报错要能直接定位到配置键，否则用户不知道去哪改
				if msg := fmt.Sprint(r); !strings.Contains(msg, "crawler.breaker") || !strings.Contains(msg, "threshold") {
					t.Fatalf("panic 信息应点名配置键与字段，实得 %q", msg)
				}
			}()
			New(c.cfg)
		})
	}

	// 关着的时候阈值随便写：整段不写就是 enabled=false + threshold=0，这是**默认状态**，
	// 不能因为它 panic。关掉的是"自动计数 + 触发"，与"能不能手动暂停"无关 ——
	// 后者归暂停信号（见 engine/pause.go），与 enabled 是两件事。
	for _, cfg := range []Config{
		{},
		{Threshold: 0},
		{Threshold: -1},
		{Enabled: false, Threshold: 0},
	} {
		b := New(cfg)
		if b.Enabled() || b.Sum() != 0 {
			t.Fatalf("%+v 关着时不该有任何计数生效", cfg)
		}
		if tripped, _ := b.RecordFailure(); tripped {
			t.Fatalf("%+v 关着时不该报触发", cfg)
		}
	}
}
