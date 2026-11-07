package frameworkdata

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
)

const maxInput = 65536
const maxText = 4096
const redacted = "[已隐藏敏感内容]"

var urlPattern = regexp.MustCompile(`https?://[^\s<>"']+`)
var secretPattern = regexp.MustCompile(`(?i)(bearer\s+)[^\s,;]+|((?:password|passwd|api[_-]?key|access[_-]?token|secret|authorization|cookie)\s*[:=]\s*)[^\s,;]+`)

func sensitive(key string) bool {
	key = strings.ToLower(strings.NewReplacer("_", "", "-", "", " ", "").Replace(key))
	for _, part := range []string{"password", "passwd", "secret", "apikey", "authorization", "cookie", "credential", "tokenhash", "accesstoken", "refreshtoken", "privatekey"} {
		if strings.Contains(key, part) {
			return true
		}
	}
	switch key {
	case "token", "headers", "requestbody", "responsebody", "body", "html":
		return true
	}
	return false
}

func sanitizeValue(key string, value any, depth int) any {
	if sensitive(key) {
		return redacted
	}
	if depth > 16 {
		return "[内容层级过深，已省略]"
	}
	switch v := value.(type) {
	case []byte:
		return sanitizeValue(key, string(v), depth)
	case json.RawMessage:
		return sanitizeValue(key, string(v), depth)
	case string:
		if len(v) > maxInput {
			return "[内容超过 64 KiB，已省略；请使用专用详情页]"
		}
		if strings.HasSuffix(key, "_json") || key == "meta" || key == "data" || key == "values" {
			var decoded any
			decoder := json.NewDecoder(strings.NewReader(v))
			decoder.UseNumber()
			if !json.Valid([]byte(v)) || decoder.Decode(&decoded) != nil {
				return "[无效 JSON，已省略]"
			}
			return sanitizeValue("", decoded, depth+1)
		}
		v = urlPattern.ReplaceAllStringFunc(v, func(raw string) string {
			u, err := url.Parse(raw)
			if err != nil {
				return "[URL 已隐藏]"
			}
			u.User = nil
			u.RawQuery = ""
			u.Fragment = ""
			u.RawFragment = ""
			u.ForceQuery = false
			return u.String()
		})
		v = secretPattern.ReplaceAllString(v, "$1$2"+redacted)
		runes := []rune(v)
		if len(runes) > maxText {
			return string(runes[:maxText]) + "…[已截断]"
		}
		return v
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = sanitizeValue(k, item, depth+1)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = sanitizeValue("", item, depth+1)
		}
		return out
	default:
		return value
	}
}
