package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 辅助：写一份最小可加载的配置（框架必填的那几项给上），再拼上业务段。
func loadWithBusiness(t *testing.T, business string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, p, `
log:
  dir: ./logs
db:
  max_idle_conns: 10
  max_open_conns: 100
`+business)
	return Load(p)
}

// business 段是框架的「业务配置位」：写在这里的键不该被「未知配置键一律拒绝启动」误伤 ——
// 那条规则的本意是"拼错的键名不该静默失效"，而不是"业务不许有自己的配置"。
func TestLoadKeepsBusinessSection(t *testing.T) {
	cfg, err := loadWithBusiness(t, `
business:
  covers:
    dir: ./covers
    size: 300
  archive: true
`)
	if err != nil {
		t.Fatalf("business 段不该让加载失败：%v", err)
	}

	covers, ok := cfg.Business["covers"].(map[string]any)
	if !ok {
		t.Fatalf("business.covers 应原样透出为 map，实得 %#v", cfg.Business["covers"])
	}
	if covers["dir"] != "./covers" {
		t.Fatalf("covers.dir = %#v, want ./covers", covers["dir"])
	}
	if covers["size"] != 300 {
		t.Fatalf("covers.size = %#v（数字应保持数字）", covers["size"])
	}
	if cfg.Business["archive"] != true {
		t.Fatalf("标量也该在：%#v", cfg.Business["archive"])
	}
}

// 放开的只有 business 这一键：框架自己的键名拼错照旧拒绝启动（这一层是 panic，不是返回 error）。
// 两件事写在一个用例里，是为了让"边界在哪"一眼可见 —— 别把前者读成"校验层放宽了"。
func TestFrameworkTyposStillRejectedAlongsideBusiness(t *testing.T) {
	assertPanicContains(t, func() {
		_, _ = loadWithBusiness(t, `
business:
  covers:
    dir: ./covers
crawler:
  d: 1
`)
	}, "未知配置键", "crawler.d")
}

// BusinessSection 按 struct 取业务配置，走的是与框架配置同一套解码 hook。
func TestBusinessSectionDecodes(t *testing.T) {
	cfg := &Config{Business: map[string]any{
		"covers": map[string]any{
			"dir":      "./covers",
			"size":     300,
			"interval": "5m",
			"tags":     "a,b",
		},
	}}

	var out struct {
		Dir      string        `mapstructure:"dir"`
		Size     int           `mapstructure:"size"`
		Interval time.Duration `mapstructure:"interval"`
		Tags     []string      `mapstructure:"tags"`
	}
	if err := cfg.BusinessSection("covers", &out); err != nil {
		t.Fatalf("BusinessSection = %v", err)
	}
	if out.Dir != "./covers" || out.Size != 300 {
		t.Fatalf("解出来 = %+v", out)
	}
	if out.Interval != 5*time.Minute {
		t.Fatalf("Interval = %v, want 5m（时长写法应与框架配置一致）", out.Interval)
	}
	if len(out.Tags) != 2 || out.Tags[0] != "a" || out.Tags[1] != "b" {
		t.Fatalf("Tags = %#v, want [a b]", out.Tags)
	}
}

// 业务段内部的键名拼错要当场报错：这正是把"不校验"这个说法说清楚的地方 ——
// 框架不认业务键的语义，但"写错了当没写"这种事不该发生在业务段里。
func TestBusinessSectionRejectsUnknownKey(t *testing.T) {
	cfg := &Config{Business: map[string]any{"covers": map[string]any{"dr": "./covers"}}}

	var out struct {
		Dir string `mapstructure:"dir"`
	}
	err := cfg.BusinessSection("covers", &out)
	if err == nil {
		t.Fatal("拼错的键应报错")
	}
	if !strings.Contains(err.Error(), "dr") || !strings.Contains(err.Error(), "business.covers") {
		t.Fatalf("报错应点名拼错的键与段名：%v", err)
	}
}

// 段没配就取：直接说清楚，不给一个零值 struct 让业务在运行期慢慢发现。
func TestBusinessSectionMissingKey(t *testing.T) {
	cfg := &Config{Business: map[string]any{"covers": map[string]any{}}}

	var out struct {
		Dir string `mapstructure:"dir"`
	}
	err := cfg.BusinessSection("archive", &out)
	if err == nil || !strings.Contains(err.Error(), "business.archive") {
		t.Fatalf("段不存在应报错并点名：%v", err)
	}
}
