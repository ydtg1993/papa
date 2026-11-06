package rulestore

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
)

func explicitSamplePolicy() htmlfetch.SamplePolicy {
	return htmlfetch.SamplePolicy{RedactBody: func(request bool, body []byte) ([]byte, error) { return body, nil }}
}

func TestSanitizeSampleExplicitPolicyAndPrivateCopies(t *testing.T) {
	input := SampleInput{URL: "https://user:secret@example.test/path?token=secret&page=2#secret", StatusCode: 404, Body: []byte("private body"), EvidenceID: "archive-1"}
	if _, err := SanitizeSample(input, htmlfetch.SamplePolicy{}); !errors.Is(err, ErrRedactionRequired) {
		t.Fatal(err)
	}
	var callbackBody []byte
	policy := htmlfetch.SamplePolicy{QueryKeys: []string{"page"}, Headers: []string{"Cookie"}, RedactBody: func(request bool, b []byte) ([]byte, error) {
		if request {
			t.Fatal("request callback")
		}
		b[0] = 'P'
		callbackBody = b
		return b, nil
	}}
	got, err := SanitizeSample(input, policy)
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "https://example.test/path?page=2" || got.StatusCode != 404 || got.Rendered || got.EvidenceID != "archive-1" {
		t.Fatalf("metadata: %+v", got)
	}
	if string(input.Body) != "private body" || string(got.Body) != "Private body" || got.BodySHA256 != bodySHA256(got.Body) || got.BodyBytes != len(got.Body) {
		t.Fatal("body copy/digest mismatch")
	}
	callbackBody[0] = 'X'
	if got.Body[0] != 'P' {
		t.Fatal("retained callback output alias")
	}
	raw, _ := json.Marshal(got)
	if bytes.Contains(raw, []byte("Private body")) || bytes.Contains(raw, []byte("secret")) || bytes.Contains(raw, []byte("headers")) {
		t.Fatalf("unsafe metadata %s", raw)
	}
}

func TestSanitizeSamplePreservesRenderedAndHTTPStatusSemantics(t *testing.T) {
	for _, tt := range []struct {
		status          int
		rendered, valid bool
	}{{0, true, true}, {200, true, false}, {0, false, false}, {99, false, false}, {600, false, false}, {503, false, true}, {200, false, true}} {
		got, err := SanitizeSample(SampleInput{URL: "https://example.test/", StatusCode: tt.status, Rendered: tt.rendered, Body: []byte("body")}, explicitSamplePolicy())
		if (err == nil) != tt.valid {
			t.Fatalf("status %d rendered %v: %v", tt.status, tt.rendered, err)
		}
		if err == nil && (got.StatusCode != tt.status || got.Rendered != tt.rendered) {
			t.Fatal("fabricated HTTP state")
		}
	}
}

func TestSanitizeSampleCallbacksNeverLeakErrorsOrPanics(t *testing.T) {
	input := SampleInput{URL: "https://example.test/", StatusCode: 200, Body: []byte("secret")}
	policies := []htmlfetch.SamplePolicy{
		{RedactBody: func(bool, []byte) ([]byte, error) { return nil, errors.New("secret-body") }},
		{RedactBody: func(bool, []byte) ([]byte, error) { panic("secret-body") }},
		{RedactBody: explicitSamplePolicy().RedactBody, RedactURL: func(string) (string, error) { return "", errors.New("secret-url") }},
		{RedactBody: explicitSamplePolicy().RedactBody, RedactURL: func(string) (string, error) { panic("secret-url") }},
		{RedactBody: explicitSamplePolicy().RedactBody, RedactURL: func(string) (string, error) { return "not a URL", nil }},
	}
	for _, policy := range policies {
		got, err := SanitizeSample(input, policy)
		if err != ErrSampleRedaction || got.Body != nil || got.URL != "" || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe callback failure: %+v, %v", got, err)
		}
	}
}

func TestSanitizeSampleRejectsInputAndOutputBudgets(t *testing.T) {
	input := SampleInput{URL: "https://example.test/", StatusCode: 200, Body: make([]byte, MaxSampleBodyBytes+1)}
	called := false
	policy := htmlfetch.SamplePolicy{RedactBody: func(bool, []byte) ([]byte, error) { called = true; return nil, nil }}
	if _, err := SanitizeSample(input, policy); !errors.Is(err, ErrSampleBudget) || called {
		t.Fatalf("input budget: %v called=%v", err, called)
	}
	input.Body = []byte("ok")
	policy.RedactBody = func(bool, []byte) ([]byte, error) { return make([]byte, MaxSampleBodyBytes+1), nil }
	if _, err := SanitizeSample(input, policy); !errors.Is(err, ErrSampleBudget) {
		t.Fatalf("output budget: %v", err)
	}
	input.URL = "https://example.test/" + strings.Repeat("a", MaxSampleURLBytes)
	if _, err := SanitizeSample(input, explicitSamplePolicy()); !errors.Is(err, ErrSampleBudget) {
		t.Fatal(err)
	}
}

func TestSanitizeSampleRechecksRedactedURL(t *testing.T) {
	policy := explicitSamplePolicy()
	policy.RedactURL = func(string) (string, error) { return "https://user:secret@example.test/clean?secret=1#secret", nil }
	got, err := SanitizeSample(SampleInput{URL: "https://example.test/", StatusCode: 200}, policy)
	if err != nil || got.URL != "https://example.test/clean" {
		t.Fatalf("recheck: %+v %v", got, err)
	}
}

func TestSanitizeSampleChecksEncodedURLAndEvidenceBudgets(t *testing.T) {
	input := SampleInput{URL: "https://example.test/" + strings.Repeat("字", MaxSampleURLBytes/4), StatusCode: 200}
	if _, err := SanitizeSample(input, explicitSamplePolicy()); !errors.Is(err, ErrSampleBudget) {
		t.Fatalf("encoded URL budget: %v", err)
	}
	input.URL = "https://example.test/"
	for _, id := range []string{"bad\nID", string([]byte{0xff})} {
		input.EvidenceID = id
		if _, err := SanitizeSample(input, explicitSamplePolicy()); !errors.Is(err, ErrSampleInvalid) {
			t.Fatalf("invalid evidence ID: %v", err)
		}
	}
}
