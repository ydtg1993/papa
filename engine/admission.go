package engine

import "errors"

// ErrEngineStopped means task admission has closed; no new persistence starts.
var ErrEngineStopped = errors.New("engine stopped accepting tasks")

// beginTaskAdmission reserves persistence ownership before any database access.
// The delivery coordinator owns the gate and in-flight count; Stop closes and
// waits for that same owner before allowing shared storage to be closed.
func (e *Engine) beginTaskAdmission() error {
	if !e.deliveryState().Begin() {
		return ErrEngineStopped
	}
	return nil
}
func (e *Engine) endTaskAdmission() { e.deliveryState().End() }
