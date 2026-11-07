package redact

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDiagnosticRedactionPreservesRequestAndNumbers(t *testing.T) {
	raw := "https://user:SECRET@example.test/a%2Fb?x=1&ToKeN=SECRET&x=a%20b&%73ign=SECRET"
	want := "https://example.test/a%2Fb?x=1&ToKeN=***&x=a%20b&%73ign=***"
	if got := URL(raw); got != want {
		t.Fatalf("got %q", got)
	}
	source := map[string]any{"url": raw, "password": "SECRET", "id": json.Number("9007199254740993")}
	b, err := JSON(source)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "SECRET") || !strings.Contains(string(b), "9007199254740993") {
		t.Fatal(string(b))
	}
	if source["url"] != raw || source["password"] != "SECRET" {
		t.Fatal("source mutated")
	}
	for _, s := range []string{`Authorization: Bearer SECRET`, `Cookie: session=SECRET; custom=SECRET`, `authorization=Bearer SECRET`, `password="a SECRET value"`, `dsn="root:SECRET@tcp(localhost:3306)/test"`, `root:SECRET@tcp(localhost:3306)/test`, `{"token":"SECRET"}`, raw} {
		got := Text(s)
		if strings.Contains(got, "SECRET") {
			t.Fatalf("unmasked diagnostic: %s", got)
		}
		if Text(got) != got {
			t.Fatalf("mask not stable: %s", got)
		}
	}
}

func TestDiagnosticJSONKeepsStructuralKeys(t *testing.T) {
	raw, err := JSON(map[string]any{"key": "backlog", "sites": []any{map[string]string{"key": "site-a"}}, "attrs": map[string]string{"key": "SECRET"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "SECRET") || !strings.Contains(string(raw), "site-a") || !strings.Contains(string(raw), "backlog") {
		t.Fatal(string(raw))
	}
}

func TestDiagnosticJSONMasksSensitiveContainersAndNumbers(t *testing.T) {
	raw, err := JSON(map[string]any{"fields": map[string]any{"token": 123456, "password": map[string]string{"value": "SECRET"}, "Cookie": map[string]string{"sid": "SECRET"}}, "id": uint64(9007199254740993)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "SECRET") || strings.Contains(string(raw), "123456") || !strings.Contains(string(raw), "9007199254740993") {
		t.Fatal(string(raw))
	}
}

func TestMalformedDiagnosticURLStillMasksCredentials(t *testing.T) {
	for _, raw := range []string{"https://user:SECRET@example.test/%zz?%73ign=SECRET", "https://user:SECRET@example.test:bad/a?token=SECRET"} {
		if got := Text(raw); strings.Contains(got, "SECRET") {
			t.Fatal("malformed URL exposed credential")
		}
	}
}

func TestDiagnosticCredentialDelimitersAndTruncation(t *testing.T) {
	for _, raw := range []string{
		`connect user:pass@SECRET@tcp(localhost:3306)/fixture`,
		`GET https://user:pass'SECRET@example.test/path`,
		`password="SECRET`, `token='SECRET`, `password="SECRET\`,
	} {
		masked := Text(raw)
		if strings.Contains(masked, "SECRET") {
			t.Errorf("credential delimiter or truncation leaked: %q", masked)
		}
		if Text(masked) != masked {
			t.Errorf("redaction not idempotent: %q", masked)
		}
	}
}

func TestTextPreservesNonCredentialURLDelimiters(t *testing.T) {
	for _, raw := range []string{
		`GET https://example.test/a%2Fb?x=1&x=a%20b`,
		`GET "https://example.test/path" user@example.test`,
		`https://example.test/path/@detail?email=user@example.test#fragment@example.test`,
		`https://example.test/author's/page?cursor=17`,
	} {
		if got := Text(raw); got != raw {
			t.Errorf("ordinary diagnostic changed: %q -> %q", raw, got)
		}
	}
}
