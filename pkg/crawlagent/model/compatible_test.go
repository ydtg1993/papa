package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testKeyEnv = "PAPA_MODEL_TEST_KEY"
const testKey = "test-secret-never-in-errors"
const validResponse = `{"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"{\"tool\":\"finish\"}"}}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`

func newTestProvider(t *testing.T, handler http.HandlerFunc, update func(*Config)) (*Compatible, *httptest.Server) {
	t.Helper()
	t.Setenv(testKeyEnv, testKey)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	config := Config{BaseURL: server.URL, Model: "host-selected-model", APIKeyEnv: testKeyEnv, AllowHTTP: true}
	if update != nil {
		update(&config)
	}
	provider, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return provider, server
}

func testMessages() []Message {
	return []Message{{Role: "system", Content: "Only return one action."}, {Role: "user", Content: "untrusted prompt with secret marker"}}
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = io.WriteString(w, body)
}

func TestCompatibleRequestShapeAndFrozenConfiguration(t *testing.T) {
	for _, field := range []OutputTokenField{"", MaxTokens} {
		t.Run(string(field), func(t *testing.T) {
			requests := make(chan map[string]json.RawMessage, 1)
			provider, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || r.URL.RawQuery != "" {
					t.Errorf("request = %s %s", r.Method, r.URL)
				}
				if r.Header.Get("Authorization") != "Bearer "+testKey || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Accept-Encoding") != "identity" {
					t.Error("required headers missing")
				}
				var request map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				requests <- request
				writeJSON(w, validResponse)
			}, func(c *Config) {
				c.BaseURL += "/v1/"
				c.OutputTokenField = field
			})
			// The secret is resolved at construction, not on each call.
			t.Setenv(testKeyEnv, "changed-environment-key")
			reply, err := provider.Complete(context.Background(), testMessages(), 123)
			if err != nil || reply.Content != `{"tool":"finish"}` || reply.Usage != (Usage{InputTokens: 7, OutputTokens: 3, Known: true}) {
				t.Fatalf("reply=%+v error=%v", reply, err)
			}
			request := <-requests
			wantField := field
			if wantField == "" {
				wantField = MaxCompletionTokens
			}
			if string(request[string(wantField)]) != "123" || string(request["n"]) != "1" || string(request["stream"]) != "false" || string(request["model"]) != `"host-selected-model"` || len(request) != 5 {
				t.Fatalf("unexpected request shape: %v", request)
			}
			var messages []Message
			if json.Unmarshal(request["messages"], &messages) != nil || !reflect.DeepEqual(messages, testMessages()) {
				t.Fatal("messages were changed")
			}
		})
	}
}

func TestCompatibleCopiesConfigurationAndJoinsOnlyCompletionPath(t *testing.T) {
	t.Setenv(testKeyEnv, testKey)
	for _, test := range []struct{ base, endpoint string }{
		{"https://example.test", "https://example.test/chat/completions"},
		{"https://example.test/api/v1///", "https://example.test/api/v1/chat/completions"},
		{"https://example.test/custom%2Fbase", "https://example.test/custom%2Fbase/chat/completions"},
	} {
		config := Config{BaseURL: test.base, Model: "original", APIKeyEnv: testKeyEnv}
		provider, err := New(config)
		if err != nil {
			t.Fatal(err)
		}
		config.BaseURL, config.Model, config.MaxInputBytes = "https://changed.test", "changed", 1
		if provider.endpoint != test.endpoint || provider.model != "original" || provider.maxInputBytes != DefaultMaxInputBytes {
			t.Fatal("configuration was not detached or endpoint was guessed")
		}
	}
}

