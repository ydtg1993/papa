package engine

import (
	"errors"
	"fmt"
	"github.com/ydtg1993/papa/v4/core"
	"testing"
)

func TestRetryable(t *testing.T) {
	if core.Retryable(nil) {
		t.Fatal("nil error should be non-retryable")
	}
	if !core.Retryable(errors.New("plain")) {
		t.Fatal("plain error should be retryable by default")
	}
	if core.Retryable(core.WrapNoRetry(errors.New("structure"))) {
		t.Fatal("core.WrapNoRetry error should be non-retryable")
	}
	// 嵌套包装也应能识别不可重试标记
	if core.Retryable(fmt.Errorf("wrap: %w", core.WrapNoRetry(errors.New("nested")))) {
		t.Fatal("nested no-retry error should be non-retryable")
	}
}

func TestWrapNoRetry(t *testing.T) {
	if core.WrapNoRetry(nil) != nil {
		t.Fatal("core.WrapNoRetry(nil) should return nil")
	}
	sentinel := errors.New("boom")
	err := core.WrapNoRetry(sentinel)
	var nr *core.NoRetryError
	if !errors.As(err, &nr) {
		t.Fatal("core.WrapNoRetry should produce *core.NoRetryError")
	}
	if nr.Error() != "boom" {
		t.Fatalf("unexpected error message: %s", nr.Error())
	}
	if !errors.Is(err, sentinel) {
		t.Fatal("Unwrap should surface the wrapped error")
	}
}

func TestWrapNoRetryKind(t *testing.T) {
	err := core.WrapNoRetryKind("structure", errors.New("missing field"))
	var nr *core.NoRetryError
	if !errors.As(err, &nr) {
		t.Fatal("core.WrapNoRetryKind should produce *core.NoRetryError")
	}
	if nr.Kind() != "structure" {
		t.Fatalf("unexpected kind: %s", nr.Kind())
	}
	if core.Retryable(err) {
		t.Fatal("core.WrapNoRetryKind error should be non-retryable")
	}
}

func TestErrorKind(t *testing.T) {
	if core.ErrorKind(nil) != "" {
		t.Fatal("nil kind should be empty")
	}
	if core.ErrorKind(errors.New("plain")) != "retryable" {
		t.Fatal("plain error should be retryable")
	}
	if core.ErrorKind(core.WrapNoRetry(errors.New("x"))) != "no-retry" {
		t.Fatal("no-retry without kind should be no-retry")
	}
	if core.ErrorKind(core.WrapNoRetryKind("protected", errors.New("x"))) != "protected" {
		t.Fatal("kind should be preserved")
	}
	if core.ErrorKind(fmt.Errorf("wrap: %w", core.WrapNoRetryKind("not-found", errors.New("x")))) != "not-found" {
		t.Fatal("nested kind should be detected")
	}
}
