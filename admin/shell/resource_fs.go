package shell

import (
	"bytes"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
)

// resourceFS is built once, then read-only. Open never exposes writable data.
type resourceFS struct {
	files map[string][]byte
	dirs  map[string][]fs.DirEntry
}

func newResourceFS() *resourceFS {
	return &resourceFS{files: map[string][]byte{}, dirs: map[string][]fs.DirEntry{}}
}
func (r *resourceFS) add(name string, data []byte) error {
	if !fs.ValidPath(name) || name == "." {
		return &fs.PathError{Op: "add", Path: name, Err: fs.ErrInvalid}
	}
	if _, exists := r.files[name]; exists {
		return &fs.PathError{Op: "add", Path: name, Err: fs.ErrExist}
	}
	r.files[name] = bytes.Clone(data)
	return nil
}
func (r *resourceFS) seal() {
	directories := map[string]bool{".": true}
	for name := range r.files {
		for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
			directories[dir] = true
		}
	}
	for dir := range directories {
		r.dirs[dir] = nil
	}
	for name, data := range r.files {
		dir := path.Dir(name)
		r.dirs[dir] = append(r.dirs[dir], fs.FileInfoToDirEntry(resourceInfo{name: path.Base(name), size: int64(len(data))}))
	}
	for dir := range directories {
		if dir != "." {
			parent := path.Dir(dir)
			r.dirs[parent] = append(r.dirs[parent], fs.FileInfoToDirEntry(resourceInfo{name: path.Base(dir), dir: true}))
		}
	}
	for dir := range r.dirs {
		sort.Slice(r.dirs[dir], func(i, j int) bool { return r.dirs[dir][i].Name() < r.dirs[dir][j].Name() })
	}
}
func (r *resourceFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if data, ok := r.files[name]; ok {
		return &resourceFile{Reader: bytes.NewReader(data), info: resourceInfo{name: path.Base(name), size: int64(len(data))}}, nil
	}
	if entries, ok := r.dirs[name]; ok {
		return &resourceDir{info: resourceInfo{name: path.Base(name), dir: true}, entries: entries}, nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}
func (r *resourceFS) ReadFile(name string) ([]byte, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "read", Path: name, Err: fs.ErrInvalid}
	}
	if data, ok := r.files[name]; ok {
		return bytes.Clone(data), nil
	}
	return nil, &fs.PathError{Op: "read", Path: name, Err: fs.ErrNotExist}
}

type resourceInfo struct {
	name string
	size int64
	dir  bool
}

func (i resourceInfo) Name() string { return i.name }
func (i resourceInfo) Size() int64  { return i.size }
func (i resourceInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0555
	}
	return 0444
}
func (i resourceInfo) ModTime() time.Time { return time.Time{} }
func (i resourceInfo) IsDir() bool        { return i.dir }
func (i resourceInfo) Sys() any           { return nil }

type resourceFile struct {
	*bytes.Reader
	info resourceInfo
}

func (f *resourceFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *resourceFile) Close() error               { return nil }

type resourceDir struct {
	info    resourceInfo
	entries []fs.DirEntry
	offset  int
}

func (d *resourceDir) Stat() (fs.FileInfo, error) { return d.info, nil }
func (d *resourceDir) Close() error               { return nil }
func (d *resourceDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.info.name, Err: fs.ErrInvalid}
}
func (d *resourceDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if n > 0 && d.offset >= len(d.entries) {
		return nil, io.EOF
	}
	end := len(d.entries)
	if n > 0 && d.offset+n < end {
		end = d.offset + n
	}
	result := append([]fs.DirEntry(nil), d.entries[d.offset:end]...)
	d.offset = end
	return result, nil
}

func javascript(runtime string, body []byte) []byte {
	return []byte(runtime + "\n;\n" + strings.TrimSpace(string(body)) + "\n")
}
