package runtime

import (
	"context"

	"agentd/internal/tasks"
)

func (r *Runtime) DeliverNotes(ctx context.Context, lease tasks.Lease, runID string, seq int) (int, error) {
	return r.deliverNotes(ctx, lease, runID, seq)
}
