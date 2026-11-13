// Package reportrunner owns a signal-only reporting sidecar: bounded admission,
// single-owner incremental counters, one latest pending report and one writer.
// It never borrows database handles or executes application callbacks. Stop is
// best effort within the caller budget; retained workers hold only owned values.
package reportrunner
