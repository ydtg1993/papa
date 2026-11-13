package incident

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

func TestClassifyErrorUsesTypedEvidenceBeforeMessage(t *testing.T) {
	for _, tc := range []struct {
		err            error
		category, code string
		semantic       bool
	}{
		{fmt.Errorf("wrapped: %w", &htmlfetch.StatusError{Code: 429}), "rate_limit", "http_429", false},
		{&htmlfetch.StatusError{Code: 404}, "not_found", "http_404", false},
		{&htmlfetch.StatusError{Code: 503}, "server", "http_503", false},
		{&net.DNSError{Err: "no such host", Name: "fixture.invalid"}, "dns", "dns", false},
		{context.DeadlineExceeded, "timeout", "timeout", false},
		{fmt.Errorf("wrapped: %w", account.ErrExpired), "authentication", "", true},
		{&browser.WaitError{Stage: "selector", Err: context.DeadlineExceeded}, "structure", "", true},
		{errors.New("item 404000 missing"), "unknown", "", false},
	} {
		got := ClassifyError(tc.err)
		if got.Category != tc.category || got.Semantic != tc.semantic || tc.code != "" && got.Code != tc.code || len(got.Fingerprint) != 64 {
			t.Fatalf("%v -> %+v", tc.err, got)
		}
	}
}

func TestClassifyErrorStableCodeAndNumericFallback(t *testing.T) {
	a := ClassifyError(core.WithFailureCode(errors.New("missing title for https://fixture.invalid/one?token=private"), "parse", "title_missing"))
	b := ClassifyError(core.WithFailureCode(errors.New("new human message"), "parse", "title_missing"))
	c := ClassifyError(core.WithFailureCode(errors.New("missing price"), "parse", "price_missing"))
	if a.Fingerprint != b.Fingerprint || a.Fingerprint == c.Fingerprint || !a.Semantic || a.Category != "structure" {
		t.Fatalf("stable code: %+v %+v %+v", a, b, c)
	}
	a = ClassifyError(errors.New("remote returned 403"))
	b = ClassifyError(errors.New("remote returned 429"))
	if a.Fingerprint == b.Fingerprint {
		t.Fatal("fallback erased numeric error codes")
	}
	a = ClassifyError(core.WithFailureCode(&htmlfetch.StatusError{Code: 500}, "fetch", "upstream_failure"))
	b = ClassifyError(core.WithFailureCode(&htmlfetch.StatusError{Code: 503}, "fetch", "upstream_failure"))
	if a.Fingerprint == b.Fingerprint || a.HTTPStatus != 500 || b.HTTPStatus != 503 {
		t.Fatal("business code erased structured HTTP status")
	}
}
