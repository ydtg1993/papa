package crawlrule

import (
	"context"
	"errors"
)

// ErrInvalidVersion means a stored version failed schema or integrity checks.
// Catalog implementations wrap this error for permanent content failures, while
// returning database, network and context errors without this classification.
var ErrInvalidVersion = errors.New("crawlrule: stored version is invalid")

// Reference identifies immutable rule content. Published tasks keep this exact
// ID/version even after a later publication or rollback.
type Reference struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

// ReleaseSnapshot is one atomically published stage binding set for a site.
// Callers own Bindings; implementations must return a detached map.
type ReleaseSnapshot struct {
	ID         string               `json:"id"`
	Site       string               `json:"site"`
	Generation uint64               `json:"generation"`
	Bindings   map[string]Reference `json:"bindings"`
}

// Catalog is the read side of a durable publisher. Resolve accepts only versions
// that have been successfully published, including older pinned versions.
// Snapshot resolves the whole site's binding set in one consistent read.
// Implementations must be safe for concurrent use and honor context cancellation.
// Resolve classifies missing/unpublished versions with ErrVersionNotFound or
// ErrUnverifiedRule, and corrupt/unsupported stored content with ErrInvalidVersion.
type Catalog interface {
	Resolve(context.Context, string, string) (*Compiled, error)
	Snapshot(context.Context, string) (ReleaseSnapshot, error)
}
