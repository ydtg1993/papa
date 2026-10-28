package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/pkg/events"
)

// 事件流开启时：按配置写 log.dir/events.jsonl，一行一条，带 run_id；关闭时排空落盘。
func TestNewEventEmitterWritesJSONL(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Log.Dir = dir
	cfg.Log.MaxSize = 10
	cfg.Obs.Events.Enabled = true
	cfg.Obs.Events.Buffer = 8

	em := newEventEmitter(cfg, "r-test")
	em.Emit(events.Event{Type: events.TypeRunStarted})
	em.Emit(events.Event{Type: events.TypeRunStopped})

	closer, ok := em.(events.Closer)
	if !ok {
		t.Fatalf("开启事件流时应返回可 Close 的 emitter，实得 %T", em)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := closer.Close(ctx); err != nil {
		t.Fatalf("Close = %v", err)
	}

	b, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatalf("读事件文件失败：%v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("应有 2 行事件，实得 %d：%q", len(lines), string(b))
	}
	for i, line := range lines {
		if !strings.Contains(line, `"run_id":"r-test"`) {
			t.Fatalf("第 %d 行缺 run_id：%s", i+1, line)
		}
		if !strings.Contains(line, `"seq":`) {
			t.Fatalf("第 %d 行缺 seq：%s", i+1, line)
		}
	}
}

// 事件流关闭时 App.Events() 返回 NopEmitter：调用方无需判空。
func TestEventsReturnsNopWhenDisabled(t *testing.T) {
	a := &App{}
	a.Events().Emit(events.Event{Type: events.TypeRunStarted}) // 不 panic 即可
}

// run_id 每次不同，且带时间戳前缀。
func TestNewRunIDUnique(t *testing.T) {
	a, b := newRunID(), newRunID()
	if a == b {
		t.Fatalf("两次 run_id 相同：%s", a)
	}
	if !strings.HasPrefix(a, "r-") {
		t.Fatalf("run_id 前缀不对：%s", a)
	}
}
