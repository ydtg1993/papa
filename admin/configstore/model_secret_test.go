package configstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v4/pkg/egress"
)

func TestModelSecretEncryptionRestartAndTamper(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", ".papa-model.key")
	s := NewWithModelKeyFile(nil, path)
	const key = "synthetic-private-api-key"
	encoded, err := s.encryptModelKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encoded, key) {
		t.Fatal("plaintext ciphertext")
	}
	reopened := NewWithModelKeyFile(nil, path)
	plain, err := reopened.decryptModelKey(encoded)
	if err != nil || plain != key {
		t.Fatal("restart lost key", err)
	}
	b, _ := base64.StdEncoding.DecodeString(encoded)
	b[len(b)-1] ^= 1
	if _, err = reopened.decryptModelKey(base64.StdEncoding.EncodeToString(b)); !errors.Is(err, ErrModelKeyStorage) {
		t.Fatal("tampered ciphertext accepted")
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err = reopened.decryptModelKey(encoded); !errors.Is(err, ErrModelKeyStorage) {
		t.Fatal("missing master accepted")
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("read regenerated lost master")
	}
}

func TestModelSettingsNeverExposeKeyAndFreezeRevision(t *testing.T) {
	s := NewWithModelKeyFile(nil, filepath.Join(t.TempDir(), ".papa-model.key"))
	m := DefaultModelSettings().WithAPIKey("pending-secret-key")
	cipher, err := s.encryptModelKey("saved-secret-key")
	if err != nil {
		t.Fatal(err)
	}
	m.secretCipher = cipher
	m.KeyConfigured = true
	for _, v := range []string{fmt.Sprintf("%v", m), fmt.Sprintf("%+v", m), fmt.Sprintf("%#v", m)} {
		for _, secret := range []string{"pending-secret-key", "saved-secret-key", cipher} {
			if strings.Contains(v, secret) {
				t.Fatal("format exposes key")
			}
		}
	}
	raw, _ := json.Marshal(m)
	if strings.Contains(string(raw), "secret-key") || strings.Contains(string(raw), cipher) {
		t.Fatal("public JSON exposes key")
	}
	stored, _ := json.Marshal(storedModelSettings{ModelSettings: m, Cipher: cipher})
	frozen, err := decodeModelSettings(stored)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := s.encryptModelKey("replacement-key")
	if err != nil {
		t.Fatal(err)
	}
	if newer == cipher {
		t.Fatal("replacement reused ciphertext")
	}
	old, err := s.decryptModelKey(frozen.secretCipher)
	if err != nil || old != "saved-secret-key" {
		t.Fatal("loaded revision changed key")
	}
	frozen.Enabled = true
	frozen.BaseURL = "https://models.example/v1"
	frozen.Model = "fixture"
	if _, err = s.NewModelProvider(context.Background(), frozen, egress.Config{}); err != nil {
		t.Fatal("saved key requires environment", err)
	}
	frozen.secretCipher = ""
	t.Setenv("PAPA_AGENT_MODEL_KEY", "unused-environment-key")
	if _, err = s.NewModelProvider(context.Background(), frozen, egress.Config{}); !errors.Is(err, ErrModelKeyMissing) {
		t.Fatal("fell back to env")
	}
}

func TestModelSecretInvalidMasterFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	s := NewWithModelKeyFile(nil, path)
	if _, err := s.encryptModelKey("replacement"); !errors.Is(err, ErrModelKeyStorage) {
		t.Fatal("overwrote corrupt master")
	}
	b, _ := os.ReadFile(path)
	if string(b) != "bad" {
		t.Fatal("modified corrupt master")
	}
}
