package access

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

type whitelistContextReaderFunc func(context.Context, string) ([]string, error)

func (f whitelistContextReaderFunc) LoadStringsContext(ctx context.Context, key string) ([]string, error) {
	return f(ctx, key)
}

func TestLoadWhitelistContextPreservesFailurePolicyExceptCancellation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		wantErr bool
	}{
		{"ordinary failure remains fail open", errors.New("db unavailable"), false},
		{"wrapped cancellation is not a whitelist", fmt.Errorf("read: %w", context.Canceled), true},
		{"deadline is not a whitelist", context.DeadlineExceeded, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			log := logrus.New()
			log.SetOutput(&output)
			reader := whitelistContextReaderFunc(func(context.Context, string) ([]string, error) { return nil, tc.err })
			entries, err := LoadWhitelistContext(context.Background(), reader, log)
			if len(entries) != 0 || (err != nil) != tc.wantErr {
				t.Fatalf("entries=%v error=%v", entries, err)
			}
			if logged := strings.Contains(output.String(), "不限制来源"); logged == tc.wantErr {
				t.Fatalf("cancellation must not log fail open; output=%q", output.String())
			}
		})
	}
}

func TestWhitelistReloaderCancelsReaderAndWaitsForExit(t *testing.T) {
	entered := make(chan context.Context, 1)
	exited := make(chan struct{})
	var published atomic.Int32
	reader := whitelistContextReaderFunc(func(ctx context.Context, _ string) ([]string, error) {
		entered <- ctx
		<-ctx.Done()
		close(exited)
		return nil, ctx.Err()
	})
	r := NewWhitelistReloader(reader, silentLogger{}, func([]string) { published.Add(1) }, time.Millisecond)
	if !r.Start(context.Background()) || r.Start(context.Background()) {
		t.Fatal("Start must own exactly one loop")
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("reader did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !r.Stop(ctx) {
		t.Fatal("context-aware reader did not drain")
	}
	select {
	case <-exited:
	default:
		t.Fatal("Stop returned before the reader exited")
	}
	if published.Load() != 0 || r.Start(context.Background()) {
		t.Fatal("stopped reloader published or restarted")
	}
	if !r.Stop(ctx) {
		t.Fatal("second Stop must observe the completed drain")
	}
}

func TestWhitelistReloaderReportsUndrainedReadAndDiscardsLateResult(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var published atomic.Int32
	reader := whitelistContextReaderFunc(func(context.Context, string) ([]string, error) {
		close(entered)
		<-release // Deliberately ignores cancellation, like a broken driver.
		return []string{"192.0.2.1"}, nil
	})
	r := NewWhitelistReloader(reader, silentLogger{}, func([]string) { published.Add(1) }, time.Millisecond)
	r.Start(context.Background())
	t.Cleanup(func() {
		close(release)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if !r.Stop(ctx) || published.Load() != 0 {
			t.Error("late read must drain without publishing after cancellation")
		}
	})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("reader did not start")
	}
	r.Cancel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r.Stop(ctx) {
		t.Fatal("blocked reader was reported drained; host might close its database")
	}
	if published.Load() != 0 {
		t.Fatal("shutdown published a whitelist")
	}
}

func TestWhitelistReloaderPublishesPeriodicChangesAndFailureFallback(t *testing.T) {
	var reads atomic.Int32
	reader := whitelistContextReaderFunc(func(context.Context, string) ([]string, error) {
		if reads.Add(1) == 1 {
			return []string{"192.0.2.1"}, nil
		}
		return nil, errors.New("database unavailable")
	})
	publications := make(chan []string, 8)
	r := NewWhitelistReloader(reader, silentLogger{}, func(entries []string) { publications <- entries }, time.Millisecond)
	r.Start(context.Background())
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if !r.Stop(ctx) {
			t.Error("reloader did not stop")
		}
	}()
	for i := 0; i < 2; i++ {
		select {
		case entries := <-publications:
			if i == 0 && (len(entries) != 1 || entries[0] != "192.0.2.1") {
				t.Fatalf("initial periodic value=%v", entries)
			}
			if i == 1 && len(entries) != 0 {
				t.Fatalf("ordinary failure must retain fail-open behavior, got %v", entries)
			}
		case <-time.After(time.Second):
			t.Fatal("periodic load did not publish")
		}
	}
}

func TestWhitelistReloaderStopBeforeStart(t *testing.T) {
	r := NewWhitelistReloader(nil, silentLogger{}, nil, time.Hour)
	if !r.Stop(context.Background()) || r.Start(context.Background()) {
		t.Fatal("stop before start must drain and prevent later startup")
	}
}

func TestWhitelistReloaderWaitsForInflightPublication(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	reader := whitelistContextReaderFunc(func(context.Context, string) ([]string, error) {
		return []string{"192.0.2.1"}, nil
	})
	r := NewWhitelistReloader(reader, silentLogger{}, func([]string) {
		close(entered)
		<-release
	}, time.Millisecond)
	r.Start(context.Background())
	t.Cleanup(func() {
		close(release)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if !r.Stop(ctx) {
			t.Error("released publication did not drain")
		}
	})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("publication did not start")
	}
	stopped := make(chan bool, 1)
	go func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		stopped <- r.Stop(ctx)
	}()
	select {
	case drained := <-stopped:
		if drained {
			t.Fatal("pending publication was reported drained")
		}
	case <-time.After(time.Second):
		t.Fatal("Cancel or Stop blocked beyond the supplied budget")
	}
}
