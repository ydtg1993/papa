package config

import (
	"strings"
	"testing"
)

// 热更那条入口**绕过 config.Load**：LoadRuntime 只 yaml.Unmarshal、
// ApplyRuntimeConfig 只 Merge + Store，两边都没有校验。所以必须单独校验 ——
// 否则 `PUT /api/config {"html":{"max_body_size":0}}` 能在不重启的情况下
// 把每一次抓取都打坏（`LimitReader(body, 0+1)`），而配置文件的校验看不见它。
//
// 形态上也不同：这个返回 error（HTTP 那边翻成 400），不是 panic ——
// 这条路上有人在等回应，报 400 比把服务打死有用。
func TestValidateRuntime(t *testing.T) {
	i64 := func(v int64) *int64 { return &v }
	i := func(v int) *int { return &v }

	for _, c := range []struct {
		name string
		rt   *RuntimeConfig
		key  string // 空 = 应当通过
	}{
		{"nil 覆盖层", nil, ""},
		{"空覆盖层", &RuntimeConfig{}, ""},
		{"max_body_size 合法（模板值）", &RuntimeConfig{HTML: RuntimeHTMLConfig{MaxBodySize: i64(10485760)}}, ""},
		{"max_body_size 下界", &RuntimeConfig{HTML: RuntimeHTMLConfig{MaxBodySize: i64(102400)}}, ""},
		{"max_body_size 上界", &RuntimeConfig{HTML: RuntimeHTMLConfig{MaxBodySize: i64(67108864)}}, ""},
		{"max_body_size 写 0", &RuntimeConfig{HTML: RuntimeHTMLConfig{MaxBodySize: i64(0)}}, "html.max_body_size"},
		{"max_body_size 写 10", &RuntimeConfig{HTML: RuntimeHTMLConfig{MaxBodySize: i64(10)}}, "html.max_body_size"},
		{"max_body_size 超上界", &RuntimeConfig{HTML: RuntimeHTMLConfig{MaxBodySize: i64(67108865)}}, "html.max_body_size"},
		{"worker_count 负数", &RuntimeConfig{RepeatQueue: RuntimeRepeatQueueConfig{WorkerCount: i(-1)}}, "repeat_queue.worker_count"},
		{"max_retry 负数", &RuntimeConfig{ErrorQueue: RuntimeErrorQueueConfig{MaxRetry: i(-1)}}, "error_queue.max_retry"},
		{"batch_size 负数", &RuntimeConfig{RecoverQueue: RuntimeRecoverQueueConfig{BatchSize: i(-1)}}, "recover_queue.batch_size"},
		{"worker_count 写 0（= 用默认，放行）", &RuntimeConfig{ErrorQueue: RuntimeErrorQueueConfig{WorkerCount: i(0)}}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateRuntime(c.rt)
			if c.key == "" {
				if err != nil {
					t.Fatalf("应当通过，实得 %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("应当被拦：%s", c.key)
			}
			if !strings.Contains(err.Error(), c.key) {
				t.Fatalf("错误信息应点名 %q，实得 %v", c.key, err)
			}
		})
	}
}
