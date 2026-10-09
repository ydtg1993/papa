package core

import "errors"

// NoRetryError 包装错误，标记其不可重试（结构错误、资源不存在、访问受限等）。
// 实现 Retryable() bool 以便 worker 识别后直接标 failed 且不再空转。
type NoRetryError struct {
	Err  error
	kind string
}

func (e *NoRetryError) Error() string   { return e.Err.Error() }
func (e *NoRetryError) Unwrap() error   { return e.Err }
func (e *NoRetryError) Retryable() bool { return false }
func (e *NoRetryError) Kind() string    { return e.kind }

// WrapNoRetry 将 err 标记为不可重试；err 为 nil 时返回 nil。
func WrapNoRetry(err error) error {
	return wrapNoRetry("", err)
}

// WrapNoRetryKind 将 err 标记为不可重试并携带分类标识（如 structure/not-found/protected）。
func WrapNoRetryKind(kind string, err error) error {
	return wrapNoRetry(kind, err)
}

func wrapNoRetry(kind string, err error) error {
	if err == nil {
		return nil
	}
	return &NoRetryError{Err: err, kind: kind}
}

// RestrictedPageError 标准化的「这一页被反爬拦下了」错误：**不可重试**，分类 access_restricted。
//
// reason 传 `htmlfetch.RestrictedReason` 的返回值（命中的那句话）—— 它会写进消息、日志与告警里，
// 排查时"为什么判它是受限页"一眼可见。业务一般不用直接调：`engine.RestrictedError` 会把站点词表
// （`SiteSpec.RestrictedKeywords`）一并处理掉。
func RestrictedPageError(stage, pageURL, reason string) error {
	msg := stage + " page is access-restricted: " + pageURL
	if reason != "" {
		msg += "（命中：" + reason + "）"
	}
	return WrapNoRetryKind("access_restricted", errors.New(msg))
}

// Retryable 判断 err 是否应重试：默认视为可重试；
// 若 err 实现了 Retryable() bool，则以自身声明为准。
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	var r interface{ Retryable() bool }
	if errors.As(err, &r) {
		return r.Retryable()
	}
	return true
}

// ErrorKind 返回错误的分类标识：优先取错误自带的 Kind() string；
// 无法识别时按是否可重试返回 "retryable" / "no-retry"。err 为 nil 返回空串。
func ErrorKind(err error) string {
	if err == nil {
		return ""
	}
	var c interface{ Kind() string }
	if errors.As(err, &c) {
		if k := c.Kind(); k != "" {
			return k
		}
	}
	if !Retryable(err) {
		return "no-retry"
	}
	return "retryable"
}
