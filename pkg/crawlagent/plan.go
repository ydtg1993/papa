package crawlagent

import (
	"encoding/json"
	"github.com/ydtg1993/papa/v3/pkg/rulestore"
	"github.com/ydtg1993/papa/v3/pkg/webaccess"
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
	p := Plan{safe, a.config.Web.Policy, a.config.Budgets, a.config.ModelName, a.config.Actor, a.config.Web.Budgets, a.config.Web.Renderer, a.config.AllowRecordAPI}
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
