package model

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestCompatibleExplicitKeyIsFrozenWithoutEnvironmentMutation(t *testing.T) {
	t.Setenv(testKeyEnv, "environment-secret")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer explicit-secret" {
			t.Error("wrong credential source")
		}
		writeJSON(w, validResponse)
	}))
	defer server.Close()
	p, e := NewWithKey(Config{BaseURL: server.URL, Model: "test", APIKeyEnv: testKeyEnv, AllowHTTP: true}, "explicit-secret")
	if e != nil {
		t.Fatal(e)
	}
	t.Setenv(testKeyEnv, "changed-environment-secret")
	if _, e = p.Complete(context.Background(), testMessages(), 10); e != nil {
		t.Fatal(e)
	}
	if os.Getenv(testKeyEnv) != "changed-environment-secret" {
		t.Fatal("environment mutated")
	}
	if strings.Contains(fmt.Sprintf("%v %#v", p, p), "explicit-secret") {
		t.Fatal("credential leaked")
	}
	for _, key := range []string{"", "bad\nsecret"} {
		if _, e = NewWithKey(Config{BaseURL: server.URL, Model: "test", AllowHTTP: true}, key); e == nil {
			t.Fatal("invalid key accepted")
		}
	}
}
