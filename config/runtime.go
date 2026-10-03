package config

import (
	"encoding/json"
	"os"
	"time"

	yaml "go.yaml.in/yaml/v3"
)

// Duration 可空时长，YAML 中表示为 "10s" 字符串。
type Duration struct {
	time.Duration
}

// UnmarshalYAML 解析 "10s" 字符串为时长。
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// MarshalYAML 序列化为 "10s" 字符串。
func (d Duration) MarshalYAML() (any, error) {
	return d.Duration.String(), nil
}

// UnmarshalJSON 解析 "10s" 字符串为时长（供 /api/config JSON 请求体）。
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// MarshalJSON 序列化为 "10s" 字符串。
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.Duration.String())
}

// RuntimeConfig 运行期动态配置覆盖层（delta）。
// 指针字段非 nil 表示该字段被 OA 后台改过；生效值 = 覆盖层 ?? 基础配置。
// 关停时把非 nil 字段落盘到 runtime.yaml，重启后叠加回基础配置。
type RuntimeConfig struct {
	Browser      RuntimeBrowserConfig      `yaml:"browser,omitempty" json:"browser,omitzero"`
	HTML         RuntimeHTMLConfig         `yaml:"html,omitempty" json:"html,omitzero"`
	ErrorQueue   RuntimeErrorQueueConfig   `yaml:"error_queue,omitempty" json:"error_queue,omitzero"`
	RecoverQueue RuntimeRecoverQueueConfig `yaml:"recover_queue,omitempty" json:"recover_queue,omitzero"`
	RepeatQueue  RuntimeRepeatQueueConfig  `yaml:"repeat_queue,omitempty" json:"repeat_queue,omitzero"`
}

// RuntimeBrowserConfig 浏览器池运行期覆盖项。池大小不在此列：它是启动时读的上限，改需重启。
type RuntimeBrowserConfig struct {
	MaxIdleTime *Duration         `yaml:"max_idle_time,omitempty" json:"max_idle_time,omitempty"`
	Headers     map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
}

// RuntimeHTMLConfig 静态 HTML 客户端运行期覆盖项。
type RuntimeHTMLConfig struct {
	Timeout     *Duration         `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	MaxBodySize *int64            `yaml:"max_body_size,omitempty" json:"max_body_size,omitempty"`
	Headers     map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
}

// RuntimeErrorQueueConfig 错误队列运行期覆盖项。
type RuntimeErrorQueueConfig struct {
	Enabled     *bool     `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	WorkerCount *int      `yaml:"worker_count,omitempty" json:"worker_count,omitempty"`
	MaxRetry    *int      `yaml:"max_retry,omitempty" json:"max_retry,omitempty"`
	Interval    *Duration `yaml:"interval,omitempty" json:"interval,omitempty"`
	BatchSize   *int      `yaml:"batch_size,omitempty" json:"batch_size,omitempty"`
}

// RuntimeRecoverQueueConfig 中断恢复队列运行期覆盖项。
type RuntimeRecoverQueueConfig struct {
	Enabled     *bool     `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	WorkerCount *int      `yaml:"worker_count,omitempty" json:"worker_count,omitempty"`
	Interval    *Duration `yaml:"interval,omitempty" json:"interval,omitempty"`
	Timeout     *Duration `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	BatchSize   *int      `yaml:"batch_size,omitempty" json:"batch_size,omitempty"`
}

