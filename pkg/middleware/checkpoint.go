package middleware

import (
	"sync"
	"time"
)

// Checkpoint owns all writes for one task. Notify coalesces; the authoritative
// completion facts remain in the task state. Close drains the final snapshot.
type Checkpoint struct {
	wake chan struct{}
	stop chan struct{}
	done chan struct{}
	once sync.Once
	err  error
}

func NewCheckpoint(save func() error) *Checkpoint {
	c := &Checkpoint{wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(c.done)
		timer := time.NewTicker(time.Second)
		defer timer.Stop()
		dirty := false
		write := func() {
			if e := save(); e != nil && c.err == nil {
				c.err = e
			}
			dirty = false
		}
		for {
			select {
			case <-c.wake:
				dirty = true
			case <-timer.C:
				if dirty {
					write()
				}
			case <-c.stop:
				write()
				return
			}
		}
	}()
	return c
}
func (c *Checkpoint) Notify() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}
func (c *Checkpoint) Close() error { c.once.Do(func() { close(c.stop) }); <-c.done; return c.err }
