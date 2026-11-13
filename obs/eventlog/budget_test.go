package eventlog

import (
	"compress/gzip"
	"context"
	"errors"
	"github.com/ydtg1993/papa/v4/obs/inputbudget"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadContextResourceBoundaries(t *testing.T) {
	raw := []byte("{\"v\":1,\"seq\":1,\"run_id\":\"r\"}\n{\"v\":1,\"seq\":2,\"run_id\":\"r\"}\n")
	for _, gz := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "events")
		if gz {
			path += ".gz"
		}
		f, e := os.Create(path)
		if e != nil {
			t.Fatal(e)
		}
		if gz {
			w := gzip.NewWriter(f)
			w.Write(raw)
			w.Close()
		} else {
			f.Write(raw)
		}
		f.Close()
		for _, tc := range []struct {
			name string
			l    inputbudget.Limits
			bad  bool
		}{
			{"exact", inputbudget.Limits{MaxBytes: int64(len(raw)), MaxEvents: 2, MaxDedupKeys: 2, MaxRetainedBytes: int64(len(raw))}, false},
			{"bytes", inputbudget.Limits{MaxBytes: int64(len(raw) - 1)}, true},
			{"events", inputbudget.Limits{MaxEvents: 1}, true},
			{"keys", inputbudget.Limits{MaxDedupKeys: 1}, true},
			{"retained", inputbudget.Limits{MaxRetainedBytes: int64(len(raw) - 1)}, true},
			{"line", inputbudget.Limits{MaxLineBytes: 3}, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				r, e := ReadContext(context.Background(), []string{path}, time.Time{}, time.Time{}, "", tc.l)
				if errors.Is(e, inputbudget.ErrLimit) != tc.bad {
					t.Fatalf("gzip=%v err=%v", gz, e)
				}
				if tc.bad && len(r.Events) != 0 {
					t.Fatal("partial result")
				}
			})
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, e = ReadContext(ctx, []string{path}, time.Time{}, time.Time{}, "", inputbudget.Limits{})
		if !errors.Is(e, context.Canceled) {
			t.Fatal(e)
		}
	}
}
func TestReadContextRunFilterPreservesDuplicateMetadata(t *testing.T) {
	p := filepath.Join(t.TempDir(), "events")
	os.WriteFile(p, []byte("{\"v\":1,\"seq\":1,\"run_id\":\"other\"}\n{\"v\":1,\"seq\":1,\"run_id\":\"other\"}\n{\"v\":1,\"seq\":2,\"run_id\":\"wanted\"}\ninvalid\n"), 0600)
	r, e := ReadContext(context.Background(), []string{p}, time.Time{}, time.Time{}, "wanted", inputbudget.Limits{})
	if e != nil || len(r.Events) != 1 || r.Duplicates != 1 || r.Unparsed != 1 {
		t.Fatalf("%+v %v", r, e)
	}
}
