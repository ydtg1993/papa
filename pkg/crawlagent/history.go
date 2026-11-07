package crawlagent

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/ydtg1993/papa/v3/pkg/agentstore"
	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
	"github.com/ydtg1993/papa/v3/pkg/rulestore"
)

const (
	MaxHistoryRules      = 4
	MaxHistoryViewBytes  = 8 << 10
	MaxTotalHistoryBytes = 16 << 10
)

// HistoryRequest is a detached disclosure approval request, not a model tool.
// The host must approve both the site/stage binding and the projected literals.
// ViewJSON excludes rule identity, validation patterns and resource ceilings.
type HistoryRequest struct {
	Site, Stage string
	Version     rulestore.Version
	ViewJSON    json.RawMessage
}

// HistoryRecord identifies approved context prepared for model disclosure.
// Model steps determine whether a call was actually attempted after preparation.
// It intentionally contains no rules, sample bodies or expected field values.
type HistoryRecord struct {
	Stage      string              `json:"stage"`
	Reference  rulestore.Reference `json:"reference"`
	RuleSHA256 string              `json:"rule_sha256"`
	ViewSHA256 string              `json:"view_sha256"`
	State      string              `json:"state"`
	ViewBytes  int                 `json:"view_bytes"`
}

type historyView struct {
	Stage string          `json:"stage"`
	State string          `json:"state"`
	Rule  json.RawMessage `json:"rule"`
}

func projectHistory(rule crawlrule.Rule) (json.RawMessage, error) {
	rule.ID, rule.Version = "", ""
	rule.MaxRows, rule.MaxBodyBytes, rule.MaxIssues, rule.MaxEvidence = 0, 0, 0, 0
	for i := range rule.Fields {
		rule.Fields[i].Validation.Pattern = ""
	}
	b, err := json.Marshal(rule)
	if err != nil {
		return nil, ErrEvidence
	}
	var view map[string]json.RawMessage
	if json.Unmarshal(b, &view) != nil {
		return nil, ErrEvidence
	}
	delete(view, "id")
	delete(view, "version")
	b, err = json.Marshal(view)
	if err != nil {
		return nil, ErrEvidence
	}
	return b, nil
}

// historyContainsHiddenURL examines decoded JSON strings so escaping cannot
// hide a direct hidden URL. Host approval remains necessary for sensitive
// selectors/pointers or indirect answers; this is not a general secret detector.
var historyURLLiteral = regexp.MustCompile(`(?i)https?://[^\s"'<>\\]+`)

func (e *execution) historyContainsHiddenURL(raw []byte) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return true
	}
	var contains func(any) bool
	contains = func(v any) bool {
		switch x := v.(type) {
		case string:
			for hidden := range e.hiddenURLs {
				if strings.Contains(x, hidden) {
					return true
				}
			}
			// Selectors may contain a literal URL using a different host case,
			// default port or query order. Use the same identity/disclosure rules
			// as evidence fetching rather than relying only on substring matches.
			// This deliberately does not interpret CSS escapes or arbitrary code.
			for _, literal := range historyURLLiteral.FindAllString(x, -1) {
				for _, candidate := range []string{literal, strings.TrimRight(literal, "])},")} {
					if hidden, err := e.isHiddenURL(candidate); err == nil && hidden {
						return true
					}
				}
			}
		case []any:
			for _, item := range x {
				if contains(item) {
					return true
				}
			}
		case map[string]any:
			for key, item := range x {
				if contains(key) || contains(item) {
					return true
				}
			}
		}
		return false
	}
	return contains(value)
}

func (e *execution) loadHistory(ctx context.Context) error {
	if e.agent.config.HistoryPolicy == nil {
		return nil
	}
	count := 0
	for _, s := range e.goal.Stages {
		if s.Existing != nil {
			count++
		}
	}
	if count > MaxHistoryRules {
		return ErrBudget
	}
	e.history = map[string]rulestore.Version{}
	total := 0
	for _, stage := range e.goal.Stages {
		if stage.Existing == nil {
			continue
		}
		step, err := e.begin(ctx, "history", encode(map[string]any{"stage": stage.Stage, "reference": stage.Existing}), nil)
		if err != nil {
			return err
		}
		v, view, loadErr := e.approveHistory(ctx, stage)
		if loadErr == nil {
			total += len(view)
			if len(view) > MaxHistoryViewBytes || total > MaxTotalHistoryBytes {
				loadErr = ErrBudget
			}
		}
		status := agentstore.StepSucceeded
		output := encode(map[string]string{"error": errorKind(loadErr)})
		if loadErr == nil {
			record := HistoryRecord{Stage: stage.Stage, Reference: v.Reference, RuleSHA256: v.Digest, ViewSHA256: hashBytes(view), State: "published", ViewBytes: len(view)}
			output = encode(record)
		}
		endCtx := ctx
		cancel := func() {}
		if ctx.Err() != nil {
			loadErr = ctx.Err()
			endCtx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
		}
		if loadErr != nil {
			status = agentstore.StepFailed
			output = encode(map[string]string{"error": errorKind(loadErr)})
		}
		err = e.end(endCtx, step, status, output)
		cancel()
		if err != nil {
			return err
		}
		if loadErr != nil {
			return loadErr
		}
		e.history[stage.Stage] = v
		e.historyViews = append(e.historyViews, historyView{Stage: stage.Stage, State: "published", Rule: view})
	}
	return nil
}

func (e *execution) approveHistory(ctx context.Context, stage StageGoal) (rulestore.Version, json.RawMessage, error) {
	var zero rulestore.Version
	v, err := e.agent.config.Repository.Get(ctx, *stage.Existing)
	if err != nil || v.Reference != *stage.Existing || !v.EverPublished || v.State != "published" {
		return zero, nil, ErrEvidence
	}
	if checkRuleComplexity(v.Rule) != nil {
		return zero, nil, ErrEvidence
	}
	c, err := crawlrule.Compile(v.Rule)
	if err != nil {
		return zero, nil, ErrEvidence
	}
	v.Rule = c.Rule()
	raw, err := json.Marshal(v.Rule)
	if err != nil || hashBytes(raw) != v.Digest || v.Rule.ID != v.ID || v.Rule.Version != v.Version {
		return zero, nil, ErrEvidence
	}
	view, err := projectHistory(c.Rule())
	if err != nil {
		return zero, nil, err
	}
	if len(view) > MaxHistoryViewBytes {
		return zero, nil, ErrBudget
	}
	if e.historyContainsHiddenURL(view) {
		return zero, nil, ErrEvidence
	}
	// Detach both fields passed to arbitrary host code from the frozen snapshot.
	var approvalVersion rulestore.Version
	encoded, _ := json.Marshal(v)
	if json.Unmarshal(encoded, &approvalVersion) != nil {
		return zero, nil, ErrEvidence
	}
	if err := e.agent.config.HistoryPolicy(ctx, HistoryRequest{Site: e.goal.Site, Stage: stage.Stage, Version: approvalVersion, ViewJSON: append(json.RawMessage(nil), view...)}); err != nil {
		return zero, nil, ErrEvidence
	}
	if ctx.Err() != nil {
		return zero, nil, ctx.Err()
	}
	return v, view, nil
}
