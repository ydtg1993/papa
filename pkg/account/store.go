package account

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ydtg1993/papa/v3/pkg/browser"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
)

// Store must not reenter Manager. Calls are serialized by the account lock.
type Store interface {
	Load(context.Context, Key) (*StoredState, error)
	Save(context.Context, Key, *StoredState) error
	Delete(context.Context, Key) error
}
type CredentialState struct {
	Cookies htmlfetch.SessionState `json:"cookies"`
	Headers map[string]string      `json:"headers"`
	Values  map[string]string      `json:"values"`
}
type StoredState struct {
	Version int                `json:"version"`
	Key     Key                `json:"key"`
	HTTP    *CredentialState   `json:"http,omitempty"`
	Browser *browser.PageState `json:"browser,omitempty"`
}

func (e *entry) load(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.paused {
		return ErrPaused
	}
	if e.loaded {
		return nil
	}
	if e.config.Store == nil {
		e.loaded = true
		return nil
	}
	s, err := e.config.Store.Load(ctx, e.key)
	if errors.Is(err, os.ErrNotExist) {
		e.loaded = true
		return nil
	}
	if err != nil {
		return &Failure{Code: "state-load-failed", Cause: err}
	}
	if s == nil || s.Version != 1 || s.Key != e.key {
		return &Failure{Code: "invalid-stored-state"}
	}
	var c *Credentials
	if s.HTTP != nil {
		session, err := htmlfetch.RestoreSession(s.HTTP.Cookies)
		if err != nil {
			return &Failure{Code: "invalid-cookie-state", Cause: err}
		}
		c = &Credentials{Session: session, Headers: s.HTTP.Headers, Values: s.HTTP.Values}
		if err = e.checkCredentials(c); err != nil {
			return &Failure{Code: "invalid-stored-credentials", Cause: err}
		}
	}
	state, err := clonePageState(s.Browser)
	if err != nil {
		return &Failure{Code: "invalid-browser-state", Cause: err}
	}
	if state != nil {
		if !e.origins[state.Origin] {
			return &Failure{Code: "browser-state-outside-origin"}
		}
	}
	e.current = c
	e.pageState = state
	e.version++
	e.revision++
	e.loaded = true
	return nil
}
func (e *entry) saveLocked(ctx context.Context) error {
	if e.config.Store == nil {
		return nil
	}
	state, err := clonePageState(e.pageState)
	if err != nil {
		return &Failure{Code: "invalid-browser-state", Cause: err}
	}
	s := &StoredState{Version: 1, Key: e.key, Browser: state}
	if c := e.current; c != nil {
		cookies := c.Session.Snapshot()
		if _, err := htmlfetch.RestoreSession(cookies); err != nil {
			return &Failure{Code: "unrestorable-cookie-state", Cause: err}
		}
		s.HTTP = &CredentialState{Cookies: cookies, Headers: maps.Clone(c.Headers), Values: maps.Clone(c.Values)}
	}
	if err := e.config.Store.Save(ctx, e.key, s); err != nil {
		return &Failure{Code: "state-save-failed", Cause: err}
	}
	return nil
}

// Logout prevents new acquisitions, removes in-memory credentials and deletes
// the stored snapshot. In-flight operations are not forcibly terminated. This
// does not call the site's remote logout endpoint. Resume permits a fresh login.
func (m *Manager) Logout(ctx context.Context, key Key) error {
	e, err := m.get(key)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.paused = true
	e.version++
	e.revision++
	e.current = nil
	e.seed = nil
	e.pageState = nil
	e.lastCheck = time.Time{}
	e.browserCheck = time.Time{}
	e.loaded = true
	if e.config.Store != nil {
		if err = e.config.Store.Delete(ctx, key); err != nil {
			return &Failure{Code: "state-delete-failed", Cause: err}
		}
	}
	return nil
}

func clonePageState(state *browser.PageState) (*browser.PageState, error) {
	if state == nil {
		return nil, nil
	}
	b, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	if len(b) > 8<<20 {
		return nil, errors.New("browser state too large")
	}
	var out browser.PageState
	err = json.Unmarshal(b, &out)
	if err == nil {
		err = browser.ValidatePageState(&out)
	}
	return &out, err
}

const maxStoredBytes = 16 << 20

var stateHeader = []byte("PAPAAC01")

// FileStore encrypts credentials with a caller-owned 32-byte key. Use a private
// directory (Windows ACLs are caller-owned); keys never appear in file names.
// One process owns a directory: no cross-process lease or writer coordination.
type FileStore struct {
	mu        sync.Mutex
	directory string
	aead      cipher.AEAD
}

func NewFileStore(directory string, key []byte) (*FileStore, error) {
	if directory == "" || len(key) != 32 {
		return nil, errors.New("state directory and 32-byte key required")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	directory, err = filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	return &FileStore{directory: directory, aead: aead}, nil
}
func (f *FileStore) location(key Key) (string, []byte) {
	aad, _ := json.Marshal(key)
	sum := sha256.Sum256(aad)
	return filepath.Join(f.directory, hex.EncodeToString(sum[:])+".state"), append(bytes.Clone(stateHeader), aad...)
}
func (f *FileStore) Save(ctx context.Context, key Key, state *StoredState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if state == nil || state.Version != 1 || state.Key != key {
		return errors.New("account state identity mismatch")
	}
	plain, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if len(plain) > maxStoredBytes {
		return errors.New("account state exceeds 16 MiB")
	}
	path, aad := f.location(key)
	nonce := make([]byte, f.aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	data := append(bytes.Clone(stateHeader), nonce...)
	data = f.aead.Seal(data, nonce, plain, aad)
	f.mu.Lock()
	defer f.mu.Unlock()
	file, err := os.CreateTemp(f.directory, ".account-*")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer os.Remove(temp)
	if _, err = file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	// Rename avoids publishing partially written ciphertext. Filesystem durability
	// and atomic replacement guarantees are platform/filesystem dependent.
	return os.Rename(temp, path)
}
func (f *FileStore) Load(ctx context.Context, key Key) (*StoredState, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, aad := f.location(key)
	f.mu.Lock()
	defer f.mu.Unlock()
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	limit := maxStoredBytes + len(stateHeader) + f.aead.NonceSize() + f.aead.Overhead()
	data, err := io.ReadAll(io.LimitReader(file, int64(limit+1)))
	if err != nil {
		return nil, err
	}
	start := len(stateHeader)
	end := start + f.aead.NonceSize()
	if len(data) > limit || len(data) < end+f.aead.Overhead() || !bytes.Equal(data[:start], stateHeader) {
		return nil, errors.New("invalid encrypted account state")
	}
	plain, err := f.aead.Open(nil, data[start:end], data[end:], aad)
	if err != nil {
		return nil, errors.New("account state authentication failed")
	}
	var state StoredState
	if err = htmlfetch.DecodeJSON(plain, &state); err != nil {
		return nil, errors.New("invalid account state JSON")
	}
	if state.Version != 1 || state.Key != key {
		return nil, errors.New("account state identity mismatch")
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return &state, nil
}
func (f *FileStore) Delete(ctx context.Context, key Key) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, _ := f.location(key)
	f.mu.Lock()
	defer f.mu.Unlock()
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
