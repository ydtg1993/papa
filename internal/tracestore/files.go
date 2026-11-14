package tracestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func (s *Store) inventory(ctx context.Context, r *os.Root) error {
	files := make(map[string]entry)
	var total int64
	var sequence uint64
	visited := 0
	var walk func(string, int) error
	walk = func(dir string, depth int) error {
		if depth > 4 {
			return errors.New("tracestore: unexpected directory depth")
		}
		f, err := r.Open(dir)
		if err != nil {
			return err
		}
		defer f.Close()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			items, e := f.ReadDir(64)
			if e != nil && e != io.EOF {
				return e
			}
			for _, item := range items {
				visited++
				if visited > max(s.opts.MaxFiles, DefaultMaxFiles)*5+512 {
					return errors.New("tracestore: directory inventory limit exceeded")
				}
				p := filepath.Join(dir, item.Name())
				info, err := r.Lstat(p)
				if err != nil {
					return err
				}
				if info.Mode()&os.ModeSymlink != 0 {
					return fmt.Errorf("tracestore: symlink rejected: %s", p)
				}
				if info.IsDir() {
					if err := walk(p, depth+1); err != nil {
						return err
					}
					continue
				}
				if !info.Mode().IsRegular() {
					return fmt.Errorf("tracestore: nonregular file: %s", p)
				}
				if depth != 4 {
					return fmt.Errorf("tracestore: unexpected file: %s", p)
				}
				seq, err := fileSequence(item.Name())
				if err != nil {
					return err
				}
				if info.Size() > max(s.opts.MaxFileBytes, DefaultMaxFileBytes) {
					return fmt.Errorf("tracestore: oversized file: %s", p)
				}
				total += info.Size()
				if total > max(s.opts.MaxTotalBytes, DefaultMaxTotalBytes) || len(files) >= max(s.opts.MaxFiles, DefaultMaxFiles) {
					return errors.New("tracestore: inventory exceeds safety ceiling")
				}
				files[p] = entry{p, info.Size(), seq}
				if seq > sequence {
					sequence = seq
				}
			}
			if e == io.EOF {
				break
			}
		}
		return nil
	}
	if err := walk(".", 0); err != nil {
		return err
	}
	s.files = files
	s.bytes = total
	s.sequence = sequence
	s.initialized = true
	return nil
}
func fileSequence(name string) (uint64, error) {
	suffix := ".json"
	if strings.HasSuffix(name, ".pending") {
		suffix = ".pending"
	}
	base := strings.TrimSuffix(name, suffix)
	if len(base) != 20 || base == name {
		return 0, fmt.Errorf("tracestore: invalid filename %q", name)
	}
	seq, err := strconv.ParseUint(base, 10, 64)
	if err != nil || seq == 0 {
		return 0, fmt.Errorf("tracestore: invalid sequence %q", name)
	}
	return seq, nil
}
func safePath(r *os.Root, p string) error {
	for q := p; q != "."; q = filepath.Dir(q) {
		info, err := r.Lstat(q)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("tracestore: symlink rejected: %s", q)
		}
	}
	return nil
}
func (s *Store) Write(ctx context.Context, b Batch) error {
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	if b.TaskID <= 0 || b.Attempt < 0 || b.IdentityHash == "" || b.ExecutionID == "" || len(b.IdentityHash) > 256 || len(b.ExecutionID) > 256 || b.RecordedAt.IsZero() || len(b.Steps) > 500 {
		return errors.New("tracestore: invalid batch metadata or step count")
	}
	// Bound caller-controlled strings before JSON allocation (escaping can expand them).
	budget := s.opts.MaxFileBytes
	if budget == 0 {
		budget = DefaultMaxFileBytes
	}
	for _, st := range b.Steps {
		budget -= int64(len(st.Step) + len(st.Status) + len(st.Kind) + len(st.Message) + len(st.Data))
		if budget < 0 {
			return errors.New("tracestore: batch exceeds file limit")
		}
	}
	data, err := json.Marshal(record{Version: 1, Batch: b})
	if err != nil {
		return err
	}
	if int64(len(data)) > s.opts.MaxFileBytes {
		return errors.New("tracestore: batch exceeds file limit")
	}
	r, err := s.open(ctx)
	if err != nil {
		return err
	}
	defer r.Close()
	if len(s.files) >= s.opts.MaxFiles || int64(len(data)) > s.opts.MaxTotalBytes-s.bytes {
		return errors.New("tracestore: storage quota exceeded")
	}
	if s.sequence == ^uint64(0) {
		return errors.New("tracestore: sequence exhausted")
	}
	dir := batchDir(b)
	if err = safePath(r, dir); err != nil {
		return err
	}
	if err = r.MkdirAll(dir, 0700); err != nil {
		return err
	}
	seq := s.sequence + 1
	name := filepath.Join(dir, fmt.Sprintf("%020d.json", seq))
	tmp := strings.TrimSuffix(name, ".json") + ".pending"
	f, err := r.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			_ = r.Remove(tmp)
		}
	}()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = r.Rename(tmp, name); err != nil {
		return err
	}
	published = true
	s.sequence = seq
	s.files[name] = entry{name, int64(len(data)), seq}
	s.bytes += int64(len(data))
	return nil
}
func (s *Store) read(ctx context.Context, r *os.Root, e entry) (record, error) {
	var rec record
	if err := ctx.Err(); err != nil {
		return rec, err
	}
	if err := safePath(r, e.path); err != nil {
		return rec, err
	}
	f, err := r.Open(e.path)
	if err != nil {
		return rec, err
	}
	defer f.Close()
	readLimit := max(s.opts.MaxFileBytes, DefaultMaxFileBytes)
	data, err := io.ReadAll(io.LimitReader(f, readLimit+1))
	if err != nil {
		return rec, err
	}
	if int64(len(data)) > readLimit {
		return rec, errors.New("tracestore: oversized record")
	}
	if err = json.Unmarshal(data, &rec); err != nil {
		return rec, fmt.Errorf("tracestore: corrupt record %s: %w", e.path, err)
	}
	if rec.Version != 1 || rec.TaskID <= 0 || rec.Attempt < 0 || rec.RecordedAt.IsZero() || len(rec.Steps) > 500 || len(rec.IdentityHash) > 256 || len(rec.ExecutionID) > 256 || rec.IdentityHash == "" || rec.ExecutionID == "" || batchDir(rec.Batch) != filepath.Dir(e.path) {
		return rec, fmt.Errorf("tracestore: invalid record metadata %s", e.path)
	}
	return rec, ctx.Err()
}
