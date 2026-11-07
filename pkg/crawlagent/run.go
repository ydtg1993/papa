package crawlagent

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/ydtg1993/papa/v3/pkg/agentstore"
	"github.com/ydtg1993/papa/v3/pkg/crawlagent/model"
	"github.com/ydtg1993/papa/v3/pkg/rulestore"
	"github.com/ydtg1993/papa/v3/pkg/webaccess"
)

type execution struct {
	agent         *Agent
	goal          Goal
	run           agentstore.Run
	mu            sync.Mutex
	usage         Usage
	source        Source
	evidence      []Evidence
	candidates    map[string]Candidate
	counts        map[string]int
	hiddenDigests map[string]bool
	hiddenURLs    map[string]bool
	pending       *agentstore.Step
}

// Run creates a fresh analysis. Interrupted runs are inspected/interrupted via
// the audit store, never silently resumed or replayed as billable requests.
func (a *Agent) Run(parent context.Context, goal Goal) (result Result, err error) {
	return a.RunWithID(parent, "agent-"+uuid.NewString(), goal)
}

// RunWithID binds a host-owned durable request to one audit identity. It never
// resumes an existing ID: the atomic audit Start must succeed before any model
// call. On a duplicate or uncertain write, inspect the returned RunID instead
// of retrying with a new identity. The model cannot choose this identifier.
func (a *Agent) RunWithID(parent context.Context, runID string, goal Goal) (result Result, err error) {
	if !validRunID(runID) {
		return result, ErrInvalid
	}
	if parent == nil {
		return result, ErrInvalid
	}
	if parent.Err() != nil {
		return result, parent.Err()
	}
	goal, err = cloneGoal(goal)
	if err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(parent, a.config.Budgets.MaxDuration)
	defer cancel()
	snapshot, err := a.Plan(goal)
	if err != nil {
		return result, err
	}
	safeGoal := snapshot.Goal
	e := &execution{agent: a, goal: goal, candidates: map[string]Candidate{}, counts: map[string]int{}, hiddenDigests: map[string]bool{}, hiddenURLs: map[string]bool{}}
	e.usage.CostKnown = a.config.Budgets.Prices.InputPerMillionMicros > 0 && a.config.Budgets.Prices.OutputPerMillionMicros > 0
	e.run, err = a.config.Audit.Start(ctx, agentstore.Run{ID: runID, SnapshotJSON: encode(snapshot), UsageJSON: encode(e.usage)})
	if err != nil {
		return Result{RunID: runID, Status: agentstore.Interrupted, Reason: "audit_unconfirmed"}, ErrAudit
	}
	defer func() {
		if recover() != nil {
			cleanup, c := context.WithTimeout(context.Background(), 3*time.Second)
			if e.pending != nil {
				_ = e.end(cleanup, *e.pending, agentstore.StepFailed, `{"error":"host_callback_failed"}`)
			}
			c()
			result, err = e.stop(ErrAction)
		}
		if e.source != nil {
			if closeErr := closeAnalysisSource(e.source); closeErr != nil && err == nil {
				err = errors.New("crawlagent: source cleanup failed")
			}
		}
	}()
	if err = e.loadFixtures(ctx); err != nil {
		// Repository and disclosure failures use stable evidence errors, but
		// cancellation remains the cause when the load was interrupted.
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return e.stop(err)
	}
	w := a.config.Web
	previousRequest, previousBytes := w.OnRequest, w.OnBytes
	w.OnRequest = func(c context.Context) error {
		if previousRequest != nil {
			if err := previousRequest(c); err != nil {
				return err
			}
		}
		pages := 0
		if e.source != nil {
			pages = e.source.Usage().Pages
		}
		return e.updateUsage(c, func(u *Usage) error {
			u.Requests++
			if pages > u.Pages {
				u.Pages = pages
			}
			return nil
		})
	}
	w.OnBytes = func(c context.Context, n int64) error {
		if previousBytes != nil {
			if err := previousBytes(c, n); err != nil {
				return err
			}
		}
		return e.updateUsage(c, func(u *Usage) error {
			if n < 0 || n > 64<<20 || u.Bytes > (64<<20)-n {
				return ErrBudget
			}
			u.Bytes += n
			return nil
		})
	}
	e.source, err = a.config.SourceFactory(w)
	if err != nil {
		return e.stop(err)
	}
	publicGoal := safeGoal
	publicGoal.EvidenceSamples = nil // approved inventory below carries accessible IDs
	publicGoal.Seeds = make([]string, len(safeGoal.Seeds))
	seedURLs := make(map[string]string, len(safeGoal.Seeds))
	for i, seed := range safeGoal.Seeds {
		key := "seed:" + strconv.Itoa(i)
		publicGoal.Seeds[i] = key
		seedURLs[key] = seed
	}
	publicGoal.Stages = append([]StageGoal(nil), goal.Stages...)
	for i := range publicGoal.Stages {
		publicGoal.Stages[i].TrainingSet = ""
		publicGoal.Stages[i].HoldoutSet = ""
		publicGoal.Stages[i].Existing = nil
	}
	messages := []model.Message{{Role: "system", Content: systemPrompt}, {Role: "user", Content: encode(struct {
		Goal         Goal              `json:"goal"`
		Evidence     []Evidence        `json:"evidence"`
		SeedURLs     map[string]string `json:"seed_urls"`
		Capabilities map[string]bool   `json:"capabilities"`
	}{publicGoal, e.evidence, seedURLs, map[string]bool{"render": a.config.Web.Renderer.Enabled, "record_api": a.config.Web.Renderer.Enabled && a.config.AllowRecordAPI}})}}
	for {
		if err = ctx.Err(); err != nil {
			return e.stop(err)
		}
		var reply model.Reply
		reply, err = e.complete(ctx, messages)
		if err != nil {
			return e.stop(err)
		}
		if ctx.Err() != nil {
			return e.stop(ctx.Err())
		}
		act, parseErr := parseAction(reply.Content)
		if parseErr != nil {
			messages = append(messages, model.Message{Role: "user", Content: `{"error":"invalid_action","instruction":"Return exactly one allowed JSON action as plain text in message.content. No native tools are registered. Do not emit DSML, XML, function-call markup, Markdown or commentary. Use the JSON property tool."}`})
			continue
		}
		input := map[string]any{"tool": act.Tool, "action_sha256": hashBytes([]byte(reply.Content))}
		if act.Wait != nil {
			input["wait"] = map[string]any{"selector_sha256": hashBytes([]byte(act.Wait.Selector)), "state": act.Wait.State, "timeout_ms": act.Wait.TimeoutMS}
		}
		if act.Tool == "submit_candidate" {
			if stage, ok := e.stage(act.Stage); ok {
				input["candidate_reference"] = rulestore.Reference{ID: stage.RuleID, Version: e.run.ID + "-" + strconv.Itoa(e.counts[stage.Stage]+1)}
			}
		}
		step, beginErr := e.begin(ctx, "tool", encode(input), nil)
		if beginErr != nil {
			return e.stop(beginErr)
		}
		toolStarted := time.Now()
		observation, terminal, toolErr := e.dispatch(ctx, act)
		if ctx.Err() != nil {
			toolErr = ctx.Err()
		}
		status := agentstore.StepSucceeded
		if toolErr != nil {
			status = agentstore.StepFailed
		}
		// Evidence values remain in the approved model conversation, not the audit.
		meta := map[string]any{"tool": act.Tool, "observation_sha256": hashBytes([]byte(encode(observation))), "error": errorKind(toolErr)}
		if act.Wait != nil {
			outcome := errorKind(toolErr)
			if toolErr == nil {
				outcome = "succeeded"
			}
			meta["render_wait"] = map[string]any{"tool_elapsed_ms": time.Since(toolStarted).Milliseconds(), "outcome": outcome}
		}
		meta["references"] = e.auditReferences()
		endCtx := ctx
		var endCancel context.CancelFunc
		if ctx.Err() != nil {
			endCtx, endCancel = context.WithTimeout(context.Background(), 3*time.Second)
		}
		err = e.end(endCtx, step, status, encode(meta))
		if endCancel != nil {
			endCancel()
		}
		if err != nil {
			return e.stop(err)
		}
		if toolErr != nil {
			if errors.Is(toolErr, ErrAudit) || errors.Is(toolErr, ErrBudget) || errors.Is(toolErr, context.Canceled) || errors.Is(toolErr, context.DeadlineExceeded) || errors.Is(toolErr, webaccess.ErrCanceled) || errors.Is(toolErr, webaccess.ErrBudget) || errors.Is(toolErr, webaccess.ErrAudit) {
				return e.stop(toolErr)
			}
			if observation == nil {
				observation = map[string]string{"error": errorKind(toolErr)}
			}
		}
		if terminal != nil {
			if err = e.finish(*terminal); err != nil {
				return e.result(agentstore.Interrupted, "audit_unconfirmed", false), err
			}
			return *terminal, toolErr
		}
		messages = append(messages, model.Message{Role: "assistant", Content: reply.Content}, model.Message{Role: "user", Content: encode(observation)})
	}
}

