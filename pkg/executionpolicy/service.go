package executionpolicy

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"time"
)

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string       { return e.Message }
func problem(code, msg string) error { return &Error{code, msg} }

var ErrConflict = problem("conflict", "configuration revision or operation ID conflict")
var ErrNotFound = problem("not_found", "configuration or operation not found")

type Update struct {
	ExpectedRevision uint64   `json:"expected_revision"`
	OperationID      string   `json:"operation_id"`
	Document         Document `json:"document"`
	Reason           string   `json:"reason"`
}
type Rollback struct {
	ExpectedRevision uint64 `json:"expected_revision"`
	OperationID      string `json:"operation_id"`
	TargetRevision   uint64 `json:"target_revision"`
	Reason           string `json:"reason"`
}
type History struct {
	Revision    uint64    `json:"revision"`
	Document    Document  `json:"document"`
	Operator    string    `json:"operator"`
	Reason      string    `json:"reason"`
	OperationID string    `json:"operation_id"`
	CreatedAt   time.Time `json:"created_at"`
}
type Operation struct {
	ID               string    `json:"operation_id"`
	Revision         uint64    `json:"revision"`
	Status           string    `json:"status"`
	Operator         string    `json:"operator"`
	Reason           string    `json:"reason"`
	ExpectedRevision uint64    `json:"expected_revision"`
	Document         Document  `json:"document"`
	CreatedAt        time.Time `json:"created_at"`
}
type View struct {
	Operator        string    `json:"operator"`
	UpdatedAt       time.Time `json:"updated_at"`
	Document        Document  `json:"document"`
	Revision        uint64    `json:"revision"`
	AppliedRevision uint64    `json:"applied_revision"`
	Status          string    `json:"status"`
	Runtime         any       `json:"runtime"`
	Sites           []string  `json:"sites"`
	Error           string    `json:"error,omitempty"`
}
type Preview struct {
	CurrentDocument Document `json:"current_document"`
	Runtime         any      `json:"runtime"`
	Document        Document `json:"document"`
	CurrentRevision uint64   `json:"current_revision"`
	Warnings        []string `json:"warnings"`
}

// Store saves the document, history and operation identity in a single transaction.
type Store interface {
	Load(context.Context) (History, error)
	Save(context.Context, Update, string) (History, error)
	History(context.Context, int) ([]History, error)
	Operation(context.Context, string) (Operation, error)
}

// Target owns runtime state. Apply must not wait for borrowed browsers or active incidents to finish.
type Target interface {
	Apply(context.Context, uint64, Document) error
	Status() any
}
type Service interface {
	Read(context.Context) (View, error)
	Validate(context.Context, Document) (Preview, error)
	Update(context.Context, Update, string) (View, error)
	History(context.Context, int) ([]History, error)
	Operation(context.Context, string) (Operation, error)
	Rollback(context.Context, Rollback, string) (View, error)
}
type Controller struct {
	writeMu           sync.Mutex // serializes commits/publication, never blocks state reads
	mu                sync.Mutex
	store             Store
	target            Target
	sites             []string
	doc               Document
	revision, applied uint64
	operator          string
	updatedAt         time.Time
	lastError         string
	applying          bool
	pending           *Update
	pendingOperator   string
	stopping          atomic.Bool
}

func New(store Store, target Target, sites []string) *Controller {
	return &Controller{store: store, target: target, sites: append([]string(nil), sites...)}
}
func (c *Controller) Initialize(ctx context.Context) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.stopping.Load() {
		return problem("stopping", "configuration service is stopping")
	}
	h, err := c.store.Load(ctx)
	if err != nil {
		return fmt.Errorf("load execution_policy (run explicit policy initialization): %w", err)
	}
	return c.apply(ctx, h)
}

