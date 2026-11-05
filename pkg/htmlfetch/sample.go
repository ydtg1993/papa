package htmlfetch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// SamplePolicy makes retained data explicit. Defaults omit all headers, query
// parameters and bodies. Allowlisted values and URL paths can still be sensitive.
type SamplePolicy struct {
	Headers   []string
	QueryKeys []string
	// RedactURL runs after userinfo/fragment removal and query filtering.
	RedactURL func(string) (string, error)
	// RedactBody receives a private copy. Nil omits the body. request distinguishes sides.
	RedactBody func(request bool, body []byte) ([]byte, error)
}

type SampleMessage struct {
	URL         string      `json:"url"`
	Method      string      `json:"method,omitempty"`
	StatusCode  int         `json:"status_code,omitempty"`
	Header      http.Header `json:"headers,omitempty"`
	Body        []byte      `json:"body,omitempty"`
	BodyOmitted bool        `json:"body_omitted"`
}

// HTTPSample is a sanitized, versioned exchange for offline parser tests.
// It is not executable and cannot restore credentials or browser state.
type HTTPSample struct {
	Version  int           `json:"version"`
	Request  SampleMessage `json:"request"`
	Response SampleMessage `json:"response"`
}

// WriteSample fully sanitizes before writing. It does not open files or send requests.
// Writer failures can leave partial output; callers own atomic file publication.
func WriteSample(w io.Writer, request Request, response *Response, policy SamplePolicy) error {
	if response == nil || response.URL == nil {
		return fmt.Errorf("sample: response URL required")
	}
	requestHeaders := make(http.Header)
	for k, v := range request.Headers {
		requestHeaders.Set(k, v)
	}
	method := request.Method
	if method == "" {
		method = http.MethodGet
	}
	s := HTTPSample{Version: 1, Request: SampleMessage{URL: request.URL, Method: method, Header: requestHeaders, Body: request.Body}, Response: SampleMessage{URL: response.URL.String(), StatusCode: response.StatusCode, Header: response.Header, Body: response.Body}}
	for i, m := range []*SampleMessage{&s.Request, &s.Response} {
		u, err := url.Parse(m.URL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("sample: invalid HTTP URL")
		}
		q, err := url.ParseQuery(u.RawQuery)
		if err != nil {
			return fmt.Errorf("sample: invalid URL query")
		}
		filtered := make(url.Values)
		for _, key := range policy.QueryKeys {
			if values, ok := q[key]; ok {
				filtered[key] = values
			}
		}
		u.User = nil
		u.Fragment = ""
		u.RawFragment = ""
		u.RawQuery = filtered.Encode()
		u.ForceQuery = false
		m.URL = u.String()
		if policy.RedactURL != nil {
			m.URL, err = policy.RedactURL(m.URL)
			if err != nil {
				return fmt.Errorf("sample: URL redaction failed")
			}
		}
		headers := make(http.Header)
		for _, key := range policy.Headers {
			// Never export common credential headers, even if accidentally allowlisted.
			switch strings.ToLower(key) {
			case "authorization", "proxy-authorization", "cookie", "set-cookie":
				continue
			}
			if values := m.Header.Values(key); len(values) > 0 {
				headers[http.CanonicalHeaderKey(key)] = append([]string(nil), values...)
			}
		}
		m.Header = headers
		if policy.RedactBody == nil {
			m.Body = nil
			m.BodyOmitted = true
		} else {
			body, err := policy.RedactBody(i == 0, bytes.Clone(m.Body))
			if err != nil {
				return fmt.Errorf("sample: body redaction failed")
			}
			m.Body = body
		}
	}
	return json.NewEncoder(w).Encode(s)
}

// ReadSample reads at most maxBytes (default 16 MiB), rejecting trailing data and
// unsupported versions. Body is decoded from JSON base64, not fetched from URL.
func ReadSample(r io.Reader, maxBytes int64) (*HTTPSample, error) {
	if maxBytes <= 0 {
		maxBytes = 16 << 20
	}
	data, err := io.ReadAll(io.LimitReader(r, maxBytes))
	if err != nil {
		return nil, err
	}
	var extra [1]byte
	if n, err := io.ReadFull(r, extra[:]); n > 0 {
		return nil, fmt.Errorf("sample exceeds %d bytes", maxBytes)
	} else if err != io.EOF {
		return nil, err
	}
	var sample HTTPSample
	if err := DecodeJSON(data, &sample); err != nil {
		return nil, err
	}
	if sample.Version != 1 {
		return nil, fmt.Errorf("unsupported sample version %d", sample.Version)
	}
	return &sample, nil
}

// OfflineResponse returns a detached response for DecodeJSON/HTML parser tests.
// An omitted body is an error, not an empty successful response.
func (s *HTTPSample) OfflineResponse() (*Response, error) {
	if s == nil || s.Version != 1 {
		return nil, fmt.Errorf("unsupported sample")
	}
	if s.Response.BodyOmitted {
		return nil, fmt.Errorf("sample response body was omitted")
	}
	u, err := url.Parse(s.Response.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("sample response URL is invalid")
	}
	return &Response{URL: u, StatusCode: s.Response.StatusCode, Header: s.Response.Header.Clone(), Body: bytes.Clone(s.Response.Body)}, nil
}