// Cleanup is host code too. A close panic must not escape after a terminal
// result has been committed, or cause callers to replace that result with failure.
func closeAnalysisSource(source Source) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("crawlagent: source cleanup failed")
		}
	}()
	return source.Close()
}

func validRunID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func (e *execution) begin(ctx context.Context, kind, input string, next *Usage) (agentstore.Step, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	u := e.usage
	if next != nil {
		u = *next
	}
	r, s, err := e.agent.config.Audit.BeginStep(ctx, e.run.ID, e.run.Revision, agentstore.Step{Kind: kind, InputJSON: input}, encode(u))
	if err != nil {
		return s, ErrAudit
	}
	e.run = r
	e.usage = u
	e.pending = &s
	return s, nil
}

func (e *execution) end(ctx context.Context, step agentstore.Step, status, output string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	step.Status = status
	step.OutputJSON = output
	r, err := e.agent.config.Audit.EndStep(ctx, e.run.ID, e.run.Revision, step, encode(e.usage))
	if err != nil {
		return ErrAudit
	}
	e.run = r
	e.pending = nil
	return nil
}

func (e *execution) updateUsage(ctx context.Context, change func(*Usage) error) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	next := e.usage
	if err := change(&next); err != nil {
		return err
	}
	r, err := e.agent.config.Audit.UpdateUsage(ctx, e.run.ID, e.run.Revision, encode(next))
	if err != nil {
		return ErrAudit
	}
	e.run = r
	e.usage = next
	return nil
}

