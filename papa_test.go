package papa_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v3"
)

// mockFetcher 用于验证 papa.Fetcher 接口别名能被子包外的业务代码实现。
type mockFetcher struct{}

func (mockFetcher) GetStage() string { return "mock" }

func (mockFetcher) FetchHandler(_ context.Context, _ *papa.Task, _ *papa.Engine) error {
	return nil
}

// TestFacadeAPI 验证门面包对外暴露的类型别名/函数签名都可用，
// 也就是验证「在自己的项目里 import github.com/ydtg1993/papa/v3 然后使用」这条链路是通的。
func TestFacadeAPI(t *testing.T) {
	// Fetcher 接口别名可用（值类型和指针类型都能实现）
	var _ papa.Fetcher = mockFetcher{}
	var _ papa.Fetcher = (*mockFetcher)(nil)

	// Task 结构及其方法可用
	task := &papa.Task{ID: 1, PID: 0, URL: "https://example.com", Stage: "mock", Repeatable: true}
	if got := task.Unique(); got != "mock|https://example.com" {
		t.Fatalf("unexpected unique key: %s", got)
	}

	// Config 类型可用
	var _ *papa.Config = (*papa.Config)(nil)

	// Option 与构造选项可用
	var _ papa.Option = papa.WithConfigPath("configs/config.yaml")
	var _ papa.Option = papa.WithModels(&struct{}{})

	// New 的签名：func(...papa.Option) (*papa.App, error)
	var _ func(...papa.Option) (*papa.App, error) = papa.New
}

// 与业务既有的写法一致：命中受限页后返回**不可重试**的 access_restricted 错误，
// 消息里带上命中的那句话（排查时"为什么判它是受限页"一眼可见）。
func TestRestrictedPageErrorIsNoRetry(t *testing.T) {
	err := papa.RestrictedPageError("catalog", "https://example.com/list", "captcha")
	if papa.Retryable(err) {
		t.Fatal("受限页不该重试（重试只会再撞一次同样的页面）")
	}
	if got := papa.ErrorKind(err); got != "access_restricted" {
		t.Fatalf("分类 = %q, want access_restricted", got)
	}
	if !strings.Contains(err.Error(), "captcha") || !strings.Contains(err.Error(), "catalog") {
		t.Fatalf("消息里应带阶段与命中的那句话：%v", err)
	}
	// reason 为空也照常工作（业务只想标一下"被拦了"，没做判据）
	if err := papa.RestrictedPageError("detail", "u", ""); err == nil || papa.Retryable(err) {
		t.Fatalf("reason 为空也该是不可重试的错误，实得 %v", err)
	}
}
