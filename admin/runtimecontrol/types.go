package runtimecontrol

import domain "github.com/ydtg1993/papa/v4/internal/runtimecontrol"

// Result is a policy outcome. Its identity matches the built-in policy service,
// while this public alias lets external hosts implement PolicyService.
type Result = domain.Result

// Update pins a complete policy document to its expected revision.
type Update = domain.Update

// Document contains the versioned policy overrides.
type Document = domain.Document

// HistoryRequest selects a historical preview or a field-level restoration.
type HistoryRequest = domain.HistoryRequest

// Kind identifies an outcome without coupling domain services to HTTP codes.
type Kind = domain.Kind

// Event is sanitized audit metadata emitted by a policy operation.
type Event = domain.Event

// Change describes one field difference in a history preview.
type Change = domain.Change

// Outcome constants are shared with the built-in service without conversion.
const (
	Success     = domain.Success
	Invalid     = domain.Invalid
	Conflict    = domain.Conflict
	Unavailable = domain.Unavailable
	NotFound    = domain.NotFound
)