func (e *execution) result(status, reason string, verified bool) Result {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := Result{RunID: e.run.ID, Status: status, Reason: reason, Verified: verified, Usage: e.usage, Evidence: append([]Evidence(nil), e.evidence...)}
	for _, stage := range e.goal.Stages {
		if c, ok := e.candidates[stage.Stage]; ok {
			out.Candidates = append(out.Candidates, c)
		}
	}
	return out
}

func (e *execution) finish(result Result) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	e.mu.Lock()
	defer e.mu.Unlock()
	// Durable audit carries bounded references, not duplicate page metadata or
	// potentially large comparison reports. Full metadata remains in rulestore.
	result.Evidence = append([]Evidence(nil), result.Evidence...)
	for i := range result.Evidence {
		result.Evidence[i].URL = ""
		result.Evidence[i].ContentType = ""
	}
	result.Candidates = append([]Candidate(nil), result.Candidates...)
	for i := range result.Candidates {
		result.Candidates[i].Comparison = nil
		result.Candidates[i].Shadow = nil
	}
	r, err := e.agent.config.Audit.Finish(ctx, e.run.ID, e.run.Revision, result.Status, encode(result), encode(e.usage))
	if err != nil {
		return ErrAudit
	}
	e.run = r
	return nil
}

func (e *execution) stop(err error) (Result, error) {
	status, reason := agentstore.Failed, errorKind(err)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, webaccess.ErrCanceled) {
		status = agentstore.Canceled
	}
	if errors.Is(err, ErrBudget) || errors.Is(err, webaccess.ErrBudget) {
		status = agentstore.Exhausted
	}
	if errors.Is(err, ErrAudit) || errors.Is(err, webaccess.ErrAudit) {
		return e.result(agentstore.Interrupted, "audit_unconfirmed", false), ErrAudit
	}
	out := e.result(status, reason, false)
	if finishErr := e.finish(out); finishErr != nil {
		return e.result(agentstore.Interrupted, "audit_unconfirmed", false), finishErr
	}
	return out, err
}

func errorKind(err error) string {
	if err == nil {
		return ""
	}
	for _, entry := range []struct {
		err  error
		kind string
	}{{ErrInvalid, "invalid_contract"}, {ErrAction, "invalid_action"}, {ErrEvidence, "unavailable_evidence"}, {ErrBudget, "budget_exhausted"}, {ErrAudit, "audit_failed"}, {webaccess.ErrWaitInvalid, "invalid_render_wait"}, {webaccess.ErrWaitTimeout, "render_wait_timeout"}, {webaccess.ErrPolicy, "access_denied"}, {webaccess.ErrRobots, "robots_denied"}, {webaccess.ErrBudget, "budget_exhausted"}, {webaccess.ErrCanceled, "canceled"}, {context.Canceled, "canceled"}, {context.DeadlineExceeded, "timeout"}} {
		if errors.Is(err, entry.err) {
			return entry.kind
		}
	}
	// Classify only public sentinel errors. Never retain provider bodies, URLs,
	// wrapped error text or credentials when diagnosing a failed paid request.
	for _, entry := range []struct {
		err  error
		kind string
	}{
		{model.ErrConfig, "model_config"}, {model.ErrCredentials, "model_credentials"},
		{model.ErrInput, "model_input"}, {model.ErrInputLimit, "model_input_limit"},
		{model.ErrOutputLimit, "model_output_limit"}, {model.ErrUnauthorized, "model_unauthorized"},
		{model.ErrRateLimit, "model_rate_limit"}, {model.ErrHTTP, "model_http"},
		{model.ErrRedirect, "model_redirect"}, {model.ErrTransport, "model_transport"},
		{model.ErrResponseLimit, "model_response_limit"}, {model.ErrEmptyResponse, "model_empty_response"}, {model.ErrInvalidResponse, "model_invalid_response"},
		{model.ErrRefusal, "model_refusal"}, {model.ErrTruncated, "model_truncated"},
		{model.ErrToolCall, "model_tool_call"}, {model.ErrExhausted, "model_replay_exhausted"},
	} {
		if errors.Is(err, entry.err) {
			return entry.kind
		}
	}
	return "operation_failed"
}

