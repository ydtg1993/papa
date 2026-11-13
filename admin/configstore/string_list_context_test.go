package configstore

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type contextStringReaderFunc func(context.Context, string) ([]string, error)

func (f contextStringReaderFunc) LoadStringsContext(ctx context.Context, key string) ([]string, error) {
	return f(ctx, key)
}

func TestCachedStringListReaderRejectsCanceledCacheHit(t *testing.T) {
	inner := &countingStringListReader{values: []string{"192.0.2.1"}}
	r := NewCachedStringListReader(inner, time.Minute)
	if _, err := r.LoadStrings(KeyIPWhitelist); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if entries, err := r.LoadStringsContext(ctx, KeyIPWhitelist); !errors.Is(err, context.Canceled) || entries != nil {
		t.Fatalf("canceled cache hit=%v, %v", entries, err)
	}
	if inner.callCount() != 1 {
		t.Fatal("canceled call queried the source")
	}
}

func TestCachedStringListReaderDoesNotCacheLateCanceledResult(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	inner := contextStringReaderFunc(func(context.Context, string) ([]string, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
			return []string{"old"}, nil
		}
		return []string{"new"}, nil
	})
	r := NewCachedContextStringListReader(inner, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := r.LoadStringsContext(ctx, KeyIPWhitelist); result <- err }()
	<-entered
	cancel()
	close(release)
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("late result error=%v", err)
	}
	entries, err := r.LoadStrings(KeyIPWhitelist)
	if err != nil || len(entries) != 1 || entries[0] != "new" || calls.Load() != 2 {
		t.Fatalf("canceled read populated cache: entries=%v, error=%v, calls=%d", entries, err, calls.Load())
	}
}

func TestCachedStringListReaderInvalidationSupersedesInflightRead(t *testing.T) {
	for _, all := range []bool{false, true} {
		name := "one key"
		if all {
			name = "all keys"
		}
		t.Run(name, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			inner := contextStringReaderFunc(func(context.Context, string) ([]string, error) {
				if calls.Add(1) == 1 {
					close(entered)
					<-release
					return []string{"before save"}, nil
				}
				return []string{"after save"}, nil
			})
			r := NewCachedContextStringListReader(inner, time.Minute)
			result := make(chan []string, 1)
			go func() { entries, _ := r.LoadStrings(KeyIPWhitelist); result <- entries }()
			<-entered
			if all {
				r.InvalidateAll()
			} else {
				r.Invalidate(KeyIPWhitelist)
			}
			close(release)
			if entries := <-result; len(entries) != 1 || entries[0] != "after save" {
				t.Fatalf("old in-flight read escaped invalidation: %v", entries)
			}
			entries, err := r.LoadStrings(KeyIPWhitelist)
			if err != nil || len(entries) != 1 || entries[0] != "after save" || calls.Load() != 2 {
				t.Fatalf("stale cache refill: entries=%v, error=%v, calls=%d", entries, err, calls.Load())
			}
		})
	}
}
