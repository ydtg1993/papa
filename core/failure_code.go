package core

// FailureCodeError adds stable diagnostic metadata without changing retry or
// compensation policy. Wrap with WrapNoRetryKind separately when appropriate.
type FailureCodeError struct {
	Err   error
	Phase string
	Code  string
}

func (e *FailureCodeError) Error() string                     { return e.Err.Error() }
func (e *FailureCodeError) Unwrap() error                     { return e.Err }
func (e *FailureCodeError) FailureCode() (phase, code string) { return e.Phase, e.Code }

// WithFailureCode gives the breaker a stable rule (e.g. parse/title_missing).
// Codes must identify a rule, not carry URLs, account IDs or credentials.
func WithFailureCode(err error, phase, code string) error {
	if err == nil {
		return nil
	}
	return &FailureCodeError{Err: err, Phase: phase, Code: code}
}
