package core

import (
	"errors"
	"testing"
)

func TestFailureCodeDoesNotChangeRetryPolicy(t *testing.T) {
	base := errors.New("fixture failure")
	wrapped := WithFailureCode(base, "parse", "missing_title")
	if !errors.Is(wrapped, base) || !Retryable(wrapped) {
		t.Fatal("diagnostic wrapper changed error identity or retryability")
	}
	wrapped = WithFailureCode(WrapNoRetryKind("structure", base), "parse", "missing_title")
	if Retryable(wrapped) || ErrorKind(wrapped) != "structure" || !errors.Is(wrapped, base) {
		t.Fatal("diagnostic wrapper lost underlying policy")
	}
	if WithFailureCode(nil, "parse", "missing_title") != nil {
		t.Fatal("nil must stay nil")
	}
}
