package crawler

import "context"

// AlertLevel 告警级别。
type AlertLevel int

const (
	AlertInfo AlertLevel = iota
	AlertWarn
	AlertError
)

func (l AlertLevel) String() string {
	switch l {
	case AlertInfo:
		return "info"
	case AlertWarn:
		return "warn"
	case AlertError:
		return "error"
	}
	return "unknown"
}

// TaskError 结构化任务错误上下文，覆盖手册要求的日志字段。
type TaskError struct {
	Stage   string `json:"stage"`
	TaskID  int    `json:"task_id"`
	URL     string `json:"url"`
	Retry   int    `json:"retry"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// AlertEvent 告警事件，在任务最终失败时发出。
type AlertEvent struct {
	Level AlertLevel `json:"level"`
	TaskError
}

// Notifier 告警通知接口，业务可实现钉钉/webhook 等。
type Notifier interface {
	Notify(ctx context.Context, event AlertEvent) error
}
