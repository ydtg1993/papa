package papa_test

import (
	"context"
	"testing"

	"github.com/ydtg1993/papa"
)

// mockFetcher 用于验证 papa.Fetcher 接口别名能被子包外的业务代码实现。
type mockFetcher struct{}

func (mockFetcher) GetStage() string { return "mock" }

func (mockFetcher) FetchHandler(_ context.Context, _ *papa.Task, _ *papa.Engine) error {
	return nil
}

// TestFacadeAPI 验证门面包对外暴露的类型别名/函数签名都可用，
// 也就是验证「在自己的项目里 import github.com/ydtg1993/papa 然后使用」这条链路是通的。
func TestFacadeAPI(t *testing.T) {
	// Fetcher 接口别名可用（值类型和指针类型都能实现）
	var _ papa.Fetcher = mockFetcher{}
	var _ papa.Fetcher = (*mockFetcher)(nil)

	// Task 结构及其方法可用
	task := &papa.Task{ID: 1, PID: 0, URL: "https://example.com", Stage: "mock", Repeatable: true}
	if got := task.Unique(); got != "mock|https://example.com" {
		t.Fatalf("unexpected unique key: %s", got)
	}

	// Config / CrawlerTask 类型可用
	var _ *papa.Config = (*papa.Config)(nil)
	var _ *papa.CrawlerTask = (*papa.CrawlerTask)(nil)

	// Option 与构造选项可用
	var _ papa.Option = papa.WithConfigPath("configs/config.yaml")
	var _ papa.Option = papa.WithModels(&papa.CrawlerTask{})

	// New 的签名：func(...papa.Option) (*papa.App, error)
	var _ func(...papa.Option) (*papa.App, error) = papa.New
}
