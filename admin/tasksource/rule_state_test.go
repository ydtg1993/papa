package tasksource

import (
	"context"
	"testing"

	"github.com/ydtg1993/oao"
)

type roleRows struct{}

func (roleRows) List(context.Context, oao.Query) ([]map[string]any, int64, error) {
	return []map[string]any{{"meta": []byte(`{"_rule_batch":"b","secret":"do not expose"}`)}, {"meta": `{"_rule_schedule":"x"}`}, {"meta": nil}}, 3, nil
}
func TestRuleSourceProjectsRoleWithoutMetadata(t *testing.T) {
	rows, total, err := (ruleSource{roleRows{}}).List(context.Background(), oao.Query{})
	if err != nil || total != 3 {
		t.Fatalf("%d %v", total, err)
	}
	for i, want := range []string{"member", "schedule", "ordinary"} {
		if rows[i]["rule_role"] != want {
			t.Fatalf("row %d: %v", i, rows[i])
		}
		if _, exists := rows[i]["meta"]; exists {
			t.Fatal("metadata leaked")
		}
	}
}
func TestRuleActionVisibility(t *testing.T) {
	table := Table(nil, &stubActions{})
	for _, action := range table.Actions {
		for _, role := range []string{"ordinary", "schedule", "member", "invalid"} {
			for _, status := range []int{0, 1, 2, 3} {
				hide := action.Key == "remove" && role != "ordinary" || (action.Key == "repeat_on" || action.Key == "repeat_interval") && (role == "member" || role == "invalid") || action.Key == "retry" && (role == "invalid" || role == "member" && status != 3)
				got := action.StateFunc(map[string]any{"rule_role": role, "status": status, "repeatable": 0})
				if got.Hidden != hide {
					t.Fatalf("%s/%s/%d: %+v", action.Key, role, status, got)
				}
			}
		}
	}
}

func TestRuleMemberWithStrayPollingCannotShowRetry(t *testing.T) {
	row := map[string]any{"rule_role": "member", "status": 3, "repeatable": 1}
	if !ruleActionState("retry", row).Hidden || ruleActionState("repeat_off", row).Hidden {
		t.Fatal("polling member must be repaired before retry")
	}
}
