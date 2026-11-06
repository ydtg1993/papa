// Package model supplies bounded, host-configured text completion providers.
// Providers do not execute model output or provide website access.
package model

import (
	"context"
	"errors"
	"fmt"
)

// Message separates trusted instructions from untrusted observations. Callers
// are responsible for choosing the role; providers never infer roles from text.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Usage is the service's reported token consumption. Known is false when the
// response omitted usage; it does not mean the call was free.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
	Known        bool
}

// Reply contains exactly one complete assistant text response.
type Reply struct {
	Content string
	Usage   Usage
}

// Provider makes one completion attempt. maxOutputTokens is a positive ceiling,
// including reasoning tokens when the service uses max_completion_tokens.
type Provider interface {
	Complete(context.Context, []Message, int) (Reply, error)
}

// Stable errors intentionally omit endpoint URLs, credentials, messages and
// service bodies. Use errors.Is to classify failures.
var (
	ErrConfig          = errors.New("model: invalid configuration")
	ErrCredentials     = errors.New("model: credentials unavailable or invalid")
	ErrInput           = errors.New("model: invalid input")
	ErrInputLimit      = errors.New("model: input size limit exceeded")
	ErrOutputLimit     = errors.New("model: invalid output token limit")
	ErrUnauthorized    = errors.New("model: authentication or authorization rejected")
	ErrRateLimit       = errors.New("model: rate limit rejected")
	ErrHTTP            = errors.New("model: service HTTP failure")
	ErrRedirect        = errors.New("model: service redirect rejected")
	ErrTimeout         = fmt.Errorf("model: timeout: %w", context.DeadlineExceeded)
	ErrCanceled        = fmt.Errorf("model: canceled: %w", context.Canceled)
	ErrTransport       = errors.New("model: transport failure")
	ErrResponseLimit   = errors.New("model: response size limit exceeded")
	ErrInvalidResponse = errors.New("model: invalid response")
	ErrRefusal         = errors.New("model: response refused")
	ErrTruncated       = errors.New("model: response truncated")
	ErrToolCall        = errors.New("model: unexpected tool call")
	ErrExhausted       = errors.New("model: replay exhausted")
)
