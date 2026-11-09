// Package webaccess provides anonymous, bounded website access for analysis.
// It does not share transports, credentials or browser profiles with production.
package webaccess

import (
	"errors"
	"net"
	"net/netip"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/egress"
)

var (
	ErrPolicy              = errors.New("webaccess: destination denied")
	ErrBudget              = errors.New("webaccess: budget exhausted")
	ErrRobots              = errors.New("webaccess: robots denied or unavailable")
	ErrTransport           = errors.New("webaccess: transport failed")
	ErrCanceled            = errors.New("webaccess: canceled or deadline exceeded")
	ErrAudit               = errors.New("webaccess: accounting rejected")
	ErrRendererUnavailable = errors.New("webaccess: controlled renderer unavailable")
)

const UserAgent = "PapaSiteAgent/1.0"

// Origin authorizes one exact scheme/host/port. Empty PathPrefixes allows all
// paths; /a matches /a and /a/... but never /ab. The host alone can opt a
// particular origin into private addressing, for example a local test server.
type Origin struct {
	URL                 string
	PathPrefixes        []string
	AllowPrivateNetwork bool
}

type Policy struct {
	// RespectRobots opts into strict robots.txt checks. Default false performs no robots request.
	RespectRobots   bool
	Origins         []Origin
	ResourceOrigins []Origin
	MinInterval     time.Duration
}

type Budgets struct {
	MaxPages         int
	MaxRequests      int
	MaxResponseBytes int64
	MaxTotalBytes    int64
	MaxDuration      time.Duration
	MaxRedirects     int
}

type allowedOrigin struct {
	prefixes []string
	private  bool
}

// CanonicalURL returns the URL spelling used by this package's transport.
// It validates URL syntax, not access authorization or the destination's DNS;
// callers must still use a Session to perform guarded network operations.
func CanonicalURL(raw string) (string, error) {
	u, _, err := normalize(raw)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

func normalize(raw string) (*url.URL, string, error) {
	if strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, "\\\r\n\t") {
		return nil, "", ErrPolicy
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, "", ErrPolicy
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || strings.HasSuffix(host, ".") || strings.ContainsAny(host, "%\x00 ") {
		return nil, "", ErrPolicy
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Zone() != "" {
			return nil, "", ErrPolicy
		}
		host = ip.String()
	} else {
		if len(host) > 253 || strings.Contains(host, ":") {
			return nil, "", ErrPolicy
		}
		allNumeric := true
		for _, label := range strings.Split(host, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return nil, "", ErrPolicy
			}
			for _, r := range label {
				if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
					return nil, "", ErrPolicy
				}
				if r < '0' || r > '9' {
					allNumeric = false
				}
			}
		}
		// Reject legacy numeric address spellings (127.1, 2130706433).
		if allNumeric || strings.HasPrefix(host, "0x") {
			return nil, "", ErrPolicy
		}
	}
	port := u.Port()
	if port == "" {
		if strings.HasSuffix(u.Host, ":") {
			return nil, "", ErrPolicy
		}
		if u.Scheme == "http" {
			port = "80"
		} else {
			port = "443"
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return nil, "", ErrPolicy
	}
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		if strings.Contains(host, ":") {
			u.Host = "[" + host + "]"
		} else {
			u.Host = host
		}
	} else {
		u.Host = net.JoinHostPort(host, port)
	}
	if u.Path == "" {
		u.Path = "/"
	}
	// Do not let a proxy/server decode a second path or interpret encoded
	// separators differently from our policy. Query is preserved as data.
	escaped := strings.ToLower(u.EscapedPath())
	if strings.Contains(escaped, "%2f") || strings.Contains(escaped, "%5c") || strings.Contains(u.Path, "%") || strings.ContainsRune(u.Path, 0) || path.Clean(u.Path) != strings.TrimSuffix(u.Path, "/") && u.Path != "/" {
		return nil, "", ErrPolicy
	}
	u.RawPath = ""
	u.Fragment = ""
	u.RawFragment = ""
	return u, u.Scheme + "://" + u.Host, nil
}

func compileOrigins(origins []Origin) (map[string]allowedOrigin, error) {
	out := make(map[string]allowedOrigin, len(origins))
	for _, o := range origins {
		u, key, err := normalize(o.URL)
		if err != nil || u.Path != "/" || u.RawQuery != "" || strings.Contains(o.URL, "#") {
			return nil, ErrPolicy
		}
		if _, ok := out[key]; ok {
			return nil, ErrPolicy
		}
		a := allowedOrigin{private: o.AllowPrivateNetwork, prefixes: make([]string, len(o.PathPrefixes))}
		for i, p := range o.PathPrefixes {
			if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "%\\?#\x00") || path.Clean(p) != strings.TrimSuffix(p, "/") && p != "/" {
				return nil, ErrPolicy
			}
			a.prefixes[i] = strings.TrimSuffix(p, "/")
		}
		out[key] = a
	}
	return out, nil
}

func pathAllowed(a allowedOrigin, p string) bool {
	if len(a.prefixes) == 0 {
		return true
	}
	for _, prefix := range a.prefixes {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}

func (s *Session) target(raw string, resource bool) (*url.URL, allowedOrigin, error) {
	if s.checkURL != nil && s.checkURL(raw) != nil {
		return nil, allowedOrigin{}, ErrPolicy
	}
	u, key, err := normalize(raw)
	if err != nil {
		return nil, allowedOrigin{}, err
	}
	if a, ok := s.origins[key]; ok && pathAllowed(a, u.Path) {
		return u, a, nil
	}
	if resource {
		if a, ok := s.resources[key]; ok && pathAllowed(a, u.Path) {
			return u, a, nil
		}
	}
	return nil, allowedOrigin{}, ErrPolicy
}

func publicIP(ip netip.Addr) bool { return egress.PublicIP(ip) }

func defaults(b Budgets) (Budgets, error) {
	if b.MaxPages == 0 {
		b.MaxPages = 8
	}
	if b.MaxRequests == 0 {
		b.MaxRequests = 128
	}
	if b.MaxResponseBytes == 0 {
		b.MaxResponseBytes = 2 << 20
	}
	if b.MaxTotalBytes == 0 {
		b.MaxTotalBytes = 16 << 20
	}
	if b.MaxDuration == 0 {
		b.MaxDuration = 180 * time.Second
	}
	if b.MaxRedirects == 0 {
		b.MaxRedirects = 8
	}
	if b.MaxPages < 1 || b.MaxPages > 32 || b.MaxRequests < 1 || b.MaxRequests > 512 || b.MaxResponseBytes < 1 || b.MaxResponseBytes > 8<<20 || b.MaxTotalBytes < 1 || b.MaxTotalBytes > 64<<20 || b.MaxDuration < 0 || b.MaxDuration > 600*time.Second || b.MaxRedirects < 1 || b.MaxRedirects > 16 {
		return b, ErrBudget
	}
	return b, nil
}
