package engine

import (
	"testing"
	"time"

	"github.com/ydtg1993/papa/v2/config"
)

func TestEngineQueueConfigGetters(t *testing.T) {
	e := &Engine{cfg: &config.Config{
		ErrorQueue: config.ErrorQueueConfig{
			Enabled: true, Interval: 10 * time.Minute,
			WorkerCount: 2, MaxRetry: 3, BatchSize: 1000,
		},
		RecoverQueue: config.RecoverQueueConfig{
			Enabled: true, WorkerCount: 2, BatchSize: 1000,
		},
		RepeatQueue: config.RepeatQueueConfig{
			Enabled: true, Interval: 10 * time.Minute,
			WorkerCount: 2, BatchSize: 1000,
		},
	}}
	e.runtime.Store(&config.RuntimeConfig{})

	// 无覆盖：回退基础配置
	if got := e.errorQueueConfig(); !got.Enabled || got.Interval != 10*time.Minute || got.WorkerCount != 2 || got.MaxRetry != 3 || got.BatchSize != 1000 {
		t.Fatalf("errorQueueConfig base = %+v", got)
	}
	if got := e.recoverQueueConfig(); !got.Enabled || got.WorkerCount != 2 || got.BatchSize != 1000 {
		t.Fatalf("recoverQueueConfig base = %+v", got)
	}
	if got := e.repeatQueueConfig(); !got.Enabled || got.Interval != 10*time.Minute || got.WorkerCount != 2 || got.BatchSize != 1000 {
		t.Fatalf("repeatQueueConfig base = %+v", got)
	}

	// 覆盖生效
	eoff := false
	eint := config.Duration{Duration: 3 * time.Minute}
	ew, em, eb := 5, 7, 500
	roff := false
	rw, rb := 3, 300
	poff := true
	pint := config.Duration{Duration: 5 * time.Minute}
	pw, pb := 4, 200
	e.runtime.Store(&config.RuntimeConfig{
		ErrorQueue: config.RuntimeErrorQueueConfig{
			Enabled: &eoff, Interval: &eint,
			WorkerCount: &ew, MaxRetry: &em, BatchSize: &eb,
		},
		RecoverQueue: config.RuntimeRecoverQueueConfig{
			Enabled: &roff, WorkerCount: &rw, BatchSize: &rb,
		},
		RepeatQueue: config.RuntimeRepeatQueueConfig{
			Enabled: &poff, Interval: &pint,
			WorkerCount: &pw, BatchSize: &pb,
		},
	})

	if got := e.errorQueueConfig(); got.Enabled || got.Interval != 3*time.Minute || got.WorkerCount != 5 || got.MaxRetry != 7 || got.BatchSize != 500 {
		t.Fatalf("errorQueueConfig overlay = %+v", got)
	}
	if got := e.recoverQueueConfig(); got.Enabled || got.WorkerCount != 3 || got.BatchSize != 300 {
		t.Fatalf("recoverQueueConfig overlay = %+v", got)
	}
	if got := e.repeatQueueConfig(); !got.Enabled || got.Interval != 5*time.Minute || got.WorkerCount != 4 || got.BatchSize != 200 {
		t.Fatalf("repeatQueueConfig overlay = %+v", got)
	}
}
