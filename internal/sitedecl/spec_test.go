package sitedecl

import (
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/core"
)

func TestStageBreakerReadOnlyCoverageDeclaration(t *testing.T) {
	for _, coverage := range []string{"", "完整读取与解析；无副作用", "  ", strings.Repeat("x", 1025)} {
		plan, err := planStage(core.Site{Key: "s"}, StageSpec{Fetcher: namedFetcher{"detail"}, WorkerCount: 1, QueueSize: 8, BreakerReadOnlyCoverage: coverage}, map[string]string{})
		invalid := coverage == "  " || len(coverage) > 1024
		if (err != nil) != invalid {
			t.Fatalf("validation mismatch length=%d err=%v", len(coverage), err)
		}
		if !invalid && plan.Config.BreakerReadOnlyCoverage != coverage {
			t.Fatal("coverage not forwarded")
		}
	}
}

func TestStageRepeatLimitDeclaration(t *testing.T) {
	for _, limit := range []int{0, 30, -1} {
		plan, err := planStage(core.Site{Key: "s"}, StageSpec{Fetcher: namedFetcher{"detail"}, WorkerCount: 1, QueueSize: 8, RepeatLimit: limit}, map[string]string{})
		if limit < 0 {
			if err == nil {
				t.Fatal("negative RepeatLimit accepted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if plan.Config.RepeatLimit != limit {
			t.Fatalf("limit not forwarded: %+v", plan.Config)
		}
	}
}

// BreakerSpec 合到 crawler.breaker 上的语义：**Enabled 不看默认值**（写了就由它说了算），
// 阈值/窗口没给才落回默认。
func TestBreakerSpecToConfig(t *testing.T) {
	def := config.BreakerConfig{Enabled: true, Window: 5 * time.Minute, Threshold: 50}

	// 全不给：阈值窗口用默认，但 Enabled 是 false（站点自己说了算 —— 于是"全局开着、某站关掉"能表达）
	got, err := BreakerSpec{}.toConfig(def)
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled || got.Threshold != 50 || got.Window != 5*time.Minute {
		t.Fatalf("空 BreakerSpec 应当只继承阈值与窗口：%+v", got)
	}

	// 给了阈值与窗口：覆盖
	got, err = BreakerSpec{Enabled: true, Threshold: 7, Window: "2m"}.toConfig(def)
	if err != nil || !got.Enabled || got.Threshold != 7 || got.Window != 2*time.Minute {
		t.Fatalf("覆盖没生效：%+v %v", got, err)
	}

	// 默认里 Window 是零值：走 WindowOrDefault（否则 time.NewTicker(0) 会在闸门里 panic）
	got, err = BreakerSpec{Enabled: true}.toConfig(config.BreakerConfig{Threshold: 3})
	if err != nil || got.Window <= 0 {
		t.Fatalf("窗口应当有默认值：%+v %v", got, err)
	}
	if got.Threshold != 3 {
		t.Fatalf("阈值应当继承默认：%+v", got)
	}

	// 窗口写错：报错（不是在运行期崩）
	if _, err := (BreakerSpec{Enabled: true, Window: "两分钟"}).toConfig(def); err == nil {
		t.Fatal("窗口解析失败应当报错")
	}
}

func TestBreakerSpecPolicyInheritanceAndReplacement(t *testing.T) {
	def := config.BreakerConfig{Enabled: true, Threshold: 20, Policy: config.BreakerPolicy{FastThreshold: 7, ShortWindow: 12 * time.Second}}
	got, err := (BreakerSpec{Enabled: true}).toConfig(def)
	if err != nil || got.Policy.FastThreshold != 7 || got.Policy.ShortWindow != 12*time.Second {
		t.Fatalf("inherit: %+v %v", got, err)
	}
	p := config.BreakerPolicy{FastThreshold: 3}
	got, err = (BreakerSpec{Enabled: true, Policy: &p}).toConfig(def)
	if err != nil || got.Policy.FastThreshold != 3 || got.Policy.ShortWindow != 0 {
		t.Fatalf("replace: %+v %v", got, err)
	}
	p.FastThreshold = -1
	if _, err := (BreakerSpec{Enabled: true, Policy: &p}).toConfig(def); err == nil {
		t.Fatal("invalid site policy accepted")
	}
}
