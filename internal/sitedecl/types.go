package sitedecl

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/engine"
	"github.com/ydtg1993/papa/v4/internal/breaker"
	"github.com/ydtg1993/papa/v4/pkg/middleware/filedown"
	"github.com/ydtg1993/papa/v4/pkg/middleware/m3u8"
)

// SiteSpec 一个站点的声明（`Key` 必填）。多站就是多个 SiteSpec。
// **阶段名只在本站内唯一**：两个站可以各有自己的 `catalog`，互不干扰（同站内重名仍会报错并提示）。
type SiteSpec struct {
	// Key 站点键：**必填** —— 阶段键是 `site:stage`（阶段名只在本站内唯一），熔断 scope、
	// 日志与监控维度、落库的 `crawler_tasks.site` 也都用它。没有 Key 的声明会在启动时报错。
	Key string
	// BaseURL 站点根地址，供 handler 取用，也作为 OA 显式代理探测的 HEAD 目标。
	// 它不自动扩大网络或账号 origin 授权（`engine.Site(task.Site)` → BaseURL）。
	BaseURL string
	// Headers 本站的请求头（UA / Referer / Cookie / Accept-Language…）：这个站的任务抓任何页面
	// 都自动带上（静态抓取与浏览器渲染都认），**同键覆盖** `html.headers` / `browser.headers`；
	// 值为空串 = 删掉那个头。handler 里还能对单次请求 `papa.WithHeaders(ctx, …)` 再覆盖。
	// 多站同进程要"各站一套 UA/Cookie"就靠它 —— 也是把请求头这条"全局项"从 §4.1 那张表里拿掉的那一步。
	Headers map[string]string
	// RestrictedKeywords 本站自己的"受限页"文案，追加到 `htmlfetch.RestrictedReason` 的默认词表之后
	//（如"安全验证""请稍后再试"这类本站特有的话术）。命中后一般返回 `papa.RestrictedPageError`。
	RestrictedKeywords []string
	// StartPaused 启动时就把**整站**闸住：入口任务照投（它们先寄放在溢出表里、池子一个都不建），
	// 启动恢复与轮询队列捞回来的任务也一样不动 —— 等后台「站点控制」卡点「恢复整站」才灌回。
	//
	// 用途是"先把入口排好、再放开这一站"：多站项目里，某几站希望启动那一刻停在原地、由人来决定
	// 什么时候开始跑（也让上线后先看一眼队列、再放量的做法成立）。
	//
	// 它是**声明期的值**：每次启动都按它重新闸上；运行期在后台点的那次「恢复」不会被它改回去
	//（进程重启即失效，与全局的"暂停不跨重启"同一条规矩）。
	StartPaused bool
	// Breaker 本站的熔断阈值；nil = 用 crawler.breaker 那份默认。
	Breaker *BreakerSpec
	// M3U8 / FileDown 本站的下载器覆盖层：本站所有阶段默认用这一份（阶段还能再各自叠一层）。
	// nil = 不覆盖（用**基座**那份，即框架按 `DefaultConfig()` 备好的）。**字段全是指针，
	// 只写要改的那几项**：`M3U8: &papa.M3U8Spec{OutputDir: &dir, Output: &papa.OutputSegments}`。
	// 解析顺序一句话：**阶段 > 站点 > 基座**；fetcher 里要临时一份就 `M3U8ForTask(task).With(...)`，
	// 要完全独立的一份就自己 `m3u8.NewDownloader(&cfg)`。
	M3U8     *m3u8.Overlay
	FileDown *filedown.Overlay
	Stages   []StageSpec
}

// BreakerSpec 一个站点的熔断阈值（覆盖 crawler.breaker 的默认值）。
type BreakerSpec struct {
	// Policy overrides the global detection/recovery policy; nil inherits it.
	Policy *config.BreakerPolicy
	// Enabled 本站是否启用熔断。**注意它不看默认值** —— 写了这个字段就由它说了算，
	// 于是"全局开着、某个站关掉"和"全局关着、某个站单独开"都能表达。
	Enabled   bool
	Threshold int    // 窗口内终态失败数达到它即暂停；Enabled 时必须 > 0（0 = 用默认值）
	Window    string // 统计窗口，如 "5m"；空 = 用默认值
}

