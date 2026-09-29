package crawler

import (
	"errors"
	"fmt"
	"testing"
)

func TestRetryable(t *testing.T) {
	if Retryable(nil) {
		t.Fatal("nil error should be non-retryable")
	}
	if !Retryable(errors.New("plain")) {
		t.Fatal("plain error should be retryable by default")
	}
	if Retryable(WrapNoRetry(errors.New("structure"))) {
		t.Fatal("WrapNoRetry error should be non-retryable")
	}
	// 嵌套包装也应能识别不可重试标记
	if Retryable(fmt.Errorf("wrap: %w", WrapNoRetry(errors.New("nested")))) {
		t.Fatal("nested no-retry error should be non-retryable")
	}
}

func TestWrapNoRetry(t *testing.T) {
	if WrapNoRetry(nil) != nil {
		t.Fatal("WrapNoRetry(nil) should return nil")
	}
	sentinel := errors.New("boom")
	err := WrapNoRetry(sentinel)
	var nr *NoRetryError
	if !errors.As(err, &nr) {
		t.Fatal("WrapNoRetry should produce *NoRetryError")
	}
	if nr.Error() != "boom" {
		t.Fatalf("unexpected error message: %s", nr.Error())
	}
	if !errors.Is(err, sentinel) {
		t.Fatal("Unwrap should surface the wrapped error")
	}
}

func TestWrapNoRetryKind(t *testing.T) {
	err := WrapNoRetryKind("structure", errors.New("missing field"))
	var nr *NoRetryError
	if !errors.As(err, &nr) {
		t.Fatal("WrapNoRetryKind should produce *NoRetryError")
	}
	if nr.Kind() != "structure" {
		t.Fatalf("unexpected kind: %s", nr.Kind())
	}
	if Retryable(err) {
		t.Fatal("WrapNoRetryKind error should be non-retryable")
	}
}

func TestErrorKind(t *testing.T) {
	if ErrorKind(nil) != "" {
		t.Fatal("nil kind should be empty")
	}
	if ErrorKind(errors.New("plain")) != "retryable" {
		t.Fatal("plain error should be retryable")
	}
	if ErrorKind(WrapNoRetry(errors.New("x"))) != "no-retry" {
		t.Fatal("no-retry without kind should be no-retry")
	}
	if ErrorKind(WrapNoRetryKind("protected", errors.New("x"))) != "protected" {
		t.Fatal("kind should be preserved")
	}
	if ErrorKind(fmt.Errorf("wrap: %w", WrapNoRetryKind("not-found", errors.New("x")))) != "not-found" {
		t.Fatal("nested kind should be detected")
	}
}
