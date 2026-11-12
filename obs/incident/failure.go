package incident

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	"github.com/ydtg1993/papa/v4/pkg/browser/identity"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

// Failure is structured trigger evidence, separate from report hypotheses.
// Fingerprint is an opaque stable digest; Code/Phase remain readable metadata.
type Failure struct {
	Category    string `json:"category"`
	Fingerprint string `json:"fingerprint"`
	Code        string `json:"code"`
	Phase       string `json:"phase"`
	Semantic    bool   `json:"semantic"`
	HTTPStatus  int    `json:"http_status,omitempty"`
}

func ClassifyError(err error) Failure {
	f := Failure{Category: "unknown"}
	if err == nil {
		return f
	}
	kind := core.ErrorKind(err)
	var dns *net.DNSError
	var op *net.OpError
	var cert x509.UnknownAuthorityError
	var certInvalid x509.CertificateInvalidError
	var hostname x509.HostnameError
	var netErr net.Error
	var wait *browser.WaitError
	var startup *identity.InitializationError
	switch {
	case kind == "structure":
		f.Category = "structure"
		f.Semantic = true
	case errors.As(err, &wait):
		f.Category = "structure"
		f.Phase = "wait_" + wait.Stage
		f.Semantic = true
	case errors.As(err, &startup):
		f.Category = "local"
		f.Code = "browser_start_" + startup.Stage
		f.Semantic = true
	case errors.Is(err, account.ErrExpired), errors.Is(err, account.ErrNeedsVerify), errors.Is(err, account.ErrForbidden), errors.Is(err, account.ErrChallenge):
		f.Category = "authentication"
		f.Semantic = true
	case errors.As(err, &dns):
		f.Category = "dns"
		f.Code = "dns"
	case errors.As(err, &cert), errors.As(err, &certInvalid), errors.As(err, &hostname):
		f.Category = "tls"
		f.Code = "certificate"
	case errors.As(err, &netErr) && netErr.Timeout():
		f.Category = "timeout"
		f.Code = "timeout"
	case errors.As(err, &op):
		f.Category = "connection"
		f.Code = op.Op
	case kind == "protected" || kind == "access_restricted" || kind == "site_challenge":
		f.Category = "access"
		f.Semantic = true
	case kind == "not_found" || kind == "not-found":
		f.Category = "not_found"
		f.Code = "not_found"
	case strings.HasPrefix(kind, "account") || kind == "authentication":
		f.Category = "authentication"
		f.Semantic = true
	case kind == "local" || kind == "database" || kind == "browser_start" || kind == "resource":
		f.Category = "local"
		f.Semantic = true
		f.Code = kind
	}
	if status, ok := htmlfetch.StatusCode(err); ok {
		f.HTTPStatus = status
		f.Code = fmt.Sprintf("http_%d", status)
		switch {
		case status == 429:
			f.Category = "rate_limit"
		case status == 401 || status == 403:
			f.Category = "authentication"
			f.Semantic = true
		case status == 404 || status == 410:
			f.Category = "not_found"
		case status >= 500:
			f.Category = "server"
		default:
			f.Category = "http"
		}
	}
	var stable interface{ FailureCode() (string, string) }
	if errors.As(err, &stable) {
		phase, code := stable.FailureCode()
		if strings.TrimSpace(code) != "" {
			f.Phase = clip(strings.TrimSpace(phase), 128)
			f.Code = clip(strings.TrimSpace(code), 256)
			f.Semantic = f.Semantic || f.Phase == "parse" || f.Phase == "extract" || f.Phase == "validate"
			if f.Category == "unknown" && f.Semantic {
				f.Category = "structure"
			}
		}
	}
	// Typed code wins; untyped messages use the existing bounded/redacted
	// template, retaining numeric status/error codes.
	if f.Code == "" {
		f.Code = Template(Clean(Sample{Error: err.Error()}).Error)
	}
	b, _ := json.Marshal([]string{f.Phase, f.Category, fmt.Sprint(f.HTTPStatus), f.Code})
	sum := sha256.Sum256(b)
	f.Fingerprint = hex.EncodeToString(sum[:])
	return f
}
