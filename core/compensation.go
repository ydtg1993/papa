package core

import (
	"errors"
	"time"
)

// CompensationMode controls a later task round, independently of immediate
// stage retries. Empty retains the historical error-queue behavior.
type CompensationMode string

const (
	CompensationAutomatic    CompensationMode = ""
	CompensationRetryAfter   CompensationMode = "retry_after"
	CompensationAccountReady CompensationMode = "account_ready"
	CompensationManual       CompensationMode = "manual"
	CompensationPermanent    CompensationMode = "permanent"
)

type CompensationPolicy struct {
	Mode      CompensationMode `json:"mode"`
	NotBefore time.Time        `json:"not_before,omitempty"`
	Reason    string           `json:"reason,omitempty"`
}

type compensationError struct {
	err    error
	policy CompensationPolicy
}

func (e *compensationError) Error() string                    { return e.err.Error() }
func (e *compensationError) Unwrap() error                    { return e.err }
func (e *compensationError) Retryable() bool                  { return false }
func (e *compensationError) Compensation() CompensationPolicy { return e.policy }

// WithCompensation ends the current attempt and declares the condition for a
// later round. It does not create a new task or change its identity. Unknown
// modes fail closed as manual work instead of silently allowing automatic retry.
func WithCompensation(err error, policy CompensationPolicy) error {
	if err == nil {
		return nil
	}
	switch policy.Mode {
	case CompensationAutomatic, CompensationRetryAfter, CompensationAccountReady, CompensationManual, CompensationPermanent:
	default:
		policy.Mode = CompensationManual
		policy.Reason = "invalid compensation policy"
	}
	if policy.Mode == CompensationRetryAfter && policy.NotBefore.IsZero() {
		policy.Mode = CompensationManual
		policy.Reason = "retry_after requires a deadline"
	}
	if !policy.NotBefore.IsZero() && (policy.NotBefore.Year() < 1000 || policy.NotBefore.Year() > 9999) {
		policy.Mode = CompensationManual
		policy.NotBefore = time.Time{}
		policy.Reason = "compensation deadline is outside supported database range"
	}
	return &compensationError{err: err, policy: policy}
}

func RetryAfter(err error, at time.Time) error {
	return WithCompensation(err, CompensationPolicy{Mode: CompensationRetryAfter, NotBefore: at})
}
func ManualCompensation(err error, reason string) error {
	return WithCompensation(err, CompensationPolicy{Mode: CompensationManual, Reason: reason})
}
func PermanentFailure(err error, reason string) error {
	return WithCompensation(err, CompensationPolicy{Mode: CompensationPermanent, Reason: reason})
}

// CompensationOf also supports business errors implementing Compensation().
// NoRetryError alone has no compensation policy and keeps its existing meaning.
func CompensationOf(err error) (CompensationPolicy, bool) {
	var p interface{ Compensation() CompensationPolicy }
	if errors.As(err, &p) {
		return p.Compensation(), true
	}
	return CompensationPolicy{}, false
}
