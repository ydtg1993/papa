package events

import (
	"encoding/json"
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
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = s.w.Write(b)
	return err
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
