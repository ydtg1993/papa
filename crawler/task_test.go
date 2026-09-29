package crawler

import (
	"testing"
	"time"
)

func TestTaskUnique(t *testing.T) {
	task := &Task{URL: "https://example.com", Stage: "mock"}
	if got := task.Unique(); got != "mock|https://example.com" {
		t.Fatalf("default unique = %q", got)
	}
	task.IdempotencyKey = "catalog:cat:2"
	if got := task.Unique(); got != "catalog:cat:2" {
		t.Fatalf("idempotency key unique = %q", got)
	}
}

func TestTaskDeliverAt(t *testing.T) {
	task := &Task{}
	if !task.deliverAt().IsZero() {
		t.Fatal("no delay should be zero time")
	}

	task.Delay = time.Minute
	if task.deliverAt().IsZero() {
		t.Fatal("Delay should produce a non-zero deliver time")
	}

	// NotBefore 优先于 Delay
	at := time.Now().Add(2 * time.Minute)
	task.NotBefore = at
	if !task.deliverAt().Equal(at) {
		t.Fatal("NotBefore should take precedence over Delay")
	}
}
