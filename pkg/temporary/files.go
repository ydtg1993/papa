// Package temporary manages host-owned temporary files and resumable cleanup.
package temporary

import (
	"errors"
	"github.com/google/uuid"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
)

var ErrInvalidPath = errors.New("temporary: invalid owned path")
var namespacePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// Files locates UUID-owned directories below a host-configured parent and namespace.
// Parent must already exist. Callers serialize writes, reads and removal for an ID.
type Files struct{ Parent, Namespace string }

func ValidID(id string) bool {
	u, e := uuid.Parse(id)
	return e == nil && u != uuid.Nil && u.String() == id
}

// Windows junctions/reparse points are rejected in addition to ordinary symlinks.
func unsafeFile(info fs.FileInfo) bool {
	if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return true
	}
	v := reflect.ValueOf(info.Sys())
	if v.IsValid() && v.Kind() == reflect.Pointer && !v.IsNil() {
		v = v.Elem()
	}
	if v.IsValid() && v.Kind() == reflect.Struct {
		f := v.FieldByName("FileAttributes")
		if f.IsValid() && f.CanUint() && f.Uint()&0x400 != 0 {
			return true
		}
	}
	return false
}

func safeAncestors(path string) error {
	for {
		info, e := os.Lstat(path)
		if e != nil {
			return e
		}
		if unsafeFile(info) || !info.IsDir() {
			return ErrInvalidPath
		}
		next := filepath.Dir(path)
		if next == path {
			return nil
		}
		path = next
	}
}

// The root is entirely host-derived. os.Root also confines operations against
// path substitution races; caller/model paths are never accepted.
func (f Files) Open(create bool) (*os.Root, error) {
	if f.Parent == "" || !namespacePattern.MatchString(f.Namespace) {
		return nil, ErrInvalidPath
	}
	base, e := filepath.Abs(f.Parent)
	if e != nil {
		return nil, e
	}
	if e = safeAncestors(base); e != nil {
		return nil, e
	}
	root, e := os.OpenRoot(base)
	if e != nil {
		return nil, e
	}
	defer root.Close()
	if create {
		if e = root.Mkdir(f.Namespace, 0700); e != nil && !errors.Is(e, fs.ErrExist) {
			return nil, e
		}
	}
	info, e := root.Lstat(f.Namespace)
	if e != nil {
		return nil, e
	}
	if unsafeFile(info) || !info.IsDir() {
		return nil, ErrInvalidPath
	}
	return root.OpenRoot(f.Namespace)
}

func (f Files) Remove(id string) error {
	if !ValidID(id) {
		return ErrInvalidPath
	}
	root, e := f.Open(false)
	if errors.Is(e, fs.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	defer root.Close()
	info, e := root.Lstat(id)
	if errors.Is(e, fs.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	if unsafeFile(info) || !info.IsDir() {
		return ErrInvalidPath
	}
	// Reject links even if they point within the root. No shared sample can be
	// reached by a recursive deletion through an experiment directory.
	e = fs.WalkDir(root.FS(), id, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := root.Lstat(path)
		if err != nil {
			return err
		}
		if unsafeFile(info) {
			return ErrInvalidPath
		}
		return nil
	})
	if e != nil {
		return e
	}
	return root.RemoveAll(id)
}

func (f Files) FilePath(id, fileID string) (string, error) {
	if !ValidID(id) || !ValidID(fileID) {
		return "", ErrInvalidPath
	}
	root, e := f.Open(false)
	if e != nil {
		return "", e
	}
	defer root.Close()
	for _, name := range []string{id, filepath.Join(id, fileID)} {
		info, e := root.Lstat(name)
		if e != nil {
			return "", e
		}
		if unsafeFile(info) || (name == id && !info.IsDir()) || (name != id && !info.Mode().IsRegular()) {
			return "", ErrInvalidPath
		}
	}
	return filepath.Join(root.Name(), id, fileID), nil
}
