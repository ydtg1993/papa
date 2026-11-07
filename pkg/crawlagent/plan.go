package crawlagent

import (
	"encoding/json"
	"github.com/ydtg1993/papa/v4/pkg/rulestore"
	"github.com/ydtg1993/papa/v4/pkg/webaccess"
)

// Plan is the effective, sanitized configuration shown before an explicit run.
// It contains neither model credentials nor a transport/source implementation.
type Plan struct {
	Goal           Goal
	Policy         webaccess.Policy
	Budgets        Budgets
	Model          string
	Actor          rulestore.Actor
	WebBudgets     webaccess.Budgets
	Renderer       webaccess.RendererConfig
	AllowRecordAPI bool
	// HistoryEnabled makes disclosure explicit in host review/digest and audit
	// snapshots. It declares intent, not that approval or a model call succeeded.
	HistoryEnabled bool `json:",omitempty"`
}

func (a *Agent) Plan(goal Goal) (Plan, error) {
	safe, err := cloneGoal(goal)
	if err != nil {
		return Plan{}, err
	}
	for i, seed := range safe.Seeds {
		policy := a.config.SamplePolicy
		policy.RedactBody = func(_ bool, b []byte) ([]byte, error) { return b, nil }
		sample, err := rulestore.SanitizeSample(rulestore.SampleInput{URL: seed, StatusCode: 200}, policy)
		if err != nil {
			return Plan{}, ErrInvalid
		}
		safe.Seeds[i] = sample.URL
	}
	p := Plan{Goal: safe, Policy: a.config.Web.Policy, Budgets: a.config.Budgets, Model: a.config.ModelName, Actor: a.config.Actor, WebBudgets: a.config.Web.Budgets, Renderer: a.config.Web.Renderer, AllowRecordAPI: a.config.AllowRecordAPI, HistoryEnabled: a.config.HistoryPolicy != nil}
	// Detached slices/maps: a review caller cannot widen the Agent's permissions.
	b, err := json.Marshal(p)
	if err != nil {
		return Plan{}, ErrInvalid
	}
	var out Plan
	if json.Unmarshal(b, &out) != nil {
		return Plan{}, ErrInvalid
	}
	return out, nil
}

// ForActor preserves the frozen host capabilities while binding authenticated
// operator metadata. It performs no model, website or database operation.
func (a *Agent) ForActor(actor rulestore.Actor) (*Agent, error) {
	cfg := a.config
	cfg.Actor = actor
	return New(cfg)
}
