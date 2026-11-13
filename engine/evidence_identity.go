package engine

import (
	"context"
	"github.com/google/uuid"
)

// evidenceExecutionKey identifies one successful claim, including its retries.
// It is observational only: taskstore remains the owner of terminal fencing.
type evidenceExecutionKey struct{}

func withEvidenceExecution(ctx context.Context) context.Context {
	return context.WithValue(ctx, evidenceExecutionKey{}, uuid.NewString())
}
func evidenceExecutionID(ctx context.Context) string {
	if id, ok := ctx.Value(evidenceExecutionKey{}).(string); ok {
		return id
	}
	// Direct attempt invocations have no claim scope and are separate executions.
	return uuid.NewString()
}
