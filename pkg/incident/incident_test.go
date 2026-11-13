package incident

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestGroupingPreservesCodesAndRedactsEvidence(t *testing.T) {
	now := time.Now()
	c := Capture{ID: "test", At: now, From: now.Add(-time.Minute), Failures: 3}
	for i, msg := range []string{"HTTP 429 https://example.test/a?token=hidden-one task_id=1", "HTTP 429 https://example.test/b?token=hidden-two task_id=2", "HTTP 403 https://example.test/c"} {
		c.Samples = append(c.Samples, Sample{At: now, Stage: "detail", Kind: "retryable", TaskID: int64(i + 1), Error: msg, URL: "https://user:password-value@example.test/a?token=hidden-url"})
	}
	r := Analyze(c)
	if !r.Complete || len(r.Groups) != 2 || r.Groups[0].Count != 2 || r.Groups[0].Tasks != 2 || r.Groups[0].Category != "HTTP 429" {
		t.Fatalf("wrong groups: %+v", r)
	}
	raw, _ := json.Marshal(r)
	for _, secret := range []string{"hidden-one", "hidden-two", "hidden-url", "password-value"} {
		if strings.Contains(string(raw)+Text(r), secret) {
			t.Fatalf("secret leaked: %s", secret)
		}
	}
	if !strings.Contains(c.Samples[0].Error, "hidden-one") {
		t.Fatal("analysis mutated source")
	}
	if !strings.Contains(r.Groups[1].Hypothesis, "不能直接认定反爬") {
		t.Fatal("403 must not claim antibot as fact")
	}
}

func TestWindowBoundsAndLimits(t *testing.T) {
	now := time.Now()
	c := Capture{At: now, From: now.Add(-time.Minute), Failures: MaxSamples + 1}
	c.Samples = append(c.Samples, Sample{At: now.Add(-2 * time.Minute)}, Sample{At: now.Add(time.Second)})
	for i := 0; i < MaxSamples+1; i++ {
		c.Samples = append(c.Samples, Sample{At: now, Error: "timeout", TaskID: 1})
	}
	r := Analyze(c)
	if r.Complete || !r.Limited || r.Observed != MaxSamples || r.Groups[0].Tasks != 1 {
		t.Fatalf("bounds: %+v", r)
	}
	c.Samples = []Sample{{At: now, Error: strings.Repeat("超", 1024)}}
	c.Failures = 1
	if r := Analyze(c); r.Complete || !r.Limited {
		t.Fatal("truncated text must be disclosed")
	}
	if r := Analyze(Capture{}); r.Complete || r.Summary != "没有可用失败样本，无法归因" {
		t.Fatal("empty evidence must be unknown")
	}
}

func TestClassificationDoesNotOverclaim(t *testing.T) {
	for _, tc := range []struct{ message, category string }{
		{"Failed to get the debug url", "浏览器启动"}, {"ProcessSingleton Lock file can not be created", "浏览器目录锁"},
		{"SQLSTATE 23000 duplicate entry", "数据库"}, {"lookup host: no such host", "DNS 解析"},
		{"context deadline exceeded", "超时"}, {"HTTP 500", "HTTP 500"}, {"unfamiliar failure", "原因待确认"},
	} {
		if category, _, verify := Classify("retryable", tc.message); category != tc.category || verify == "" {
			t.Fatalf("%s: %s", tc.message, category)
		}
	}
	if Template("SQLSTATE 1062") == Template("SQLSTATE 1213") {
		t.Fatal("database error codes merged")
	}
}

func TestOversizedCredentialIsNotPartiallyExposed(t *testing.T) {
	secret := strings.Repeat("sensitive", 1000)
	s := Clean(Sample{URL: "https://user:" + secret + "@example.test/", Error: "request https://user:" + secret + "@example.test/ failed"})
	if !s.Truncated || strings.Contains(s.URL+s.Error, "sensitive") {
		t.Fatal("oversized credential prefix leaked")
	}
}

func TestBrowserLogVolatilePrefixesMergeWithoutLosingErrorCodes(t *testing.T) {
	a := "[1200:400:1110/210000.120:ERROR:process_singleton_win.cc(465)] Lock file can not be created! Error code: 5"
	b := "[2300:500:1110/210001.240:ERROR:process_singleton_win.cc(465)] Lock file can not be created! Error code: 5"
	if Template(a) != Template(b) {
		t.Fatal("volatile browser process/time split same failure")
	}
	if Template(a) == Template(strings.ReplaceAll(b, "Error code: 5", "Error code: 32")) {
		t.Fatal("OS error codes were merged")
	}
}
