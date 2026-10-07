// Package core 汇集**跨包流动的纯值类型**：告警事件、任务错误上下文、监控快照 DTO。
//
// 它是一个**零依赖叶子包** —— 只 import 标准库。存在的理由很具体：
// admin/server 与 pkg/notify 只需要这几个 struct，却因为它们在 engine 包里，
// 不得不把整个引擎（连同 rod、m3u8、ffmpeg 下载器）拖进自己的依赖集。
//
// 边界在哪：**引擎自己的类型不在这里**。Engine / Task / Trace / Fetcher 与引擎强耦合
// （Fetcher 的签名里就带 *Engine），搬过来只会把 gorm、logrus、models 一并灌进叶子包，
// 换来零解耦收益。所以这里只放「谁都能拿、谁也不依赖谁」的值。
package core

import "context"

// AlertLevel 告警级别。
type AlertLevel int

const (
	AlertInfo AlertLevel = iota
	AlertWarn
	AlertError
	// AlertCritical 需要人介入的那种：比如熔断已把整个爬虫闸住。比 AlertError（单个任务失败）高一级，
	// 好让 webhook 那边能单独路由（钉钉 @全体之类）。
	AlertCritical
)

func (l AlertLevel) String() string {
	switch l {
	case AlertInfo:
		return "info"
	case AlertWarn:
		return "warn"
	case AlertError:
		return "error"
	case AlertCritical:
		return "critical"
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
