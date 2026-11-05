// Package redact masks known credential shapes in diagnostic copies only.
// It must never be used to rewrite a request URL, task identity or resume data.
package redact

import (
	"bytes"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
)

const Mask = "***"

func SensitiveKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "token", "sign", "sig", "key", "api_key", "apikey", "x-api-key", "access_token", "session", "sessionid", "secret", "password", "passwd", "authorization", "proxy-authorization", "cookie", "set-cookie", "dsn":
		return true
	}
	return false
}

// URL preserves non-sensitive escaped path/query spelling and query order.
func URL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		// Failed requests often contain malformed escapes/ports. Remove known
		// authority credentials even when net/url cannot parse the whole URL.
		if scheme := strings.Index(raw, "://"); scheme >= 0 {
			start := scheme + 3
			end := len(raw)
			if n := strings.IndexAny(raw[start:], "/?#"); n >= 0 {
				end = start + n
			}
			if at := strings.LastIndex(raw[start:end], "@"); at >= 0 {
				raw = raw[:start] + raw[start+at+1:]
			}
		}
		if path, query, ok := strings.Cut(raw, "?"); ok {
			q, fragment, hasFragment := strings.Cut(query, "#")
			raw = path + "?" + maskQuery(q)
			if hasFragment {
				raw += "#" + fragment
			}
		}
		return assignments(raw)
	}
	u.User = nil
	u.RawQuery = maskQuery(u.RawQuery)
	return u.String()
}

func maskQuery(query string) string {
	parts := strings.Split(query, "&")
	for i, part := range parts {
		k, _, _ := strings.Cut(part, "=")
		decoded, err := url.QueryUnescape(k)
		if err == nil && SensitiveKey(decoded) {
			parts[i] = k + "=" + Mask
		}
	}
	return strings.Join(parts, "&")
}

var urls = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^\s<>"']+`)

// Userinfo may contain a literal apostrophe. Strip the authority credentials
// before using quotes as URL boundaries in surrounding diagnostic prose.
var urlUserinfo = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/\s?#<>]+@`)

// MySQL passwords may contain '@'; the delimiter is the final @tcp/@unix.
var mysqlDSN = regexp.MustCompile(`[^\s:"'=]+:[^\s]*@(?:tcp|unix)\([^)]*\)/[^\s"']*`)
var headers = regexp.MustCompile(`(?im)\b(?:authorization|proxy-authorization|cookie|set-cookie)\s*:\s*[^\r\n]+`)
var bearer = regexp.MustCompile(`(?i)\b((?:authorization|proxy-authorization)\s*=\s*)(?:bearer|basic)\s+[^\s,;"']+`)

// Diagnostic strings can already be truncated. A missing closing quote must
// not make the remaining credential value invisible to the matcher.
var pairs = regexp.MustCompile(`(?i)(\b(?:token|sign|sig|key|api_key|apikey|x-api-key|access_token|session|sessionid|secret|password|passwd|authorization|proxy-authorization|cookie|set-cookie|dsn)\b["']?\s*[:=]\s*)("(?:\\.|[^"\\])*"?|'(?:\\.|[^'\\])*'?|[^\s&,;}\]"']+)`)

func assignments(s string) string {
	return pairs.ReplaceAllStringFunc(s, func(match string) string {
		m := pairs.FindStringSubmatch(match)
		value := Mask
		if len(m[2]) > 0 && (m[2][0] == '"' || m[2][0] == '\'') {
			value = string(m[2][0]) + Mask + string(m[2][0])
		}
		return m[1] + value
	})
}

// Text handles URLs, known key/value shapes, credential headers and MySQL DSNs.
// Arbitrary secrets in prose, HTML, SQL literals or unknown keys are out of scope.
func Text(s string) string {
	s = urlUserinfo.ReplaceAllString(s, "${1}")
	s = urls.ReplaceAllStringFunc(s, URL)
	s = mysqlDSN.ReplaceAllString(s, "[redacted-dsn]")
	s = headers.ReplaceAllStringFunc(s, func(h string) string { k, _, _ := strings.Cut(h, ":"); return k + ": " + Mask })
	s = bearer.ReplaceAllString(s, "${1}"+Mask)
	return assignments(s)
}

// JSON serializes and masks a detached diagnostic copy. Numbers retain precision.
func JSON(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var copy any
	if err = dec.Decode(&copy); err != nil {
		return nil, err
	}
	return json.Marshal(clean(copy, "", false))
}
func clean(value any, key string, credentials bool) any {
	// A schema's "key" is often a site/section identifier. Only treat
	// ambiguous names as credentials inside diagnostic attribute bags.
	ambiguous := strings.EqualFold(key, "key") || strings.EqualFold(key, "sign") || strings.EqualFold(key, "sig")
	if SensitiveKey(key) && (!ambiguous || credentials) {
		return Mask
	}
	switch v := value.(type) {
	case string:
		return Text(v)
	case map[string]any:
		switch strings.ToLower(key) {
		case "attrs", "fields", "headers", "environment":
			credentials = true
		}
		for k, item := range v {
			v[k] = clean(item, k, credentials)
		}
	case []any:
		for i, item := range v {
			v[i] = clean(item, key, credentials)
		}
	}
	return value
}
