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

// RuntimeBrowserConfig 浏览器池运行期覆盖项。
type RuntimeBrowserConfig struct {
	PoolSize    *int              `yaml:"pool_size,omitempty" json:"pool_size,omitempty"`
	DirectSize  *int              `yaml:"direct_pool_size,omitempty" json:"direct_pool_size,omitempty"`
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

// IsZero 报告是否有任何覆盖；供 SaveRuntime 判断是否落盘及 yaml omitempty 使用。
func (rt *RuntimeConfig) IsZero() bool {
	return rt == nil || (rt.Browser.IsZero() && rt.HTML.IsZero() &&
		rt.ErrorQueue.IsZero() && rt.RecoverQueue.IsZero() && rt.RepeatQueue.IsZero())
}

// IsZero 报告浏览器覆盖项是否为空。
func (b RuntimeBrowserConfig) IsZero() bool {
	return b.PoolSize == nil && b.DirectSize == nil && b.MaxIdleTime == nil && len(b.Headers) == 0
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
