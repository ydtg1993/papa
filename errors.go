package papa

import (
	"github.com/ydtg1993/papa/v2/core"
)

// 本文件是**错误与告警门面**。
//
// 这些类型本身住在 core（零依赖叶子包），这里只做一次转发 ——
// 让业务代码统一写 `papa.Xxx`，不用关心它落在哪个包里。

// NoRetryError 标记不可重试的错误（结构错误、资源不存在等）。
type NoRetryError = core.NoRetryError

// Notifier 告警通知接口。
type Notifier = core.Notifier

// AlertEvent 告警事件。
type AlertEvent = core.AlertEvent

// TaskError 结构化任务错误上下文。
type TaskError = core.TaskError

// AlertLevel 告警级别。
type AlertLevel = core.AlertLevel

// 告警级别常量。
const (
	AlertInfo     = core.AlertInfo
	AlertWarn     = core.AlertWarn
	AlertError    = core.AlertError
	AlertCritical = core.AlertCritical
)

// WrapNoRetry 将错误标记为不可重试。
var WrapNoRetry = core.WrapNoRetry

// WrapNoRetryKind 将错误标记为不可重试并携带分类标识（如 structure/not-found/protected）。
var WrapNoRetryKind = core.WrapNoRetryKind

// RestrictedPageError 标准化的「这一页被反爬拦下了」错误：**不可重试**，分类 access_restricted。
//
// **一般不用直接调它**：`engine.RestrictedError(task, page, stage)` 把站点词表（`task.Site`
// 那份 `RestrictedKeywords`）一并处理了 ——
//
//	if err := engine.RestrictedError(task, page, "catalog"); err != nil { return err }
//
// 需要自己判、或词表不来自站点声明时才用它（reason 传 `htmlfetch.RestrictedReason` 的返回值，
// 即命中的那句话，它会写进消息、日志与告警）。
var RestrictedPageError = core.RestrictedPageError

// Retryable 判断错误是否应重试。
var Retryable = core.Retryable

// ErrorKind 返回错误的分类标识。
var ErrorKind = core.ErrorKind
