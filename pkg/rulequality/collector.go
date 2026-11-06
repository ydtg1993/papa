package rulequality

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"sync"
	"time"
)

const (
	DefaultCollectorTimeout = 500 * time.Millisecond
	MaxCollectorTimeout     = 3 * time.Second
)

// Collector errors deliberately omit recorder messages and panic values, which
// may contain database credentials, raw URLs or user data.
var (
	ErrObservationWrite = errors.New("rulequality: observation write failed")
	ErrObservationPanic = errors.New("rulequality: observation recorder panicked")
)

type CollectorStats struct{ Attempted, Failed uint64 }

// Collector has no queue, goroutine or retry. Recorders are trusted host code and
// must honor their context; a context cannot forcibly interrupt an arbitrary Go
// function. Stats cover this collector's lifetime, not global production coverage.
type Collector struct {
	recorder  Recorder
	timeout   time.Duration
	statsMu   sync.Mutex
	attempted uint64
	failed    uint64
}

func NewCollector(recorder Recorder, timeout time.Duration) (*Collector, error) {
	if collectorNil(recorder) || timeout < 0 {
		return nil, ErrInvalid
	}
	if timeout == 0 {
		timeout = DefaultCollectorTimeout
	}
	if timeout > MaxCollectorTimeout {
		timeout = MaxCollectorTimeout
	}
	return &Collector{recorder: recorder, timeout: timeout}, nil
}

func (c *Collector) Observe(ctx context.Context, observation Observation) (err error) {
	if c == nil {
		return ErrInvalid
	}
	c.statsMu.Lock()
	c.attempted++
	c.statsMu.Unlock()
	defer func() {
		if recover() != nil {
			err = ErrObservationPanic
		}
		if err != nil {
			c.statsMu.Lock()
			c.failed++
			c.statsMu.Unlock()
		}
	}()
	if ctx == nil {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateObservation(observation); err != nil {
		return err
	}
	bounded, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	observation.Fields = append([]FieldStat(nil), observation.Fields...)
	_, writeErr := c.recorder.Record(bounded, observation)
	if err := bounded.Err(); err != nil {
		return err
	}
	if writeErr != nil {
		return ErrObservationWrite
	}
	return nil
}

func (c *Collector) Stats() CollectorStats {
	if c == nil {
		return CollectorStats{}
	}
	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	return CollectorStats{Attempted: c.attempted, Failed: c.failed}
}

type logicalKeyContext struct{}

// WithLogicalKey marks a host-defined logical collection attempt, so retries
// may be folded together. It retains only SHA256, never the supplied raw key.
// An empty key clears the parent's key. Keys should identify a specific round,
// not just a URL; otherwise unrelated successful collections would be folded.
func WithLogicalKey(ctx context.Context, rawKey string) context.Context {
	key := ""
	if rawKey != "" {
		hash := sha256.Sum256([]byte(rawKey))
		key = hex.EncodeToString(hash[:])
	}
	return context.WithValue(ctx, logicalKeyContext{}, key)
}

// LogicalKey returns an already-hashed key suitable for Observation.LogicalKey.
func LogicalKey(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	key, _ := ctx.Value(logicalKeyContext{}).(string)
	return key
}

func collectorNil(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	}
	return false
}