func TestCompatibleRejectsInvalidConfigurationBeforeNetwork(t *testing.T) {
	t.Setenv(testKeyEnv, testKey)
	for _, test := range []struct {
		name   string
		update func(*Config)
	}{
		{"relative", func(c *Config) { c.BaseURL = "/v1" }},
		{"userinfo", func(c *Config) { c.BaseURL = "https://user:password@example.test" }},
		{"query", func(c *Config) { c.BaseURL += "?api_key=secret" }},
		{"empty query", func(c *Config) { c.BaseURL += "?" }},
		{"fragment", func(c *Config) { c.BaseURL += "#secret" }},
		{"empty fragment", func(c *Config) { c.BaseURL += "#" }},
		{"plain HTTP", func(c *Config) { c.BaseURL = "http://127.0.0.1" }},
		{"public HTTP", func(c *Config) { c.BaseURL = "http://example.test"; c.AllowHTTP = true }},
		{"scheme", func(c *Config) { c.BaseURL = "ftp://example.test" }},
		{"zero port", func(c *Config) { c.BaseURL = "https://example.test:0" }},
		{"overflow port", func(c *Config) { c.BaseURL = "https://example.test:65536" }},
		{"empty port", func(c *Config) { c.BaseURL = "https://example.test:" }},
		{"bad URL", func(c *Config) { c.BaseURL = "https://example.test/%zz" }},
		{"unbracketed IPv6", func(c *Config) { c.BaseURL = "https://::1" }},
		{"bracketed DNS name", func(c *Config) { c.BaseURL = "https://[example.test]" }},
		{"empty hostname label", func(c *Config) { c.BaseURL = "https://example..test" }},
		{"empty model", func(c *Config) { c.Model = "" }},
		{"bad model", func(c *Config) { c.Model = "model\nsecret" }},
		{"key as env name", func(c *Config) { c.APIKeyEnv = "sk-raw-secret" }},
		{"empty env name", func(c *Config) { c.APIKeyEnv = "" }},
		{"negative timeout", func(c *Config) { c.Timeout = -1 }},
		{"long timeout", func(c *Config) { c.Timeout = 11 * time.Minute }},
		{"unknown token field", func(c *Config) { c.OutputTokenField = "max_output_tokens" }},
		{"negative input", func(c *Config) { c.MaxInputBytes = -1 }},
		{"large input", func(c *Config) { c.MaxInputBytes = HardMaxInputBytes + 1 }},
		{"negative response", func(c *Config) { c.MaxResponseBytes = -1 }},
		{"large response", func(c *Config) { c.MaxResponseBytes = HardMaxResponseBytes + 1 }},
		{"negative output", func(c *Config) { c.MaxOutputTokens = -1 }},
		{"large output", func(c *Config) { c.MaxOutputTokens = HardMaxOutputTokens + 1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := Config{BaseURL: "https://example.test/v1", Model: "test", APIKeyEnv: testKeyEnv}
			test.update(&config)
			if _, err := New(config); !errors.Is(err, ErrConfig) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestCompatibleRejectsMissingAndUnsafeCredentials(t *testing.T) {
	for _, key := range []string{"", "secret\r\nX-Leak: true", " leading", "nonascii-密钥"} {
		t.Setenv(testKeyEnv, key)
		if _, err := New(Config{BaseURL: "https://example.test", Model: "test", APIKeyEnv: testKeyEnv}); !errors.Is(err, ErrCredentials) || strings.Contains(err.Error(), "X-Leak") {
			t.Fatalf("error=%v", err)
		}
	}
}

func TestModelRedirectDoesNotForwardAPIKey(t *testing.T) {
	var reached atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		writeJSON(w, validResponse)
	}))
	defer destination.Close()
	provider, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", destination.URL+"/?secret="+testKey)
		w.WriteHeader(http.StatusTemporaryRedirect)
		_, _ = io.WriteString(w, "vendor echoed "+testKey)
	}, nil)
	_, err := provider.Complete(context.Background(), testMessages(), 12)
	if !errors.Is(err, ErrRedirect) || reached.Load() != 0 || strings.Contains(err.Error(), testKey) || strings.Contains(err.Error(), destination.URL) {
		t.Fatalf("error=%v destination requests=%d", err, reached.Load())
	}
}