// RuntimeRepeatQueueConfig 周期轮询队列运行期覆盖项。
type RuntimeRepeatQueueConfig struct {
	Enabled     *bool     `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	WorkerCount *int      `yaml:"worker_count,omitempty" json:"worker_count,omitempty"`
	Interval    *Duration `yaml:"interval,omitempty" json:"interval,omitempty"`
	BatchSize   *int      `yaml:"batch_size,omitempty" json:"batch_size,omitempty"`
}

// Merge 把 next 里**显式给出**的字段并进当前覆盖层，返回新的覆盖层。
// 不修改接收者也不修改 next；返回的结构与入参共享那几个映射，按只读对待（与本包既有约定一致）。
//
// 为什么需要它：`PUT /api/config` 原来是直接整体替换（`Store(rt)`）—— 只提交一个
// `html.timeout` 就会把之前设的 `browser.headers`、各队列的 `interval` 全清掉，
// 而操作人从响应上完全看不出来。PUT 的语义应当是「改我提到的字段」。
//
// 「显式给出」怎么判断：**标量看指针**（nil = 没提），**映射看 nil 与空 map**（nil = 没提，
// `{}` = 显式把这组覆盖清空）。
//
// 注意标量没有「清除」这一说 —— JSON 里 null 与字段缺失解出来都是 nil，分不开。
// 想让它回落成基础配置的值，直接把它设成那个值就行：覆盖层里会多留一条，行为一致。
func (rt *RuntimeConfig) Merge(next *RuntimeConfig) *RuntimeConfig {
	if rt == nil {
		rt = &RuntimeConfig{}
	}
	if next == nil {
		next = &RuntimeConfig{}
	}
	out := *rt

	if next.Browser.MaxIdleTime != nil {
		out.Browser.MaxIdleTime = next.Browser.MaxIdleTime
	}
	if next.Browser.Headers != nil {
		out.Browser.Headers = next.Browser.Headers
	}
	if next.HTML.Timeout != nil {
		out.HTML.Timeout = next.HTML.Timeout
	}
	if next.HTML.MaxBodySize != nil {
		out.HTML.MaxBodySize = next.HTML.MaxBodySize
	}
	if next.HTML.Headers != nil {
		out.HTML.Headers = next.HTML.Headers
	}

	if next.ErrorQueue.Enabled != nil {
		out.ErrorQueue.Enabled = next.ErrorQueue.Enabled
	}
	if next.ErrorQueue.WorkerCount != nil {
		out.ErrorQueue.WorkerCount = next.ErrorQueue.WorkerCount
	}
	if next.ErrorQueue.MaxRetry != nil {
		out.ErrorQueue.MaxRetry = next.ErrorQueue.MaxRetry
	}
	if next.ErrorQueue.Interval != nil {
		out.ErrorQueue.Interval = next.ErrorQueue.Interval
	}
	if next.ErrorQueue.BatchSize != nil {
		out.ErrorQueue.BatchSize = next.ErrorQueue.BatchSize
	}

	if next.RecoverQueue.Enabled != nil {
		out.RecoverQueue.Enabled = next.RecoverQueue.Enabled
	}
	if next.RecoverQueue.WorkerCount != nil {
		out.RecoverQueue.WorkerCount = next.RecoverQueue.WorkerCount
	}
	if next.RecoverQueue.Interval != nil {
		out.RecoverQueue.Interval = next.RecoverQueue.Interval
	}
	if next.RecoverQueue.Timeout != nil {
		out.RecoverQueue.Timeout = next.RecoverQueue.Timeout
	}
	if next.RecoverQueue.BatchSize != nil {
		out.RecoverQueue.BatchSize = next.RecoverQueue.BatchSize
	}

	if next.RepeatQueue.Enabled != nil {
		out.RepeatQueue.Enabled = next.RepeatQueue.Enabled
	}
	if next.RepeatQueue.WorkerCount != nil {
		out.RepeatQueue.WorkerCount = next.RepeatQueue.WorkerCount
	}
	if next.RepeatQueue.Interval != nil {
		out.RepeatQueue.Interval = next.RepeatQueue.Interval
	}
	if next.RepeatQueue.BatchSize != nil {
		out.RepeatQueue.BatchSize = next.RepeatQueue.BatchSize
	}
	return &out
}

// IsZero 报告是否有任何覆盖；供 SaveRuntime 判断是否落盘及 yaml omitempty 使用。
func (rt *RuntimeConfig) IsZero() bool {
	return rt == nil || (rt.Browser.IsZero() && rt.HTML.IsZero() &&
		rt.ErrorQueue.IsZero() && rt.RecoverQueue.IsZero() && rt.RepeatQueue.IsZero())
}

// IsZero 报告浏览器覆盖项是否为空。
func (b RuntimeBrowserConfig) IsZero() bool {
	return b.MaxIdleTime == nil && len(b.Headers) == 0
}

// IsZero 报告 HTML 覆盖项是否为空。
func (h RuntimeHTMLConfig) IsZero() bool {
	return h.Timeout == nil && h.MaxBodySize == nil && len(h.Headers) == 0
}

// IsZero 报告错误队列覆盖项是否为空。
func (q RuntimeErrorQueueConfig) IsZero() bool {
	return q.Enabled == nil && q.WorkerCount == nil && q.MaxRetry == nil && q.Interval == nil && q.BatchSize == nil
}

// IsZero 报告中断恢复队列覆盖项是否为空。
func (q RuntimeRecoverQueueConfig) IsZero() bool {
	return q.Enabled == nil && q.WorkerCount == nil && q.Interval == nil && q.Timeout == nil && q.BatchSize == nil
}

// IsZero 报告周期轮询队列覆盖项是否为空。
func (q RuntimeRepeatQueueConfig) IsZero() bool {
	return q.Enabled == nil && q.WorkerCount == nil && q.Interval == nil && q.BatchSize == nil
}

// LoadRuntime 读取运行期覆盖文件；文件不存在返回空覆盖（无错误）。
func LoadRuntime(path string) (*RuntimeConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &RuntimeConfig{}, nil
		}
		return nil, err
	}
	var rt RuntimeConfig
	if err := yaml.Unmarshal(data, &rt); err != nil {
		return nil, err
	}
	return &rt, nil
}

// SaveRuntime 把覆盖层落盘；无任何覆盖时删除文件。
func SaveRuntime(path string, rt *RuntimeConfig) error {
	if rt.IsZero() {
		_ = os.Remove(path)
		return nil
	}
	data, err := yaml.Marshal(rt)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
