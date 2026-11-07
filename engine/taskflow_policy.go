package engine

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/ydtg1993/papa/v3/models"
)

var ErrFlowInvalid = errors.New("task flow: invalid input or unsupported execution")
var ErrFlowConflict = errors.New("task flow: revision or task relationship conflict")

// FlowLane controls one set of stages. Depth permits one in-flight task per
// branch; Parallel bounds the number of branches. Breadth bounds task concurrency.
type FlowLane struct {
	Mode     string `json:"mode"`
	Parallel int    `json:"parallel"`
}
type FlowPolicy struct {
	Stages map[string]string   `json:"stages"`
	Lanes  map[string]FlowLane `json:"lanes"`
}

func (p FlowPolicy) Validate() error {
	if len(p.Stages) == 0 || len(p.Stages) > 32 || len(p.Lanes) == 0 || len(p.Lanes) > 32 {
		return ErrFlowInvalid
	}
	used := map[string]bool{}
	for stage, lane := range p.Stages {
		if strings.TrimSpace(stage) == "" || len(stage) > 128 || lane == "" || len(lane) > 64 {
			return ErrFlowInvalid
		}
		if _, ok := p.Lanes[lane]; !ok {
			return ErrFlowInvalid
		}
		used[lane] = true
	}
	for name, lane := range p.Lanes {
		if !used[name] || (lane.Mode != "breadth" && lane.Mode != "depth") || lane.Parallel < 1 || lane.Parallel > 64 {
			return ErrFlowInvalid
		}
	}
	return nil
}

// flowEligible is deterministic over the persisted active tree. Parents remain
// active until their synchronous child submissions have completed and committed.
// Returning false never consumes a worker while waiting for another stage.
func flowEligible(policy FlowPolicy, rows []models.CrawlerTask, target models.CrawlerTask) (bool, string) {
	lane, ok := policy.Lanes[target.FlowLane]
	if !ok || policy.Stages[target.Stage] != target.FlowLane {
		return false, "invalid lane"
	}
	if target.Status != models.TaskStatusPending {
		return false, "not pending"
	}
	active := make([]models.CrawlerTask, 0)
	for _, r := range rows {
		if r.Status != models.TaskStatusPending && r.Status != models.TaskStatusProcessing {
			continue
		}
		if r.ID == target.PID {
			return false, "parent still producing children"
		}
		if r.FlowLane == target.FlowLane {
			active = append(active, r)
		}
	}
	if lane.Mode == "breadth" {
		depth := strings.Count(target.FlowPath, "/")
		running := 0
		for _, r := range active {
			if strings.Count(r.FlowPath, "/") < depth {
				return false, "waiting for shallower tasks"
			}
			if r.Status == models.TaskStatusProcessing {
				running++
			}
		}
		if running >= lane.Parallel {
			return false, "lane concurrency limit"
		}
		return true, "ready"
	}
	branches := map[uint]bool{}
	occupied := map[uint]bool{}
	for _, r := range active {
		branches[r.FlowBranch] = true
		if r.Status == models.TaskStatusProcessing {
			occupied[r.FlowBranch] = true
		}
	}
	if occupied[target.FlowBranch] {
		return false, "branch task running"
	}
	// During a live concurrency reduction, old tasks finish; no new branch starts.
	if len(occupied) >= lane.Parallel {
		return false, "draining active branches"
	}
	ids := make([]uint, 0, len(branches))
	for id := range branches {
		if !occupied[id] {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	selected := false
	for i, id := range ids {
		if i >= lane.Parallel-len(occupied) {
			break
		}
		if id == target.FlowBranch {
			selected = true
		}
	}
	if !selected {
		return false, "waiting for earlier branch"
	}
	for _, r := range active {
		if r.FlowBranch == target.FlowBranch && r.FlowPath < target.FlowPath {
			return false, "waiting for preceding subtree"
		}
	}
	return true, "ready"
}

func flowPath(parent string, id uint) string { return fmt.Sprintf("%s%020d/", parent, id) }