// apply publishes saved state before invoking a component, so readers can see
// applying/degraded while the component acknowledges the immutable generation.
func (c *Controller) apply(ctx context.Context, h History) error {
	if h.Revision == 0 {
		return problem("invalid", "policy revision must be positive")
	}
	if err := h.Document.Validate(c.sites); err != nil {
		return problem("invalid", err.Error())
	}
	c.mu.Lock()
	if h.Revision < c.revision {
		c.mu.Unlock()
		return problem("unavailable", "stored policy revision moved backwards")
	}
	if c.revision != 0 && h.Revision == c.revision && !reflect.DeepEqual(c.doc, h.Document) {
		c.mu.Unlock()
		return problem("unavailable", "stored policy content changed without revision")
	}
	c.doc = h.Document.Clone()
	c.revision = h.Revision
	c.operator = h.Operator
	c.updatedAt = h.CreatedAt
	c.pending = nil
	c.pendingOperator = ""
	if c.stopping.Load() {
		c.applying = false
		c.mu.Unlock()
		return nil
	}
	if c.applied == h.Revision {
		c.lastError = ""
		c.mu.Unlock()
		return nil
	}
	c.applying = true
	c.lastError = ""
	c.mu.Unlock()
	var err error
	if c.target != nil {
		err = c.target.Apply(ctx, h.Revision, h.Document.Clone())
	}
	c.mu.Lock()
	c.applying = false
	if err != nil {
		c.lastError = err.Error()
	} else {
		c.applied = h.Revision
		c.lastError = ""
	}
	c.mu.Unlock()
	if err != nil {
		return problem("unavailable", err.Error())
	}
	return nil
}
func (c *Controller) BeginShutdown() { c.stopping.Store(true) }