func TestCompatibleHTTPFailuresDoNotRetryOrLeak(t *testing.T) {
	for _, test := range []struct {
		status int
		want   error
	}{{401, ErrUnauthorized}, {403, ErrUnauthorized}, {429, ErrRateLimit}, {500, ErrHTTP}, {204, ErrHTTP}} {
		t.Run(fmt.Sprint(test.status), func(t *testing.T) {
			var calls atomic.Int32
			provider, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, testKey+testMessages()[1].Content+"vendor-private-error")
			}, nil)
			_, err := provider.Complete(context.Background(), testMessages(), 12)
			if !errors.Is(err, test.want) || calls.Load() != 1 {
				t.Fatalf("error=%v calls=%d", err, calls.Load())
			}
			for _, secret := range []string{testKey, testMessages()[1].Content, "vendor-private-error"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatal("error leaked private data")
				}
			}
		})
	}
}

func TestCompatibleStrictResponseValidation(t *testing.T) {
	withoutUsage := strings.Split(validResponse, `,"usage"`)[0] + "}"
	for _, test := range []struct {
		name string
		body string
		want error
	}{
		{"no usage", withoutUsage, nil},
		{"usage without total", strings.ReplaceAll(validResponse, `,"total_tokens":10`, ""), nil},
		{"null optional message metadata", strings.ReplaceAll(validResponse, `"role":"assistant"`, `"role":"assistant","refusal":null,"tool_calls":null`), nil},
		{"bad JSON", `{"echo":"` + testKey, ErrInvalidResponse},
		{"null", "null", ErrInvalidResponse},
		{"array", "[]", ErrInvalidResponse},
		{"empty choices", `{"choices":[]}`, ErrInvalidResponse},
		{"multiple choices", `{"choices":[{},{}]}`, ErrInvalidResponse},
		{"null choice", `{"choices":[null]}`, ErrInvalidResponse},
		{"missing finish", strings.ReplaceAll(validResponse, `"finish_reason":"stop",`, ""), ErrInvalidResponse},
		{"wrong role", strings.ReplaceAll(validResponse, `"assistant"`, `"system"`), ErrInvalidResponse},
		{"null content", strings.ReplaceAll(validResponse, `"content":"{\"tool\":\"finish\"}"`, `"content":null`), ErrInvalidResponse},
		{"missing content", strings.ReplaceAll(validResponse, `,"content":"{\"tool\":\"finish\"}"`, ""), ErrInvalidResponse},
		{"array content", strings.ReplaceAll(validResponse, `"content":"{\"tool\":\"finish\"}"`, `"content":[]`), ErrInvalidResponse},
		{"empty content", strings.ReplaceAll(validResponse, `"content":"{\"tool\":\"finish\"}"`, `"content":"  "`), ErrInvalidResponse},
		{"truncated", strings.ReplaceAll(validResponse, `"stop"`, `"length"`), ErrTruncated},
		{"filter", strings.ReplaceAll(validResponse, `"stop"`, `"content_filter"`), ErrRefusal},
		{"refusal", strings.ReplaceAll(validResponse, `"role":"assistant"`, `"role":"assistant","refusal":"private refusal"`), ErrRefusal},
		{"tool calls", strings.ReplaceAll(validResponse, `"role":"assistant"`, `"role":"assistant","tool_calls":[{"type":"function"}]`), ErrToolCall},
		{"tool finish", strings.ReplaceAll(validResponse, `"stop"`, `"tool_calls"`), ErrToolCall},
		{"legacy function", strings.ReplaceAll(validResponse, `"role":"assistant"`, `"role":"assistant","function_call":{}`), ErrToolCall},
		{"wrong index", strings.ReplaceAll(validResponse, `"index":0`, `"index":1`), ErrInvalidResponse},
		{"trailing JSON", validResponse + " {}", ErrInvalidResponse},
		{"duplicate field", strings.ReplaceAll(validResponse, `"role":"assistant"`, `"role":"system","role":"assistant"`), ErrInvalidResponse},
		{"negative usage", strings.ReplaceAll(validResponse, `"prompt_tokens":7`, `"prompt_tokens":-7`), ErrInvalidResponse},
		{"negative output usage", strings.ReplaceAll(validResponse, `"completion_tokens":3`, `"completion_tokens":-3`), ErrInvalidResponse},
		{"overflow usage", strings.ReplaceAll(validResponse, `"prompt_tokens":7`, `"prompt_tokens":9223372036854775808`), ErrInvalidResponse},
		{"overflow sum", strings.ReplaceAll(validResponse, `"prompt_tokens":7`, `"prompt_tokens":9223372036854775807`), ErrInvalidResponse},
		{"fractional usage", strings.ReplaceAll(validResponse, `"prompt_tokens":7`, `"prompt_tokens":1.1`), ErrInvalidResponse},
		{"quoted usage", strings.ReplaceAll(validResponse, `"prompt_tokens":7`, `"prompt_tokens":"7"`), ErrInvalidResponse},
		{"null usage count", strings.ReplaceAll(validResponse, `"prompt_tokens":7`, `"prompt_tokens":null`), ErrInvalidResponse},
		{"missing usage count", strings.ReplaceAll(validResponse, `"prompt_tokens":7,`, ""), ErrInvalidResponse},
		{"null usage", strings.ReplaceAll(validResponse, `{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}`, `null`), ErrInvalidResponse},
		{"null total", strings.ReplaceAll(validResponse, `"total_tokens":10`, `"total_tokens":null`), ErrInvalidResponse},
		{"inconsistent total", strings.ReplaceAll(validResponse, `"total_tokens":10`, `"total_tokens":9`), ErrInvalidResponse},
		{"deep extra data", strings.TrimSuffix(validResponse, "}") + `,"extra":` + strings.Repeat("[", 66) + "0" + strings.Repeat("]", 66) + "}", ErrInvalidResponse},
		{"invalid UTF8", strings.ReplaceAll(validResponse, "finish\\", "fi\xffnish\\"), ErrInvalidResponse},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, test.body) }, nil)
			reply, err := provider.Complete(context.Background(), testMessages(), 12)
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v want=%v", err, test.want)
			}
			if test.want != nil && reply != (Reply{}) {
				t.Fatal("invalid response returned usable text")
			}
			if test.name == "no usage" && reply.Usage.Known {
				t.Fatal("missing usage reported as known")
			}
		})
	}
}

