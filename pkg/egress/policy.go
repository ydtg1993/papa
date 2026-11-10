// Package egress enforces destination policy at framework-owned network exits.
package egress

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

var ErrDenied = errors.New("egress: destination denied")
var ErrTransport = errors.New("egress: unsupported transport")

// Config is immutable after construction. PrivateOrigins grants exact HTTP(S)
// origins access to non-public unicast addresses, never to other origins.
type Config struct {
	PrivateOrigins []string `mapstructure:"private_origins"`
	// AllowedOrigins, when nonempty, restricts all exits to these exact origins.
	// It does not grant access to private addresses; PrivateOrigins still applies.
	AllowedOrigins []string `mapstructure:"allowed_origins"`
}
type Policy struct {
	private map[string]bool
	allowed map[string]bool
	lookup  func(context.Context, string, string) ([]netip.Addr, error)
}

func New(cfg Config) (*Policy, error) {
	p := &Policy{private: map[string]bool{}, lookup: net.DefaultResolver.LookupNetIP}
	for _, raw := range cfg.PrivateOrigins {
		u, key, err := Normalize(raw)
		if err != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
			return nil, ErrDenied
		}
		p.private[key] = true
	}
	if len(cfg.AllowedOrigins) > 0 {
		p.allowed = make(map[string]bool, len(cfg.AllowedOrigins))
		for _, raw := range cfg.AllowedOrigins {
			u, key, err := Normalize(raw)
			if err != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
				return nil, ErrDenied
			}
			p.allowed[key] = true
		}
	}
	return p, nil
}
func (c Config) Validate() error { _, err := New(c); return err }

// Normalize rejects host spellings that browsers/proxies may interpret differently.
// Paths and query octets are preserved (signed URLs must not be rewritten).
func Normalize(raw string) (*url.URL, string, error) {
	if strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, "\\\r\n\t") {
		return nil, "", ErrDenied
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, "", ErrDenied
	}
	h := strings.ToLower(u.Hostname())
	if h == "" || strings.HasSuffix(h, ".") || strings.ContainsAny(h, "%\x00 ") {
		return nil, "", ErrDenied
	}
	if ip, e := netip.ParseAddr(h); e == nil {
		if ip.Zone() != "" {
			return nil, "", ErrDenied
		}
		h = ip.Unmap().String()
	} else {
		if len(h) > 253 || strings.Contains(h, ":") {
			return nil, "", ErrDenied
		}
		numeric := true
		for _, label := range strings.Split(h, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return nil, "", ErrDenied
			}
			numeric = numeric && numericHostLabel(label)
			for _, r := range label {
				if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
					return nil, "", ErrDenied
				}
			}
		}
		if numeric {
			return nil, "", ErrDenied
		}
	}
	port := u.Port()
	if port == "" {
		if strings.HasSuffix(u.Host, ":") {
			return nil, "", ErrDenied
		}
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	n, e := strconv.Atoi(port)
	if e != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return nil, "", ErrDenied
	}
	key := u.Scheme + "://" + net.JoinHostPort(h, port)
	// Normalize only host spelling, without changing escaped path/query.
	if port == "80" && u.Scheme == "http" || port == "443" && u.Scheme == "https" {
		u.Host = h
		if strings.Contains(h, ":") {
			u.Host = "[" + h + "]"
		}
	} else {
		u.Host = net.JoinHostPort(h, port)
	}
	return u, key, nil
}

func numericHostLabel(label string) bool {
	hex := strings.HasPrefix(label, "0x")
	if hex {
		label = label[2:]
	}
	if label == "" {
		return false
	}
	for _, c := range label {
		if !(c >= '0' && c <= '9' || hex && c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (p *Policy) Resolve(ctx context.Context, raw string) (*url.URL, []netip.Addr, error) {
	u, key, err := Normalize(raw)
	if err != nil {
		return nil, nil, err
	}
	if p.allowed != nil && !p.allowed[key] {
		return nil, nil, ErrDenied
	}
	var ips []netip.Addr
	if ip, e := netip.ParseAddr(u.Hostname()); e == nil {
		ips = []netip.Addr{ip}
	} else {
		ips, err = p.lookup(ctx, "ip", u.Hostname())
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			return nil, nil, ErrDenied
		}
	}
	if len(ips) == 0 {
		return nil, nil, ErrDenied
	}
	for i, ip := range ips {
		ip = ip.Unmap()
		ips[i] = ip
		if !usable(ip) || (!p.private[key] && !PublicIP(ip)) {
			return nil, nil, ErrDenied
		}
	}
	return u, ips, nil
}
func usable(ip netip.Addr) bool {
	return ip.IsValid() && ip.Zone() == "" && !ip.IsUnspecified() && !ip.IsMulticast() && (ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsLinkLocalUnicast())
}
func portOf(u *url.URL) string {
	if u.Port() != "" {
		return u.Port()
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}
