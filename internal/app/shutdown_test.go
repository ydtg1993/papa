package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestShutdownExtensionsDrainBeforeSharedResources(t *testing.T) {
	a := &App{}
	var order []int
	a.UseShutdown(func(context.Context) error { order = append(order, 1); return nil })
	a.UseShutdown(func(context.Context) error { order = append(order, 2); return nil })
	if !a.stopExtensions(time.Second) || !reflect.DeepEqual(order, []int{2, 1}) {
		t.Fatal(order)
	}
}
func TestShutdownExtensionsFailureAndTimeoutRetainSharedResources(t *testing.T) {
	for _, kind := range []string{"error", "panic", "timeout"} {
		t.Run(kind, func(t *testing.T) {
			a := &App{}
			finished := make(chan struct{})
			a.UseShutdown(func(ctx context.Context) error {
				defer close(finished)
				switch kind {
				case "error":
					return errors.New("private")
				case "panic":
					panic("private")
				default:
					<-ctx.Done()
					return ctx.Err()
				}
			})
			if a.stopExtensions(10 * time.Millisecond) {
				t.Fatal("shared resources would close")
			}
			<-finished
		})
	}
}
