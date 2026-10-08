package engine

import (
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v2/config"
)

// ApplyRuntimeConfig 是配置层的"腰"：HTTP 那条路（PUT /api/config）在 handler 里先校验一次，
// 但程序化调用（业务自己调 app.Engine.ApplyRuntimeConfig）绕不过这里。
// 两处都调同一个 config.ValidateRuntime —— 规则只有一份，只是两个入口各拦一道。
func TestApplyRuntimeConfigRejectsOutOfRange(t *testing.T) {
	f := newFakeTaskDB()
	e, _ := urgentEngine(t, f)

	zeroBody := int64(0)
	negWorker := -1
	for _, c := range []struct {
		name string
		rt   *config.RuntimeConfig
		key  string
	}{
		{"max_body_size 写 0", &config.RuntimeConfig{
			HTML: config.RuntimeHTMLConfig{MaxBodySize: &zeroBody},
		}, "html.max_body_size"},
		{"worker_count 负数", &config.RuntimeConfig{
			ErrorQueue: config.RuntimeErrorQueueConfig{WorkerCount: &negWorker},
		}, "error_queue.worker_count"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := e.ApplyRuntimeConfig(c.rt)
			if err == nil {
				t.Fatalf("越界的运行期覆盖应当报错（%s）", c.key)
			}
			if !strings.Contains(err.Error(), c.key) {
				t.Fatalf("错误信息应点名 %q，实得 %v", c.key, err)
			}
		})
	}

	// 合法的覆盖照旧生效
	okBody := int64(10485760)
	if err := e.ApplyRuntimeConfig(&config.RuntimeConfig{
		HTML: config.RuntimeHTMLConfig{MaxBodySize: &okBody},
	}); err != nil {
		t.Fatalf("合法覆盖不该被拦：%v", err)
	}
}
