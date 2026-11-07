package events

import (
	"encoding/json"
	"github.com/ydtg1993/papa/v4/pkg/redact"
	"io"
	"sync"
)

// JSONLSink 把 Event 序列化成一行 JSON 写到一个 io.WriteCloser。
// 每行用一次 Write 写完整（含换行），避免一行被拆到轮转边界两侧。
type JSONLSink struct {
	w      io.WriteCloser
	mu     sync.Mutex
	closed bool
}

// NewJSONLSink 用一个已打开的写入端构造 sink；sink 拥有该写入端的关闭责任。
func NewJSONLSink(w io.WriteCloser) *JSONLSink {
	return &JSONLSink{w: w}
}

// Write 写出一行 JSON（含换行）。并发调用安全。
func (s *JSONLSink) Write(e Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 直接 Marshal + 一次 Write（json.Encoder.Encode 不保证单次 Write）。
	b, err := marshalDiagnosticEvent(e)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = s.w.Write(b)
	return err
}

// Metric dictionaries use application-defined names, not credential field
// names. Preserve their keys and concrete numeric/snapshot schemas even when a
// counter or queue is called "token" or "session". Mask snapshot text in
// detached values; the original maps remain untouched.
func marshalDiagnosticEvent(e Event) ([]byte, error) {
	counts, stages, queues := e.Counts, e.Stages, e.Queues
	e.Counts, e.Stages, e.Queues = nil, nil, nil
	raw, err := redact.JSON(e)
	if err != nil {
		return nil, err
	}
	var diagnostic Event
	if err := json.Unmarshal(raw, &diagnostic); err != nil {
		return nil, err
	}
	diagnostic.Counts = counts
	if stages != nil {
		diagnostic.Stages = make(map[string]StageSnapshot, len(stages))
		for name, snapshot := range stages {
			snapshot.Site, snapshot.Stage = redact.Text(snapshot.Site), redact.Text(snapshot.Stage)
			diagnostic.Stages[name] = snapshot
		}
	}
	if queues != nil {
		diagnostic.Queues = make(map[string]QueueSnapshot, len(queues))
		for name, snapshot := range queues {
			snapshot.LastError = redact.Text(snapshot.LastError)
			diagnostic.Queues[name] = snapshot
		}
	}
	return json.Marshal(diagnostic)
}

// Close 关闭底层写入端；重复调用幂等（第二次返回 nil）。
func (s *JSONLSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.w.Close()
}
