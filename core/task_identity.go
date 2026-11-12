package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// TaskIdentity is the framework-owned, scoped identity of one task.
// Value is diagnostic data; Hash is SHA-256 of an unambiguous versioned tuple.
type TaskIdentity struct {
	Type  string
	Value string
	Hash  string
}

// NormalizeTaskIdentity prefers an explicit stable resource ID; otherwise it
// uses an origin-preserving HTTP URL. ID and URL occupy separate namespaces.
func NormalizeTaskIdentity(site, stage, resourceID, rawURL string) (TaskIdentity, error) {
	if strings.TrimSpace(site) == "" || strings.TrimSpace(stage) == "" {
		return TaskIdentity{}, fmt.Errorf("task identity requires site and stage")
	}
	kind, value := "id", resourceID
	if resourceID == "" {
		kind = "url"
		var err error
		value, err = CanonicalTaskURL(rawURL)
		if err != nil {
			return TaskIdentity{}, err
		}
	}
	b, _ := json.Marshal([]string{"papa-task-v1", site, stage, kind, value})
	h := sha256.Sum256(b)
	return TaskIdentity{Type: kind, Value: value, Hash: hex.EncodeToString(h[:])}, nil
}

// CanonicalTaskURL preserves path escaping and raw query order/value. It never
// removes a host or tracking parameter, since these can identify distinct work.
func CanonicalTaskURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("task URL: %w", err)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return "", fmt.Errorf("task URL must be absolute HTTP(S) without userinfo")
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	} else {
		u.Host = host
	}
	if u.Path == "" {
		u.Path = "/"
	}
	u.Fragment, u.RawFragment = "", ""
	return u.String(), nil
}