// Drain fences new writes and waits for the current public Go or HTTP write to
// finish. The caller must retain component/database resources if it times out.
func (c *Controller) Drain(ctx context.Context) error {
	c.BeginShutdown()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if c.writeMu.TryLock() {
			c.writeMu.Unlock()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
func (c *Controller) view() View {
	c.mu.Lock()
	status := "applied"
	if c.revision == 0 {
		status = "uninitialized"
	} else if c.applied != c.revision {
		status = "degraded"
	}
	if c.applying {
		status = "applying"
	}
	if c.pending != nil {
		status = "pending"
	}
	if c.stopping.Load() {
		status = "stopping"
	}
	v := View{Document: c.doc.Clone(), Revision: c.revision, AppliedRevision: c.applied, Status: status, Sites: append([]string(nil), c.sites...), Error: c.lastError, Operator: c.operator, UpdatedAt: c.updatedAt}
	c.mu.Unlock()
	if c.target != nil {
		v.Runtime = c.target.Status()
	}
	return v
}
func (c *Controller) Read(ctx context.Context) (View, error) { return c.view(), ctx.Err() }
func (c *Controller) Validate(ctx context.Context, d Document) (Preview, error) {
	if err := ctx.Err(); err != nil {
		return Preview{}, err
	}
	if err := d.Validate(c.sites); err != nil {
		return Preview{}, problem("invalid", err.Error())
	}
	current := c.view()
	return Preview{Document: d.Clone(), CurrentDocument: current.Document, Runtime: current.Runtime, CurrentRevision: current.Revision, Warnings: []string{"Active breaker incidents retain their frozen policy until completion; saving does not resume paused sites.", "Pool reduction drains idle/returned browsers; direct_pool_size=0 rejects new direct borrows.", "Expanding breaker windows or capacity cannot restore expired or discarded observations; the expanded evidence window must accumulate."}}, nil
}
func sameOperation(op Operation, u Update, operator string) bool {
	return op.ID == u.OperationID && op.ExpectedRevision == u.ExpectedRevision && op.Operator == operator && op.Reason == u.Reason && reflect.DeepEqual(op.Document.Clone(), u.Document.Clone())
}
func (c *Controller) reconcile(ctx context.Context, op Operation) (View, error) {
	h, err := c.store.Load(ctx)
	if err != nil {
		return c.view(), err
	}
	if h.Revision < op.Revision || (h.Revision == op.Revision && !reflect.DeepEqual(h.Document.Clone(), op.Document.Clone())) {
		return c.view(), problem("unavailable", "saved operation does not match current policy; reconciliation required")
	}
	applyCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = c.apply(applyCtx, h)
	return c.view(), err
}
func (c *Controller) Update(ctx context.Context, u Update, operator string) (View, error) {
	if c.stopping.Load() {
		return c.view(), problem("stopping", "configuration service is stopping")
	}
	if err := ctx.Err(); err != nil {
		return c.view(), err
	}
	if len(u.OperationID) < 8 || len(u.OperationID) > 128 || len(u.Reason) > 2048 || operator == "" {
		return View{}, problem("invalid", "operation_id (8..128 characters), authenticated operator and bounded reason required")
	}
	if err := u.Document.Validate(c.sites); err != nil {
		return View{}, problem("invalid", err.Error())
	}
	u.Document = u.Document.Clone()
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.stopping.Load() {
		return c.view(), problem("stopping", "configuration service is stopping")
	}
	if err := ctx.Err(); err != nil {
		return c.view(), err
	}
	c.mu.Lock()
	pending := c.pending
	pendingOperator := c.pendingOperator
	revision, applied := c.revision, c.applied
	c.mu.Unlock()
	if pending != nil {
		if pending.OperationID != u.OperationID {
			return c.view(), problem("unavailable", "save result unknown; reconcile the pending operation before another update")
		}
		if pendingOperator != operator || !reflect.DeepEqual(*pending, u) {
			return c.view(), ErrConflict
		}
	}
	if op, err := c.store.Operation(ctx, u.OperationID); err == nil {
		if !sameOperation(op, u, operator) {
			return c.view(), ErrConflict
		}
		return c.reconcile(ctx, op)
	} else if !errors.Is(err, ErrNotFound) {
		return c.view(), err
	}
	if pending != nil {
		return c.view(), problem("unavailable", "save result unknown; query operation_id before retrying")
	}
	if revision == 0 {
		return c.view(), problem("unavailable", "initialize the persisted execution policy before updating")
	}
	if applied != revision {
		return c.view(), problem("unavailable", "saved policy is not applied; reconcile before another update")
	}
	if u.ExpectedRevision != revision {
		return c.view(), ErrConflict
	}
	// Preserve the intent before entering Save: a transport error cannot establish
	// whether its transaction committed, so no subsequent write may overtake it.
	c.mu.Lock()
	pendingCopy := u
	pendingCopy.Document = u.Document.Clone()
	c.pending = &pendingCopy
	c.pendingOperator = operator
	c.mu.Unlock()
	saveCtx, saveCancel := context.WithTimeout(ctx, 5*time.Second)
	h, err := c.store.Save(saveCtx, u, operator)
	saveCancel()
	if err != nil {
		if errors.Is(err, ErrConflict) {
			c.mu.Lock()
			c.pending = nil
			c.pendingOperator = ""
			c.mu.Unlock()
			// A competing writer can have advanced the database; return its latest state.
			checkCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if latest, loadErr := c.store.Load(checkCtx); loadErr == nil {
				_ = c.apply(checkCtx, latest)
			}
			return c.view(), err
		}
		checkCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		op, checkErr := c.store.Operation(checkCtx, u.OperationID)
		if checkErr == nil && sameOperation(op, u, operator) {
			return c.reconcile(checkCtx, op)
		}
		c.mu.Lock()
		c.lastError = "save result unknown; query operation_id before retrying"
		c.mu.Unlock()
		return c.view(), problem("unavailable", "save result unknown; query operation_id before retrying")
	}
	applyCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = c.apply(applyCtx, h)
	return c.view(), err
}
func (c *Controller) History(ctx context.Context, limit int) ([]History, error) {
	if limit < 1 || limit > 100 {
		limit = 100
	}
	rows, err := c.store.History(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := append([]History(nil), rows...)
	for i := range out {
		out[i].Document = out[i].Document.Clone()
	}
	return out, nil
}
func (c *Controller) Operation(ctx context.Context, id string) (Operation, error) {
	op, err := c.store.Operation(ctx, id)
	if err != nil {
		return op, err
	}
	op.Document = op.Document.Clone()
	c.mu.Lock()
	op.Status = "saved"
	if c.applied == op.Revision {
		op.Status = "applied"
	} else if c.applied > op.Revision {
		op.Status = "superseded"
	}
	c.mu.Unlock()
	return op, nil
}
func (c *Controller) Rollback(ctx context.Context, r Rollback, operator string) (View, error) {
	hs, err := c.History(ctx, 100)
	if err != nil {
		return View{}, err
	}
	for _, h := range hs {
		if h.Revision == r.TargetRevision {
			return c.Update(ctx, Update{ExpectedRevision: r.ExpectedRevision, OperationID: r.OperationID, Document: h.Document, Reason: r.Reason}, operator)
		}
	}
	return View{}, ErrNotFound
}
