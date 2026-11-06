package rulestore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadSnapshotChecksBodyAndKeepsRenderedSemantics(t *testing.T) {
	dir := t.TempDir()
	id := strings.Repeat("a", 32)
	body := []byte("<article>public fixture</article>")
	hash := sha256.Sum256(body)
	digest := hex.EncodeToString(hash[:])
	for _, sub := range []string{"observations", "bodies"} {
		if err := os.MkdirAll(filepath.Join(dir, ".rule-snapshots", sub), 0700); err != nil {
			t.Fatal(err)
		}
	}
	bodyPath := ".rule-snapshots/bodies/" + digest + ".body"
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(bodyPath)), body, 0600); err != nil {
		t.Fatal(err)
	}
	m := map[string]any{"version": 1, "evidence_id": id, "source_url": "https://example.test/page", "status_code": 0, "rendered": true, "body_file": bodyPath, "body_sha256": digest, "body_bytes": len(body), "rule": map[string]string{"id": "list", "version": "v1"}}
	write := func() {
		t.Helper()
		data, _ := json.Marshal(m)
		if err := os.WriteFile(filepath.Join(dir, ".rule-snapshots", "observations", id+".json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	input, err := ReadSnapshot(context.Background(), dir, id)
	if err != nil || !input.Rendered || input.StatusCode != 0 || input.EvidenceID != id || string(input.Body) != string(body) {
		t.Fatalf("read %+v,%v", input, err)
	}
	for _, key := range []string{"version", "body_file", "body_sha256", "body_omitted", "body_bytes", "status_code", "source_url"} {
		old, exists := m[key]
		switch key {
		case "version":
			m[key] = 2
		case "body_file":
			m[key] = "../outside"
		case "body_sha256":
			m[key] = strings.Repeat("b", 64)
		case "body_omitted":
			m[key] = "policy"
		case "body_bytes":
			m[key] = len(body) + 1
		case "status_code":
			m[key] = 200
		case "source_url":
			m[key] = "file:///private"
		}
		write()
		if _, err := ReadSnapshot(context.Background(), dir, id); err == nil {
			t.Errorf("accepted corrupt %s", key)
		}
		if exists {
			m[key] = old
		} else {
			delete(m, key)
		}
	}
	write()
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(bodyPath)), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSnapshot(context.Background(), dir, id); err == nil {
		t.Fatal("accepted corrupt body")
	}
	if _, err := ReadSnapshot(context.Background(), dir, "../"+id); err == nil {
		t.Fatal("accepted path ID")
	}
}
