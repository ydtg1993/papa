package server

import (
	"context"
	"sort"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/agentadmin"
	"github.com/ydtg1993/papa/v4/pkg/agentstore"
)

// One bounded audit page, never a global total or an in-memory queue metric.
type agentActivitySnapshot struct {
	Runs    []agentadmin.RunView  `json:"runs"`
	Detail  *agentadmin.RunDetail `json:"detail,omitempty"`
	Partial bool                  `json:"partial"`
	Error   string                `json:"error,omitempty"`
}

func (s *Monitor) agentActivity(ctx context.Context) agentActivitySnapshot {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out := agentActivitySnapshot{Runs: []agentadmin.RunView{}}
	now := time.Now().UTC()
	page, err := s.cfg.AgentAdmin.Runs(ctx, agentstore.RunFilter{From: now.Add(-24 * time.Hour), Until: now})
	if err != nil {
		out.Error = "audit_unavailable"
		return out
	}
	out.Runs, out.Partial = page.Items, page.Next != ""
	// Audit IDs are opaque; their lexical order is not chronology.
	sort.SliceStable(out.Runs, func(i, j int) bool { return out.Runs[i].CreatedAt.After(out.Runs[j].CreatedAt) })
	if len(out.Runs) > 0 {
		detail, err := s.cfg.AgentAdmin.Run(ctx, out.Runs[0].ID, 0)
		if err != nil {
			out.Error = "steps_unavailable"
		} else {
			out.Detail = &detail
			// Detail is a newer, internally consistent audit snapshot. Use its
			// status/usage in the list as well if a running audit advanced.
			out.Runs[0] = detail.Run
		}
	}
	return out
}
