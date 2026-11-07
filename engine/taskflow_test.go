package engine

import (
	"github.com/ydtg1993/papa/v4/models"
	"testing"
)

func TestFlowDynamicBreadthDepthAndIndependentLanes(t *testing.T) {
	p := FlowPolicy{Stages: map[string]string{"catalog": "discover", "detail": "items", "content": "items"}, Lanes: map[string]FlowLane{"discover": {"breadth", 2}, "items": {"depth", 1}}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	a := models.CrawlerTask{ID: 1, Stage: "detail", FlowLane: "items", FlowBranch: 1, FlowPath: flowPath("", 1), Status: models.TaskStatusSuccess}
	b := models.CrawlerTask{ID: 2, Stage: "detail", FlowLane: "items", FlowBranch: 2, FlowPath: flowPath("", 2)}
	c := models.CrawlerTask{ID: 3, PID: 1, Stage: "content", FlowLane: "items", FlowBranch: 1, FlowPath: flowPath(a.FlowPath, 3)}
	d := models.CrawlerTask{ID: 4, Stage: "catalog", FlowLane: "discover", FlowBranch: 4, FlowPath: flowPath("", 4)}
	rows := []models.CrawlerTask{a, b, c, d}
	check := func(task models.CrawlerTask, want bool) {
		t.Helper()
		got, reason := flowEligible(p, rows, task)
		if got != want {
			t.Fatalf("id=%d allowed=%v want=%v reason=%s", task.ID, got, want, reason)
		}
	}
	check(b, false)
	check(c, true)
	check(d, true)
	p.Lanes["items"] = FlowLane{"breadth", 2}
	check(b, true)
	check(c, false)
	rows[1].Status = models.TaskStatusProcessing
	check(c, false)
	p.Lanes["items"] = FlowLane{"depth", 1}
	check(c, false) // no new claims until in-flight B drains
	rows[1].Status = models.TaskStatusSuccess
	check(c, true)
	rows[0].Status = models.TaskStatusProcessing
	check(c, false) // parent still publishing children
}

func TestFlowDepthPreorderAndParallelBranches(t *testing.T) {
	p := FlowPolicy{Stages: map[string]string{"x": "x"}, Lanes: map[string]FlowLane{"x": {"depth", 2}}}
	row := func(id, branch uint, path string) models.CrawlerTask {
		return models.CrawlerTask{ID: id, Stage: "x", FlowLane: "x", FlowBranch: branch, FlowPath: path}
	}
	a := row(3, 1, flowPath(flowPath("", 1), 3))
	b := row(4, 1, flowPath(flowPath("", 1), 4))
	grand := row(5, 1, flowPath(a.FlowPath, 5))
	other := row(2, 2, flowPath("", 2))
	a.Status = models.TaskStatusSuccess
	rows := []models.CrawlerTask{a, b, grand, other}
	if ok, _ := flowEligible(p, rows, b); ok {
		t.Fatal("sibling bypassed earlier subtree")
	}
	if ok, _ := flowEligible(p, rows, grand); !ok {
		t.Fatal("subtree blocked")
	}
	if ok, _ := flowEligible(p, rows, other); !ok {
		t.Fatal("parallel branch blocked")
	}
	rows[2].Status = models.TaskStatusProcessing
	if ok, _ := flowEligible(p, rows, b); ok {
		t.Fatal("branch concurrency exceeded")
	}
	if ok, _ := flowEligible(p, rows, other); !ok {
		t.Fatal("second branch blocked")
	}
	rows[2].Status = models.TaskStatusFailed
	if ok, _ := flowEligible(p, rows, b); !ok {
		t.Fatal("terminal failure did not release branch")
	}
}

func TestFlowPolicyValidation(t *testing.T) {
	for _, p := range []FlowPolicy{{}, {Stages: map[string]string{"x": "missing"}, Lanes: map[string]FlowLane{"x": {"depth", 1}}}, {Stages: map[string]string{"x": "x"}, Lanes: map[string]FlowLane{"x": {"lifo", 1}}}, {Stages: map[string]string{"x": "x"}, Lanes: map[string]FlowLane{"x": {"depth", 0}}}} {
		if p.Validate() == nil {
			t.Fatal("invalid policy accepted")
		}
	}
}