func TestCompatibleInputAndTokenLimitsPreventRequests(t *testing.T) {
	var calls atomic.Int32
	provider, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(w, validResponse)
	}, func(c *Config) { c.MaxInputBytes = 200 })
	for _, test := range []struct {
		messages []Message
		tokens   int
		want     error
	}{
		{nil, 12, ErrInput},
		{[]Message{{Role: "tool", Content: "tool"}}, 12, ErrInput},
		{[]Message{{Role: "user"}}, 12, ErrInput},
		{[]Message{{Role: "user", Content: "bad\xff"}}, 12, ErrInput},
		{testMessages(), 0, ErrOutputLimit},
		{testMessages(), DefaultMaxOutputTokens + 1, ErrOutputLimit},
		{[]Message{{Role: "user", Content: strings.Repeat("x", 201)}}, 12, ErrInputLimit},
		// Raw text fits, but its escaped JSON representation exceeds the cap.
		{[]Message{{Role: "user", Content: strings.Repeat("\x00", 30)}}, 12, ErrInputLimit},
	} {
		if _, err := provider.Complete(context.Background(), test.messages, test.tokens); !errors.Is(err, test.want) {
			t.Fatalf("error=%v want=%v", err, test.want)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid input made %d requests", calls.Load())
	}
}

func TestCompatibleResponseBoundsAndMediaType(t *testing.T) {
	for _, test := range []struct {
		name    string
		handler http.HandlerFunc
		limit   int64
		want    error
	}{
		{"declared oversized", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "9999999")
			w.WriteHeader(http.StatusOK)
		}, 512, ErrResponseLimit},
		{"chunked oversized", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.(http.Flusher).Flush()
			_, _ = io.WriteString(w, strings.Repeat(" ", 513))
		}, 512, ErrResponseLimit},
		{"exact boundary", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, validResponse) }, int64(len(validResponse)), nil},
		{"one byte below boundary", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, validResponse) }, int64(len(validResponse) - 1), ErrResponseLimit},
		{"HTML body", func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "<html>private vendor body</html>")
		}, 512, ErrInvalidResponse},
		{"JSON wrong media", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, validResponse)
		}, 512, ErrInvalidResponse},
		{"compressed response", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Encoding", "gzip")
			writeJSON(w, validResponse)
		}, 512, ErrInvalidResponse},
		{"partial body", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "500")
			writeJSON(w, validResponse)
		}, 512, ErrTransport},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider, _ := newTestProvider(t, test.handler, func(c *Config) { c.MaxResponseBytes = test.limit })
			if _, err := provider.Complete(context.Background(), testMessages(), 12); !errors.Is(err, test.want) {
				t.Fatalf("error=%v want=%v", err, test.want)
			}
		})
	}
}

