package queues

import (
	"encoding/json"
	"github.com/ydtg1993/papa/v4/admin/access"
	"github.com/ydtg1993/papa/v4/admin/auth"
	"github.com/ydtg1993/papa/v4/engine"
	"net/http"
	"time"
)

type Logger interface{ Errorf(string, ...any) }
type QueueProcessEvent struct {
	Queue     string // 队列名：engine.QueueRepeat / engine.QueueError（与后台那一行的名字一致）
	Operator  string // 谁点的（鉴权中间件解析令牌得到；未鉴权/测试构造的请求为空）
	IP        string // 直连来源 IP（与白名单同一取法，不信 X-Forwarded-For）
	Processed int    // 实际处理条数
	Err       error  // nil = 成功
	At        time.Time
}
type Options struct {
	ProcessErrorQueue  func() (int, error)
	ProcessRepeatQueue func() (int, error)
	OnQueueProcess     func(QueueProcessEvent)
}
type Handler struct {
	cfg    Options
	logger Logger
}

func New(cfg Options, logger Logger) *Handler { return &Handler{cfg: cfg, logger: logger} }
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Handler) auditQueueProcess(r *http.Request, queue string, processed int, err error) {
	if s.cfg.OnQueueProcess == nil {
		return
	}
	s.cfg.OnQueueProcess(QueueProcessEvent{
		Queue:     queue,
		Operator:  auth.OperatorFrom(r.Context()),
		IP:        access.ClientIP(r).String(),
		Processed: processed,
		Err:       err,
		At:        time.Now(),
	})
}

func (s *Handler) Error(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.ProcessErrorQueue == nil {
		http.Error(w, "error queue not configured", http.StatusNotFound)
		return
	}
	count, err := s.cfg.ProcessErrorQueue()
	s.auditQueueProcess(r, engine.QueueError, count, err)
	if err != nil {
		s.logger.Errorf("process error queue: %s", err.Error())
		http.Error(w, "process error queue failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"status": "ok", "processed": count})
}

func (s *Handler) Repeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.ProcessRepeatQueue == nil {
		http.Error(w, "repeat queue not configured", http.StatusNotFound)
		return
	}
	count, err := s.cfg.ProcessRepeatQueue()
	s.auditQueueProcess(r, engine.QueueRepeat, count, err)
	if err != nil {
		s.logger.Errorf("process repeat queue: %s", err.Error())
		http.Error(w, "process repeat queue failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"status": "ok", "repolled": count})
}
