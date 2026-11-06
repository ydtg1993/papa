package model

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// OutputTokenField is selected by the host, never by fallback requests.
type OutputTokenField string

const (
	MaxCompletionTokens OutputTokenField = "max_completion_tokens"
	MaxTokens           OutputTokenField = "max_tokens"

	DefaultMaxInputBytes    int64 = 128 << 10
	HardMaxInputBytes       int64 = 256 << 10
	DefaultMaxResponseBytes int64 = 256 << 10
	HardMaxResponseBytes    int64 = 1 << 20
	DefaultMaxOutputTokens        = 4096
	HardMaxOutputTokens           = 16384
)

// Config is copied and validated by New. Zero limits select defaults. BaseURL
// is an API base (for example https://host/v1), not a completion URL; New appends
// /chat/completions and never inserts /v1. AllowHTTP permits only explicit local
// loopback test endpoints. APIKeyEnv names an environment variable, not a key.
type Config struct {
	BaseURL          string
	Model            string
	APIKeyEnv        string
	Timeout          time.Duration
	OutputTokenField OutputTokenField
	MaxInputBytes    int64
	MaxResponseBytes int64
	MaxOutputTokens  int
	AllowHTTP        bool
}

// Compatible implements one nonstreaming OpenAI-compatible request. Its frozen
// transport uses no environment proxy, redirects, retries or shared HTTP client.
type Compatible struct {
	endpoint         string
	model            string
	key              string
	outputTokenField OutputTokenField
	maxInputBytes    int64
	maxResponseBytes int64
	maxOutputTokens  int
	client           *http.Client
}

var _ Provider = (*Compatible)(nil)

// String and GoString keep accidental diagnostic formatting from exposing the
// credential held in the client's private fields.
func (p Compatible) String() string   { return "model.Compatible{redacted}" }
func (p Compatible) GoString() string { return p.String() }

// New freezes configuration and reads APIKeyEnv once. It performs no network
// requests and never searches other environment variables for credentials.
func New(config Config) (*Compatible, error) {
	u, err := url.Parse(config.BaseURL)
	if err != nil || u == nil || u.Opaque != "" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(config.BaseURL, "#") {
		return nil, ErrConfig
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, ErrConfig
	}
	if u.Scheme == "http" && (!config.AllowHTTP || !loopbackHost(u.Hostname())) {
		return nil, ErrConfig
	}
	if !validHost(u) || strings.HasSuffix(u.Host, ":") {
		return nil, ErrConfig
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, ErrConfig
		}
	}
	if !validConfigText(config.Model, 256) || !validEnvName(config.APIKeyEnv) {
		return nil, ErrConfig
	}
	if config.Timeout == 0 {
		config.Timeout = 30 * time.Second
	}
	if config.Timeout < 0 || config.Timeout > 10*time.Minute {
		return nil, ErrConfig
	}
	if config.OutputTokenField == "" {
		config.OutputTokenField = MaxCompletionTokens
	}
	if config.OutputTokenField != MaxCompletionTokens && config.OutputTokenField != MaxTokens {
		return nil, ErrConfig
	}
	if config.MaxInputBytes == 0 {
		config.MaxInputBytes = DefaultMaxInputBytes
	}
	if config.MaxResponseBytes == 0 {
		config.MaxResponseBytes = DefaultMaxResponseBytes
	}
	if config.MaxOutputTokens == 0 {
		config.MaxOutputTokens = DefaultMaxOutputTokens
	}
	if config.MaxInputBytes < 1 || config.MaxInputBytes > HardMaxInputBytes || config.MaxResponseBytes < 1 || config.MaxResponseBytes > HardMaxResponseBytes || config.MaxOutputTokens < 1 || config.MaxOutputTokens > HardMaxOutputTokens {
		return nil, ErrConfig
	}
	key, ok := os.LookupEnv(config.APIKeyEnv)
	if !ok || !validHeaderSecret(key) {
		return nil, ErrCredentials
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/chat/completions"
	if u.RawPath != "" {
		u.RawPath = strings.TrimRight(u.RawPath, "/") + "/chat/completions"
	}
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            (&net.Dialer{Timeout: config.Timeout, KeepAlive: -1}).DialContext,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:    config.Timeout,
		ResponseHeaderTimeout:  config.Timeout,
		MaxResponseHeaderBytes: 32 << 10,
		DisableCompression:     true,
		// A fresh connection for each attempt also prevents net/http's reused
		// connection retry path from silently making an additional attempt.
		DisableKeepAlives: true,
	}
	return &Compatible{
		endpoint: u.String(), model: config.Model, key: key,
		outputTokenField: config.OutputTokenField,
		maxInputBytes:    config.MaxInputBytes, maxResponseBytes: config.MaxResponseBytes,
		maxOutputTokens: config.MaxOutputTokens,
		client: &http.Client{
			Transport: transport, Timeout: config.Timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validHost(u *url.URL) bool {
	host := u.Hostname()
	if host == "" || strings.ContainsAny(host, "\\% \t\r\n") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return !strings.Contains(host, ":") || strings.HasPrefix(u.Host, "[")
	}
	if strings.HasPrefix(u.Host, "[") || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(host, "."), ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if c != '-' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
				return false
			}
		}
	}
	return true
}

