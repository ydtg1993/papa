package core

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// wrap 哨兵：包过的错误要能 unwrap 回去（日志与 errors.Is 都依赖它），
// 并声明自己不可重试。
func TestWrapNoRetry(t *testing.T) {
	base := errors.New("页面结构变了")

	wrapped := WrapNoRetry(base)
	if wrapped == nil {
		t.Fatal("非 nil 错误应被包装")
	}
	if !errors.Is(wrapped, base) {
		t.Fatal("应当能 unwrap 回原错误")
	}
	if wrapped.Error() != base.Error() {
		t.Fatalf("错误文本应原样透出：%q", wrapped.Error())
	}
	if Retryable(wrapped) {
		t.Fatal("包过的错误应不可重试")
	}
	// 不带分类时 Kind() 为空 → ErrorKind 回退到 no-retry
	if got := ErrorKind(wrapped); got != "no-retry" {
		t.Fatalf("ErrorKind = %q, want no-retry", got)
	}

	// nil 进 nil 出：不然业务写 `return WrapNoRetry(err)` 会在成功路径上造一个假错误
	if WrapNoRetry(nil) != nil {
		t.Fatal("WrapNoRetry(nil) 应当是 nil")
	}
	if WrapNoRetryKind("x", nil) != nil {
		t.Fatal("WrapNoRetryKind(kind, nil) 应当是 nil")
	}
}

func TestWrapNoRetryKind(t *testing.T) {
	wrapped := WrapNoRetryKind("structure", errors.New("boom"))
	if got := ErrorKind(wrapped); got != "structure" {
		t.Fatalf("ErrorKind = %q, want structure", got)
	}
	var nre *NoRetryError
	if !errors.As(wrapped, &nre) {
		t.Fatal("应当能 As 到 *NoRetryError")
	}
	if nre.Kind() != "structure" || nre.Retryable() {
		t.Fatalf("NoRetryError = %+v", nre)
	}
	// errors.As 穿透 fmt.Errorf 的 %w 包装：业务在中间层再包一层也认得出
	deeper := fmt.Errorf("catalog: %w", wrapped)
	if got := ErrorKind(deeper); got != "structure" {
		t.Fatalf("穿过 %%w 之后 ErrorKind = %q, want structure", got)
	}
	if Retryable(deeper) {
		t.Fatal("穿过 %w 之后仍应不可重试")
	}
}

// Retryable 的默认是"可重试"：没声明过的错误按普通网络抖动处理，靠重试兜住。
func TestRetryableDefault(t *testing.T) {
	if Retryable(nil) {
		t.Fatal("nil 不该被当作可重试")
	}
	if !Retryable(errors.New("网络抖动")) {
		t.Fatal("没声明过的错误应默认可重试")
	}
}

// 实现了 Kind() 但返回空串时，回退到按可重试性推断 —— 空分类等于没分类。
func TestErrorKindFallbacks(t *testing.T) {
	if got := ErrorKind(nil); got != "" {
		t.Fatalf("nil 的 ErrorKind = %q, want 空串", got)
	}
	if got := ErrorKind(errors.New("x")); got != "retryable" {
		t.Fatalf("普通错误 = %q, want retryable", got)
	}
	if got := ErrorKind(&NoRetryError{Err: errors.New("x")}); got != "no-retry" {
		t.Fatalf("无分类的不可重试错误 = %q, want no-retry", got)
	}
	if got := ErrorKind(emptyKindErr{}); got != "retryable" {
		t.Fatalf("Kind() 返回空串时应回退：%q", got)
	}
}

// 自定义错误可以自己声明分类与可重试性 —— Retryable/ErrorKind 认的是接口，不是具体类型。
type customErr struct {
	kind      string
	retryable bool
}

func (e customErr) Error() string   { return "custom" }
func (e customErr) Kind() string    { return e.kind }
func (e customErr) Retryable() bool { return e.retryable }

func TestErrorKindHonoursCustomInterface(t *testing.T) {
	if got := ErrorKind(customErr{kind: "protected", retryable: false}); got != "protected" {
		t.Fatalf("自定义分类 = %q, want protected", got)
	}
	if Retryable(customErr{retryable: true}) != true {
		t.Fatal("自定义声明可重试时应返回 true")
	}
	if Retryable(customErr{retryable: false}) != false {
		t.Fatal("自定义声明不可重试时应返回 false")
	}
}

// emptyKindErr 实现了 Kind() 但返回空串（用于验证回退逻辑）。
type emptyKindErr struct{}

func (emptyKindErr) Error() string { return "empty-kind" }
func (emptyKindErr) Kind() string  { return "" }

// 受限页错误的形状是**约定**：分类名 access_restricted 会写进告警与 ops 的过滤条件，
// 消息里要能看到是哪一步、哪个 URL、命中的哪句话（排查时不用再去翻页面归档）。
func TestRestrictedPageErrorMessageAndKind(t *testing.T) {
	err := RestrictedPageError("catalog", "https://example.com/list?page=2", "验证码")
	if err == nil {
		t.Fatal("RestrictedPageError 不该返回 nil")
	}
	if Retryable(err) {
		t.Fatal("受限页必须不可重试")
	}
	if kind := ErrorKind(err); kind != "access_restricted" {
		t.Fatalf("分类 = %q, want access_restricted", kind)
	}
	msg := err.Error()
	for _, want := range []string{"catalog", "https://example.com/list?page=2", "验证码"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("消息里缺 %q：%s", want, msg)
		}
	}

	// 没给命中话术（自己判的情况）也要能出消息，不能出现空的"（命中：）"
	msg = RestrictedPageError("detail", "https://example.com/a", "").Error()
	if strings.Contains(msg, "命中") {
		t.Fatalf("没有命中话术时不该出现那段：%s", msg)
	}
}
