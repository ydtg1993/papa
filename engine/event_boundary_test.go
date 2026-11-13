package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/events"
)

func TestEngineCustomEventBoundaryNeverWaitsForCallback(t *testing.T) {
	e := &Engine{}
	entered, release := make(chan struct{}), make(chan struct{})
	e.SetEventsEmitter(stagePoolEventFunc(func(events.Event) { close(entered); <-release }))
	var releaseOnce sync.Once
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		drainTestEvents(t, e)
	})
	if e.events != e.breakers.events {
		t.Fatal("breaker bypassed engine observation boundary")
	}
	e.emit(events.Event{})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("consumer not started")
	}
	closed := make(chan struct{})
	go func() { e.requestEventsClose(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("engine shutdown waited for its event callback")
	}
	releaseOnce.Do(func() { close(release) })
	drainTestEvents(t, e)
}

func TestEngineDoesNotCloseAppOwnedBoundary(t *testing.T) {
	e := &Engine{}
	out := make(chan struct{}, 1)
	safe := events.Isolate(stagePoolEventFunc(func(events.Event) { out <- struct{}{} }))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := safe.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	e.SetEventsEmitter(safe)
	e.requestEventsClose()
	safe.Emit(events.Event{})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := safe.Close(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-out:
	default:
		t.Fatal("engine prematurely closed App-owned boundary")
	}
}