func TestCompatibleCancellationAndTimeout(t *testing.T) {
	for _, phase := range []string{"before request", "waiting headers", "reading body", "client timeout"} {
		t.Run(phase, func(t *testing.T) {
			started := make(chan struct{})
			provider, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
				// Drain the POST so the server's disconnect watcher can observe
				// cancellation while the handler deliberately stalls its response.
				_, _ = io.Copy(io.Discard, r.Body)
				_ = r.Body.Close()
				if phase == "reading body" {
					w.Header().Set("Content-Type", "application/json")
					w.(http.Flusher).Flush()
				}
				close(started)
				<-r.Context().Done()
			}, func(c *Config) {
				if phase == "client timeout" {
					c.Timeout = 30 * time.Millisecond
				}
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if phase == "before request" {
				cancel()
			} else if phase != "client timeout" {
				go func() { <-started; cancel() }()
			}
			_, err := provider.Complete(ctx, testMessages(), 12)
			want, contextWant := ErrCanceled, context.Canceled
			if phase == "client timeout" {
				want, contextWant = ErrTimeout, context.DeadlineExceeded
			}
			if !errors.Is(err, want) || !errors.Is(err, contextWant) {
				t.Fatalf("error=%v want=%v", err, want)
			}
		})
	}
}

func TestCompatibleTransportIsPrivateAndDoesNotInheritProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://proxy.invalid:1")
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid:1")
	provider, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, validResponse) }, nil)
	transport, ok := provider.client.Transport.(*http.Transport)
	if !ok || transport == http.DefaultTransport || transport.Proxy != nil || !transport.DisableKeepAlives || !transport.DisableCompression {
		t.Fatal("transport inherits settings or permits implicit retries/decompression")
	}
	if _, err := provider.Complete(context.Background(), testMessages(), 12); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"%v", "%+v", "%#v"} {
		if strings.Contains(fmt.Sprintf(format, provider), testKey) || strings.Contains(fmt.Sprintf(format, *provider), testKey) {
			t.Fatalf("client formatting with %s exposed its credential", format)
		}
	}
}

func TestCompatibleTransportErrorDoesNotLeakEndpoint(t *testing.T) {
	provider, server := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {}, nil)
	server.Close()
	_, err := provider.Complete(context.Background(), testMessages(), 12)
	if !errors.Is(err, ErrTransport) || strings.Contains(err.Error(), server.URL) || strings.Contains(err.Error(), testKey) {
		t.Fatalf("error=%v", err)
	}
}

func TestCompatibleConcurrentRequestsRemainIndependent(t *testing.T) {
	var requests atomic.Int32
	provider, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		var request completionRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		requests.Add(1)
		body, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
			"finish_reason": "stop", "message": map[string]string{"role": "assistant", "content": request.Messages[0].Content},
		}}})
		writeJSON(w, string(body))
	}, nil)
	const count = 12
	var group sync.WaitGroup
	for i := 0; i < count; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			content := fmt.Sprintf("call-%d", i)
			reply, err := provider.Complete(context.Background(), []Message{{Role: "user", Content: content}}, 12)
			if err != nil || reply.Content != content {
				t.Errorf("response crossed requests: content=%q reply=%+v error=%v", content, reply, err)
			}
		}(i)
	}
	group.Wait()
	if requests.Load() != count {
		t.Fatalf("requests=%d want=%d", requests.Load(), count)
	}
}
