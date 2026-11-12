package core

import "testing"

func TestStandardTaskIdentity(t *testing.T) {
	identity := func(site, stage, id, u string) TaskIdentity {
		t.Helper()
		v, err := NormalizeTaskIdentity(site, stage, id, u)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	a := identity("s", "detail", "", "https://EXAMPLE.test:443/a%2Fb?q=1&x=2#fragment")
	b := identity("s", "detail", "", "https://example.test/a%2Fb?q=1&x=2")
	if a != b || len(a.Hash) != 64 {
		t.Fatal("URL normalization drift", a, b)
	}
	for _, u := range []string{"https://other.test/a%2Fb?q=1&x=2", "https://example.test/a/b?q=1&x=2", "https://example.test/a%2Fb?x=2&q=1"} {
		if identity("s", "detail", "", u).Hash == a.Hash {
			t.Fatal("distinct URL merged", u)
		}
	}
	a = identity("s", "detail", "42", "https://example.test/old")
	if a != identity("s", "detail", "42", "https://example.test/new") {
		t.Fatal("ID depends on URL")
	}
	if a.Hash == identity("other", "detail", "42", "").Hash || a.Hash == identity("s", "other", "42", "").Hash {
		t.Fatal("identity escaped scope")
	}
	if identity("a-b", "c", "d", "").Hash == identity("a", "b-c", "d", "").Hash {
		t.Fatal("ambiguous tuple")
	}
	if identity("s", "detail", "https://example.test/", "").Hash == identity("s", "detail", "", "https://example.test/").Hash {
		t.Fatal("ID/URL namespace collision")
	}
}

func TestStandardTaskIdentityRejectsInvalidURL(t *testing.T) {
	for _, u := range []string{"", "/a", "ftp://example.test/a", "https://u:p@example.test/a", "https://example.test/%zz"} {
		if _, err := NormalizeTaskIdentity("s", "detail", "", u); err == nil {
			t.Fatal("accepted invalid identity URL", u)
		}
	}
}