func validConfigText(s string, limit int) bool {
	if s == "" || len(s) > limit || strings.TrimSpace(s) != s || !utf8.ValidString(s) {
		return false
	}
	for _, c := range s {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

func validEnvName(s string) bool {
	if len(s) == 0 || len(s) > 256 {
		return false
	}
	for i, c := range s {
		if c != '_' && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (i == 0 || c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func validHeaderSecret(key string) bool {
	if len(key) == 0 || len(key) > 8192 {
		return false
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x21 || key[i] > 0x7e {
			return false
		}
	}
	return true
}

type completionRequest struct {
	Model               string    `json:"model"`
	Messages            []Message `json:"messages"`
	N                   int       `json:"n"`
	Stream              bool      `json:"stream"`
	MaxCompletionTokens int       `json:"max_completion_tokens,omitempty"`
	MaxTokens           int       `json:"max_tokens,omitempty"`
}

// Complete sends at most one POST. The input ceiling includes the entire JSON
// request, not just its visible text. The response ceiling is checked both from
// Content-Length and while reading; compressed bodies are not accepted.
func (p *Compatible) Complete(ctx context.Context, messages []Message, maxOutputTokens int) (Reply, error) {
	if p == nil || p.client == nil || ctx == nil {
		return Reply{}, ErrInput
	}
	if err := contextFailure(ctx); err != nil {
		return Reply{}, err
	}
	if maxOutputTokens < 1 || maxOutputTokens > p.maxOutputTokens {
		return Reply{}, ErrOutputLimit
	}
	if len(messages) == 0 {
		return Reply{}, ErrInput
	}
	var rawSize int64
	for _, message := range messages {
		if message.Role != "system" && message.Role != "user" && message.Role != "assistant" {
			return Reply{}, ErrInput
		}
		if message.Content == "" || !utf8.ValidString(message.Content) {
			return Reply{}, ErrInput
		}
		if int64(len(message.Content)) > p.maxInputBytes-rawSize {
			return Reply{}, ErrInputLimit
		}
		rawSize += int64(len(message.Content))
	}
	request := completionRequest{Model: p.model, Messages: messages, N: 1}
	if p.outputTokenField == MaxTokens {
		request.MaxTokens = maxOutputTokens
	} else {
		request.MaxCompletionTokens = maxOutputTokens
	}
	body, err := json.Marshal(request)
	if err != nil {
		return Reply{}, ErrInput
	}
	if int64(len(body)) > p.maxInputBytes {
		return Reply{}, ErrInputLimit
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return Reply{}, ErrInput
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Authorization", "Bearer "+p.key)
	resp, err := p.client.Do(req)
	if err != nil {
		return Reply{}, transportFailure(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return Reply{}, ErrRedirect
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return Reply{}, ErrUnauthorized
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return Reply{}, ErrRateLimit
	}
	if resp.StatusCode != http.StatusOK {
		return Reply{}, ErrHTTP
	}
	if resp.ContentLength > p.maxResponseBytes {
		return Reply{}, ErrResponseLimit
	}
	contentType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || (contentType != "application/json" && !strings.HasSuffix(contentType, "+json")) {
		return Reply{}, ErrInvalidResponse
	}
	if encoding := resp.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return Reply{}, ErrInvalidResponse
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, p.maxResponseBytes+1))
	if int64(len(data)) > p.maxResponseBytes {
		return Reply{}, ErrResponseLimit
	}
	if err != nil {
		return Reply{}, transportFailure(ctx, err)
	}
	if err := contextFailure(ctx); err != nil {
		return Reply{}, err
	}
	return decodeReply(data)
}

func contextFailure(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return ErrCanceled
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ErrTimeout
	}
	return nil
}

func transportFailure(ctx context.Context, err error) error {
	if failure := contextFailure(ctx); failure != nil {
		return failure
	}
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		return ErrTimeout
	}
	return ErrTransport
}

type completionResponse struct {
	Choices []struct {
		Index        *int   `json:"index"`
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Role         string          `json:"role"`
			Content      *string         `json:"content"`
			Refusal      json.RawMessage `json:"refusal"`
			ToolCalls    json.RawMessage `json:"tool_calls"`
			FunctionCall json.RawMessage `json:"function_call"`
		} `json:"message"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage"`
}

func decodeReply(body []byte) (Reply, error) {
	if !utf8.Valid(body) || !validJSON(body) {
		return Reply{}, ErrInvalidResponse
	}
	var response completionResponse
	if json.Unmarshal(body, &response) != nil || len(response.Choices) != 1 {
		return Reply{}, ErrInvalidResponse
	}
	choice := response.Choices[0]
	if choice.Index != nil && *choice.Index != 0 {
		return Reply{}, ErrInvalidResponse
	}
	if nonNull(choice.Message.Refusal) || choice.FinishReason == "content_filter" {
		return Reply{}, ErrRefusal
	}
	if nonNull(choice.Message.ToolCalls) || nonNull(choice.Message.FunctionCall) || choice.FinishReason == "tool_calls" || choice.FinishReason == "function_call" {
		return Reply{}, ErrToolCall
	}
	if choice.FinishReason == "length" {
		return Reply{}, ErrTruncated
	}
	if choice.FinishReason != "stop" || choice.Message.Role != "assistant" || choice.Message.Content == nil || strings.TrimSpace(*choice.Message.Content) == "" {
		return Reply{}, ErrInvalidResponse
	}
	reply := Reply{Content: *choice.Message.Content}
	if len(response.Usage) > 0 {
		var usage struct {
			Input  *int64 `json:"prompt_tokens"`
			Output *int64 `json:"completion_tokens"`
			Total  *int64 `json:"total_tokens"`
		}
		if !nonNull(response.Usage) || json.Unmarshal(response.Usage, &usage) != nil || usage.Input == nil || usage.Output == nil || *usage.Input < 0 || *usage.Output < 0 || *usage.Output > math.MaxInt64-*usage.Input {
			return Reply{}, ErrInvalidResponse
		}
		if usage.Total != nil && (*usage.Total < 0 || *usage.Total != *usage.Input+*usage.Output) {
			return Reply{}, ErrInvalidResponse
		}
		// Explicit null is not an absent optional total.
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(response.Usage, &fields)
		if total, exists := fields["total_tokens"]; exists && !nonNull(total) {
			return Reply{}, ErrInvalidResponse
		}
		reply.Usage = Usage{InputTokens: *usage.Input, OutputTokens: *usage.Output, Known: true}
	}
	return reply, nil
}

func nonNull(raw json.RawMessage) bool {
	return len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// validJSON rejects duplicate object keys, trailing values and excessive
// nesting before decoding into the compatible service envelope.
func validJSON(body []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if !jsonValue(decoder, 0) {
		return false
	}
	_, err := decoder.Token()
	return errors.Is(err, io.EOF)
}

func jsonValue(decoder *json.Decoder, depth int) bool {
	if depth > 64 {
		return false
	}
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return true
	}
	switch delimiter {
	case '{':
		seen := make(map[string]bool)
		for decoder.More() {
			keyToken, err := decoder.Token()
			key, ok := keyToken.(string)
			if err != nil || !ok || seen[key] {
				return false
			}
			seen[key] = true
			if !jsonValue(decoder, depth+1) {
				return false
			}
		}
		end, err := decoder.Token()
		return err == nil && end == json.Delim('}')
	case '[':
		for decoder.More() {
			if !jsonValue(decoder, depth+1) {
				return false
			}
		}
		end, err := decoder.Token()
		return err == nil && end == json.Delim(']')
	default:
		return false
	}
}