func (e *execution) complete(ctx context.Context, messages []model.Message) (model.Reply, error) {
	b := e.agent.config.Budgets
	data, err := json.Marshal(messages)
	if err != nil || len(data) > b.MaxInputBytes {
		return model.Reply{}, ErrBudget
	}
	e.mu.Lock()
	next := e.usage
	e.mu.Unlock()
	if next.ModelCalls >= b.MaxModelCalls || next.InputBytes+int64(len(data)) > int64(b.MaxTotalInputBytes) {
		return model.Reply{}, ErrBudget
	}
	inputCost, err := cost(int64(len(data))+4096, b.Prices.InputPerMillionMicros)
	if err != nil {
		return model.Reply{}, err
	}
	outputCost, err := cost(int64(b.MaxOutputTokens), b.Prices.OutputPerMillionMicros)
	if err != nil {
		return model.Reply{}, err
	}
	reserved := inputCost + outputCost
	if b.Prices.MaxMicros > 0 && next.EstimatedMicros+reserved > b.Prices.MaxMicros {
		return model.Reply{}, ErrBudget
	}
	next.ModelCalls++
	next.InputBytes += int64(len(data))
	next.EstimatedMicros += reserved
	step, err := e.begin(ctx, "model", encode(map[string]any{"model": e.agent.config.ModelName, "input_sha256": hashBytes(data), "input_bytes": len(data), "max_output_tokens": b.MaxOutputTokens}), &next)
	if err != nil {
		return model.Reply{}, err
	}
	reply, callErr := safeComplete(e.agent.config.Model, ctx, messages, b.MaxOutputTokens)
	if callErr != nil {
		e.mu.Lock()
		e.usage.CostKnown = false
		e.mu.Unlock()
	}
	if callErr == nil && len(reply.Content) > 64<<10 {
		callErr = ErrBudget
	}
	if callErr == nil {
		e.mu.Lock()
		if reply.Usage.Known {
			in, c1 := cost(reply.Usage.InputTokens, b.Prices.InputPerMillionMicros)
			out, c2 := cost(reply.Usage.OutputTokens, b.Prices.OutputPerMillionMicros)
			if c1 != nil || c2 != nil || reply.Usage.InputTokens > 1_000_000_000 || reply.Usage.OutputTokens > 1_000_000_000 {
				callErr = ErrBudget
				e.usage.CostKnown = false
			} else {
				e.usage.InputTokens += reply.Usage.InputTokens
				e.usage.OutputTokens += reply.Usage.OutputTokens
				e.usage.EstimatedMicros += in + out - reserved
				if reply.Usage.OutputTokens > int64(b.MaxOutputTokens) {
					callErr = ErrBudget
				}
			}
		} else {
			e.usage.CostKnown = false
		}
		if b.Prices.MaxMicros > 0 && e.usage.EstimatedMicros > b.Prices.MaxMicros {
			callErr = ErrBudget
		}
		e.mu.Unlock()
	}
	if ctx.Err() != nil {
		callErr = ctx.Err()
	}
	status := agentstore.StepSucceeded
	if callErr != nil {
		status = agentstore.StepFailed
	}
	endCtx := ctx
	var cancel context.CancelFunc
	if ctx.Err() != nil {
		endCtx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
	}
	if err = e.end(endCtx, step, status, encode(map[string]any{"response_sha256": hashBytes([]byte(reply.Content)), "usage": reply.Usage, "error": errorKind(callErr)})); err != nil {
		return model.Reply{}, err
	}
	return reply, callErr
}

func safeComplete(p model.Provider, ctx context.Context, m []model.Message, max int) (reply model.Reply, err error) {
	defer func() {
		if recover() != nil {
			reply = model.Reply{}
			err = errors.New("crawlagent: model adapter failed")
		}
	}()
	return p.Complete(ctx, append([]model.Message(nil), m...), max)
}
