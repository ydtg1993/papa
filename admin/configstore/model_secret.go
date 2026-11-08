package configstore

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/ydtg1993/papa/v4/pkg/crawlagent/model"
	"github.com/ydtg1993/papa/v4/pkg/egress"
)

var ErrModelKeyMissing = errors.New("model API key must be entered")
var ErrModelKeyStorage = errors.New("model credential storage unavailable")
var masterKeyMu sync.Mutex

func validModelKey(key string) bool {
	if len(key) < 1 || len(key) > 4096 {
		return false
	}
	for _, b := range []byte(key) {
		if b < 33 || b > 126 {
			return false
		}
	}
	return true
}

func (s *Store) modelAEAD(create bool) (cipher.AEAD, error) {
	masterKeyMu.Lock()
	defer masterKeyMu.Unlock()
	path, err := filepath.Abs(s.modelKeyFile)
	if err != nil || s.modelKeyFile == "" {
		return nil, ErrModelKeyStorage
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && create {
		if os.MkdirAll(filepath.Dir(path), 0700) != nil {
			return nil, ErrModelKeyStorage
		}
		key := make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, ErrModelKeyStorage
		}
		f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return nil, ErrModelKeyStorage
		}
		_, e = f.Write(key)
		if e == nil {
			e = f.Sync()
		}
		closeErr := f.Close()
		if e != nil || closeErr != nil {
			return nil, ErrModelKeyStorage
		}
		info, err = os.Lstat(path)
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() != 32 {
		return nil, ErrModelKeyStorage
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, ErrModelKeyStorage
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, ErrModelKeyStorage
	}
	key, err := io.ReadAll(io.LimitReader(f, 33))
	if err != nil || len(key) != 32 {
		return nil, ErrModelKeyStorage
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrModelKeyStorage
	}
	return cipher.NewGCM(block)
}

func (s *Store) encryptModelKey(key string) (string, error) {
	if !validModelKey(key) {
		return "", ErrModelSettings
	}
	aead, err := s.modelAEAD(true)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", ErrModelKeyStorage
	}
	data := aead.Seal(nonce, nonce, []byte(key), []byte("papa/agent-model/v1"))
	return base64.StdEncoding.EncodeToString(data), nil
}
func (s *Store) decryptModelKey(encoded string) (string, error) {
	if encoded == "" {
		return "", ErrModelKeyMissing
	}
	aead, err := s.modelAEAD(false)
	if err != nil {
		return "", err
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(data) < aead.NonceSize()+aead.Overhead() {
		return "", ErrModelKeyStorage
	}
	plain, err := aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], []byte("papa/agent-model/v1"))
	if err != nil || !validModelKey(string(plain)) {
		return "", ErrModelKeyStorage
	}
	return string(plain), nil
}

// NewModelProvider binds the key and provider from the loaded revision, and
// applies the shared persisted daily quota to every completion. No env fallback.
func (s *Store) NewModelProvider(ctx context.Context, m ModelSettings, network egress.Config) (model.Provider, error) {
	if ctx == nil || m.Validate() != nil {
		return nil, model.ErrConfig
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if !m.Enabled {
		return nil, model.ErrConfig
	}
	key, err := s.decryptModelKey(m.secretCipher)
	if err != nil {
		return nil, err
	}
	provider, err := model.NewWithKey(m.ProviderConfig(network), key)
	if err != nil {
		return nil, err
	}
	return s.WithDailyModelQuota(provider), nil
}
