package workerpool

import "testing"

func TestNewWorkerPoolRejectsInvalidDimensions(t *testing.T) {
	for _, dims := range [][2]int{{0, 1}, {-1, 1}, {1, 0}, {1, -1}} {
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("dimensions %v panic: %v", dims, p)
				}
			}()
			if p := NewWorkerPool[*testTask](dims[0], dims[1], 1); p != nil {
				t.Errorf("dimensions %v created invalid pool", dims)
			}
		}()
	}
}
