package tasksource

import (
	"context"
	"fmt"

	"github.com/ydtg1993/oao"
	"github.com/ydtg1993/papa/v4/engine"
)

// ruleSource derives a small role hint without exposing task metadata, which
// may contain site-specific credentials or large rule checkpoints.
type ruleSource struct{ oao.Source }

func (s ruleSource) List(ctx context.Context, q oao.Query) ([]map[string]any, int64, error) {
	rows, total, err := s.Source.List(ctx, q)
	for _, row := range rows {
		var meta []byte
		switch v := row["meta"].(type) {
		case []byte:
			meta = v
		case string:
			meta = []byte(v)
		case nil:
		default:
			meta = []byte("invalid")
		}
		row["rule_role"] = engine.RuleTaskRole(meta)
		delete(row, "meta")
	}
	return rows, total, err
}

func ruleActionState(action string, row map[string]any) oao.ActionState {
	role, _ := row["rule_role"].(string)
	protected := role == "member" || role == "schedule" || role == "invalid"
	hide := false
	switch action {
	case "remove":
		hide = protected
	case "repeat_on", "repeat_interval":
		hide = role == "member" || role == "invalid"
	case "retry":
		hide = role == "invalid" || (role == "member" && (fmt.Sprint(row["status"]) != "3" || fmt.Sprint(row["repeatable"]) != "0"))
	}
	return oao.ActionState{Hidden: hide}
}
