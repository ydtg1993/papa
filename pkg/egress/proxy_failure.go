package egress

import (
	"context"
	"net"
)

// ProxyFailureError identifies a failure of the upstream proxy itself. Errors
// from the destination and its HTTP status codes do not use this type. Its error
// text deliberately omits addresses and credentials.
type ProxyFailureError struct {
	kind string
	err  error
}

func (e *ProxyFailureError) Error() string        { return "egress: proxy " + e.kind + " failure" }
func (e *ProxyFailureError) Unwrap() error        { return e.err }
func (e *ProxyFailureError) ProxyFailure() string { return e.kind }

func proxyFailure(ctx context.Context, kind string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return &ProxyFailureError{kind: kind, err: err}
}

// The SOCKS library uses this dialer only for its connection to the upstream.
// Target connection denials from the SOCKS handshake remain unclassified.
type proxyDialer struct{ base *net.Dialer }

func (d proxyDialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

func (d proxyDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	c, err := d.base.DialContext(ctx, network, address)
	if err != nil {
		return nil, proxyFailure(ctx, "transport", err)
	}
	return c, nil
}
