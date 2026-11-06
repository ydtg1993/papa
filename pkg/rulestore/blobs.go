package rulestore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// MaxSampleBodyBytes bounds both imported and redacted bodies, before copying.
const MaxSampleBodyBytes = 8 << 20

var (
	ErrBlobInvalid  = errors.New("rulestore: invalid sample blob")
	ErrSampleBudget = errors.New("rulestore: sample exceeds budget")
)

// FileBlobs stores immutable, SHA-256-addressed sample bodies independently of
// short-lived failure archives. It never automatically deletes published blobs.
// Keep the directory private to the service account. POSIX permissions are
// 0700/0600; Windows deployments must also restrict the directory's ACL.
type FileBlobs struct{ root *os.Root }

func NewFileBlobs(path string) (*FileBlobs, error) {
	if strings.TrimSpace(path) == "" {
		return nil, ErrBlobInvalid
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return nil, fmt.Errorf("rulestore: create blob directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrBlobInvalid
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("rulestore: open blob directory: %w", err)
	}
	if err := root.Chmod(".", 0700); err != nil {
		root.Close()
		return nil, fmt.Errorf("rulestore: protect blob directory: %w", err)
	}
	return &FileBlobs{root: root}, nil
}

func (b *FileBlobs) Close() error {
	if b == nil || b.root == nil {
		return nil
	}
	return b.root.Close()
}

func bodySHA256(body []byte) string { sum := sha256.Sum256(body); return hex.EncodeToString(sum[:]) }

func validBlobHash(hash string) bool {
	if len(hash) != sha256.Size*2 {
		return false
	}
	for _, c := range hash {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Put publishes a fully written, synced private file using an atomic hard link
// that cannot replace an existing digest. Existing files are read and verified,
// never trusted merely because their names match. Filesystems must support hard
// links; failure is explicit. Unreferenced blobs after DB failure are retained.
func (b *FileBlobs) Put(ctx context.Context, body []byte) (string, error) {
	if ctx == nil || b == nil || b.root == nil {
		return "", ErrBlobInvalid
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(body) > MaxSampleBodyBytes {
		return "", ErrSampleBudget
	}
	body = bytes.Clone(body)
	hash := bodySHA256(body)
	if _, err := b.root.Lstat(hash); err == nil {
		existing, err := b.Get(ctx, hash, MaxSampleBodyBytes)
		if err != nil {
			return "", err
		}
		if !bytes.Equal(existing, body) {
			return "", ErrBlobInvalid
		}
		return hash, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("rulestore: inspect blob: %w", err)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("rulestore: create blob nonce: %w", err)
	}
	temp := ".pending-" + hex.EncodeToString(nonce[:])
	f, err := b.root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", fmt.Errorf("rulestore: create blob: %w", err)
	}
	defer b.root.Remove(temp)
	_, writeErr := f.Write(body)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		return "", fmt.Errorf("rulestore: write blob: %w", writeErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("rulestore: close blob: %w", closeErr)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := b.root.Link(temp, hash); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("rulestore: publish blob: %w", err)
		}
	}
	verified, err := b.Get(ctx, hash, MaxSampleBodyBytes)
	if err != nil {
		return "", err
	}
	if !bytes.Equal(verified, body) {
		return "", ErrBlobInvalid
	}
	return hash, nil
}

// Get rejects unknown, nonregular, symlinked, oversized and corrupted files.
// A zero maxBytes selects MaxSampleBodyBytes; other limits must be positive and
// no larger than that hard ceiling. Returned bytes belong to the caller.
func (b *FileBlobs) Get(ctx context.Context, hash string, maxBytes int) ([]byte, error) {
	if ctx == nil || b == nil || b.root == nil || !validBlobHash(hash) {
		return nil, ErrBlobInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maxBytes == 0 {
		maxBytes = MaxSampleBodyBytes
	}
	if maxBytes < 0 || maxBytes > MaxSampleBodyBytes {
		return nil, ErrSampleBudget
	}
	info, err := b.root.Lstat(hash)
	if err != nil {
		return nil, fmt.Errorf("rulestore: inspect blob: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, ErrBlobInvalid
	}
	if info.Size() > int64(maxBytes) {
		return nil, ErrSampleBudget
	}
	f, err := b.root.Open(hash)
	if err != nil {
		return nil, fmt.Errorf("rulestore: open blob: %w", err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, ErrBlobInvalid
	}
	body, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
	if err != nil {
		return nil, fmt.Errorf("rulestore: read blob: %w", err)
	}
	if len(body) > maxBytes {
		return nil, ErrSampleBudget
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if bodySHA256(body) != hash {
		return nil, ErrBlobInvalid
	}
	return body, nil
}