// StageSpec 一个阶段的完整声明。
type StageSpec struct {
	// 非空表示完整阶段可由只读探针验证；有写入/下载/派发的阶段不得声明。
	BreakerReadOnlyCoverage string
	RepeatLimit             int // 轮询队列上限：repeat >= 此值停止重投；0 不限，不影响恢复/错误队列。
	// Fetcher 阶段的实现。名字取自它自己的 `GetStage()`，不在这里写第二遍。
	// 实现了 engine.EntrySubmitter（`SubmitEntries`）的阶段，声明里写了 `Entry: true` 时启动那一趟投一轮。
	Fetcher     engine.Fetcher
	WorkerCount int // 物理 worker 上限；运行策略只能调低/恢复有效执行并发。
	QueueSize   int
	// Delay 任务间隔默认值："5m"（固定）或 "10s-30s"（随机区间）；空 = 不延迟。
	// runtime/OA 可覆盖为固定时长，清除覆盖后恢复本声明范围；与请求限速独立。
	Delay string
	Retry RetrySpec
	// M3U8 / FileDown 本阶段的下载器覆盖层（nil = 不覆盖）：比站点那层更细，赢了就用它。
	M3U8     *m3u8.Overlay
	FileDown *filedown.Overlay
	// Entry 本阶段**有入口任务**：启动那一趟就投（fetcher 实现了 `SubmitEntries` 才有得投）。
	// 零值 false = 这阶段没有入口这回事。
	//
	// 它**不是**"现在跑不跑"的开关 —— 那个在站点声明上（`SiteSpec.StartPaused`）：
	// 一个站想"启动先别动、等人来点"，要的是整站级闸门（入口任务照投、任务先寄放在溢出表），
	// 而不是"入口阶段不投、其它阶段照跑"。原先这两件事挤在一个三态枚举（auto/manual/off）里，
	// 声明 manual 之后启动恢复与轮询队列照样把这一站跑起来 —— 接口就是那么写的，别改回去。
	Entry bool
}

// RetrySpec 重试声明。
type RetrySpec struct {
	MaxAttempts int    // 含首次执行；<=0 用默认 3
	Backoff     string // 退避基数，如 "30s"；空 = 1s；指数值封顶 60s 后在半值至全值间抖动
}

// snapshot 把站点声明压成框架内部流通的快照（core.Site）：引擎侧、告警、入口回调都读它。
// **加字段就改这里**，别在应用层另抄一份 —— 抄的那份不会跟着声明走。
func (s SiteSpec) snapshot() core.Site {
	return core.Site{
		Key:                s.Key,
		BaseURL:            s.BaseURL,
		Headers:            maps.Clone(s.Headers),
		RestrictedKeywords: slices.Clone(s.RestrictedKeywords),
		StartPaused:        s.StartPaused,
	}
}

// defaultBreakerConfig 把全局那份默认阈值翻成引擎侧的 breaker.Config：给**没单独声明**
// 的站点当它自己的阈值用（每站一个计数器，见 planSite）。
func defaultBreakerConfig(def config.BreakerConfig) breaker.Config {
	return breaker.Config{
		Policy:    def.Policy,
		Enabled:   def.Enabled,
		Window:    def.WindowOrDefault(),
		Threshold: def.Threshold,
	}
}

// toConfig 把站点自己的熔断声明合到默认值上；未给的字段用默认值。
func (b BreakerSpec) toConfig(def config.BreakerConfig) (breaker.Config, error) {
	out := breaker.Config{
		Policy:    def.Policy,
		Enabled:   b.Enabled, // 不看默认：写了就由它说了算（见 BreakerSpec.Enabled）
		Threshold: def.Threshold,
		Window:    def.WindowOrDefault(),
	}
	if b.Policy != nil {
		out.Policy = *b.Policy
	}
	if err := breaker.ValidatePolicy(out.Policy); err != nil {
		return out, err
	}
	if b.Threshold != 0 {
		out.Threshold = b.Threshold
	}
	if strings.TrimSpace(b.Window) != "" {
		w, err := time.ParseDuration(b.Window)
		if err != nil {
			return out, fmt.Errorf("Window 无法解析: %w", err)
		}
		out.Window = w
	}
	return out, nil
}
