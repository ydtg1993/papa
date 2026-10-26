package configstore

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

// countingStringListReader 记录回源次数，并可注入返回值/错误。
type countingStringListReader struct {
	mu     sync.Mutex
	calls  int
	values []string
	err    error
}

func (r *countingStringListReader) LoadStrings(string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return append([]string(nil), r.values...), r.err
}

func (r *countingStringListReader) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func TestCachedStringListReaderServesFromCacheWithinTTL(t *testing.T) {
	inner := &countingStringListReader{values: []string{"10.0.0.0/8"}}
	reader := NewCachedStringListReader(inner, time.Minute)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	reader.now = func() time.Time { return now }

	first, err := reader.LoadStrings(KeyIPWhitelist)
	if err != nil || !reflect.DeepEqual(first, []string{"10.0.0.0/8"}) {
		t.Fatalf("first read = %v, %v", first, err)
	}
	now = now.Add(30 * time.Second) // TTL 内
	if _, err := reader.LoadStrings(KeyIPWhitelist); err != nil {
		t.Fatal(err)
	}
	if got := inner.callCount(); got != 1 {
		t.Fatalf("inner calls = %d, want 1（TTL 内不该回源）", got)
	}

	now = now.Add(31 * time.Second) // 过期
	inner.values = []string{"192.168.1.0/24"}
	got, err := reader.LoadStrings(KeyIPWhitelist)
	if err != nil || !reflect.DeepEqual(got, []string{"192.168.1.0/24"}) {
		t.Fatalf("expired read = %v, %v", got, err)
	}
	if c := inner.callCount(); c != 2 {
		t.Fatalf("inner calls = %d, want 2（过期后回源一次）", c)
	}
}

func TestCachedStringListReaderInvalidateForcesReload(t *testing.T) {
	inner := &countingStringListReader{values: []string{"10.0.0.0/8"}}
	reader := NewCachedStringListReader(inner, time.Minute)
	if _, err := reader.LoadStrings(KeyIPWhitelist); err != nil {
		t.Fatal(err)
	}
	reader.Invalidate(KeyIPWhitelist)
	inner.values = []string{"192.168.1.1"}
	got, err := reader.LoadStrings(KeyIPWhitelist)
	if err != nil || !reflect.DeepEqual(got, []string{"192.168.1.1"}) {
		t.Fatalf("after invalidate = %v, %v", got, err)
	}
	if c := inner.callCount(); c != 2 {
		t.Fatalf("inner calls = %d, want 2", c)
	}
}

func TestCachedStringListReaderDoesNotCacheErrors(t *testing.T) {
	inner := &countingStringListReader{err: errors.New("db down")}
	reader := NewCachedStringListReader(inner, time.Minute)
	for i := 0; i < 2; i++ {
		if _, err := reader.LoadStrings(KeyIPWhitelist); err == nil {
			t.Fatalf("read %d: error was swallowed", i+1)
		}
	}
	if c := inner.callCount(); c != 2 {
		t.Fatalf("inner calls = %d, want 2（错误不缓存，每次都要重试）", c)
	}

	inner.mu.Lock()
	inner.err = nil
	inner.values = []string{"10.0.0.0/8"}
	inner.mu.Unlock()
	got, err := reader.LoadStrings(KeyIPWhitelist)
	if err != nil || !reflect.DeepEqual(got, []string{"10.0.0.0/8"}) {
		t.Fatalf("recovered read = %v, %v", got, err)
	}
}

func TestCachedStringListReaderReturnsCopy(t *testing.T) {
	inner := &countingStringListReader{values: []string{"a", "b"}}
	reader := NewCachedStringListReader(inner, time.Minute)
	got, err := reader.LoadStrings(KeyIPWhitelist)
	if err != nil {
		t.Fatal(err)
	}
	got[0] = "mutated"
	again, err := reader.LoadStrings(KeyIPWhitelist)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again, []string{"a", "b"}) {
		t.Fatalf("cache was polluted by caller mutation: %v", again)
	}
}

func TestCachedStringListReaderZeroTTLPassthrough(t *testing.T) {
	inner := &countingStringListReader{values: []string{"a"}}
	reader := NewCachedStringListReader(inner, 0)
	for i := 0; i < 2; i++ {
		if _, err := reader.LoadStrings(KeyIPWhitelist); err != nil {
			t.Fatal(err)
		}
	}
	if c := inner.callCount(); c != 2 {
		t.Fatalf("inner calls = %d, want 2（ttl<=0 透传）", c)
	}
}
