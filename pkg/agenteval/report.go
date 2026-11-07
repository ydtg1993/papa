package agenteval

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"sort"

	"github.com/ydtg1993/papa/v3/pkg/agentstore"
	"github.com/ydtg1993/papa/v3/pkg/crawlagent"
	"github.com/ydtg1993/papa/v3/pkg/crawlagent/model"
	"github.com/ydtg1993/papa/v3/pkg/rulestore"
	"github.com/ydtg1993/papa/v3/pkg/webaccess"
)

const ProtocolVersion = "papa-fixed-evaluation-v1"

type Identity struct {
	Protocol, CorpusSHA256, PromptSHA256, ToolProtocol string
	Mode, Model, EndpointSHA256, OutputTokenField      string
	PriceVersion, Currency                             string
	Budgets                                            crawlagent.Budgets
	WebBudgets                                         webaccess.Budgets
	Scenarios                                          []string
	Repetitions                                        int
	HistoryMode                                        string `json:",omitempty"` // empty is legacy; none or published for paired history evaluation
	HistoryFixtureSHA256                               string `json:",omitempty"`
}

type Tokens struct{ Input, Output int64 }
type AuditMetrics struct {
	Tools                  Count                      `json:"tool_errors"` // Matched counts non-successful reserved tools.
	Corrections            int                        `json:"correction_attempts"`
	ModelCalls             int                        `json:"model_calls"`
	Tokens                 *Tokens                    `json:"tokens"` // null if any call omitted/failed usage.
	Incomplete             bool                       `json:"incomplete"`
	InfrastructureFailures int                        `json:"infrastructure_failures"`
	History                []crawlagent.HistoryRecord `json:"history,omitempty"`
}

// MeasureAudit counts reserved attempts, not just successful calls. Tool parse
// failures consume model calls but never reserve a tool and are not tool errors.
func MeasureAudit(steps []agentstore.Step) AuditMetrics {
	var m AuditMetrics
	tokens := Tokens{}
	known, submits := true, 0
	for _, s := range steps {
		if s.Kind == "history" && s.Status == agentstore.StepSucceeded {
			var record crawlagent.HistoryRecord
			if json.Unmarshal([]byte(s.OutputJSON), &record) != nil || !validHistoryRecord(record) {
				m.Incomplete = true
			} else {
				m.History = append(m.History, record)
			}
		}
		if s.Status == agentstore.StepPending || s.Status == agentstore.StepUnknown {
			m.Incomplete = true
		}
		if s.Status == agentstore.StepFailed {
			var failure struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal([]byte(s.OutputJSON), &failure)
			// Invalid candidate/actions and model formatting/refusals are quality
			// failures. Resource, access, transport and persistence failures make
			// this evaluation operationally ineligible, even after a later retry.
			switch failure.Error {
			case "invalid_action", "invalid_render_wait", "model_empty_response", "model_invalid_response", "model_refusal", "model_truncated", "model_tool_call":
			default:
				m.InfrastructureFailures++
			}
		}
		if s.Kind == "tool" {
			m.Tools.Total++
			if s.Status != agentstore.StepSucceeded {
				m.Tools.Matched++
			}
			var input struct {
				Tool string `json:"tool"`
			}
			if json.Unmarshal([]byte(s.InputJSON), &input) != nil || input.Tool == "" {
				m.Incomplete = true
			}
			if input.Tool == "submit_candidate" {
				submits++
			}
		}
		if s.Kind == "model" {
			m.ModelCalls++
			var output struct {
				Usage model.Usage `json:"usage"`
			}
			if s.Status != agentstore.StepSucceeded || json.Unmarshal([]byte(s.OutputJSON), &output) != nil || !output.Usage.Known || output.Usage.InputTokens < 0 || output.Usage.OutputTokens < 0 || output.Usage.InputTokens > 1_000_000_000 || output.Usage.OutputTokens > 1_000_000_000 {
				known = false
			} else {
				tokens.Input += output.Usage.InputTokens
				tokens.Output += output.Usage.OutputTokens
			}
		}
	}
	if submits > 1 {
		m.Corrections = submits - 1
	}
	if known && m.ModelCalls > 0 {
		m.Tokens = &tokens
	}
	return m
}

type Run struct {
	Scenario                                               string `json:"scenario"`
	Repetition                                             int    `json:"repetition"`
	RunID                                                  string `json:"run_id"`
	Status                                                 string `json:"status"`
	Failure                                                string `json:"failure,omitempty"` // stable category, never raw provider errors
	TrainingSet, TrainingDigest, HoldoutSet, HoldoutDigest string
	Candidate                                              rulestore.Reference `json:"candidate"`
	CandidateDigest, TrainingReportID, HoldoutReportID     string
	Scores                                                 Scores          `json:"scores"`
	Audit                                                  AuditMetrics    `json:"audit"`
	InputBytes                                             int64           `json:"model_input_bytes"`
	WebUsage                                               webaccess.Usage `json:"web_usage"`
	ElapsedMS                                              int64           `json:"elapsed_ms"`
	InfrastructureFailure                                  bool            `json:"infrastructure_failure"`
	ReadyForReview                                         bool            `json:"ready_for_review"`
	EstimatedMicros                                        *int64          `json:"estimated_micros"`
}

type Report struct {
	Identity Identity `json:"identity"`
	Runs     []Run    `json:"runs"`
}

// ValidateComplete prevents a partial run or cherry-picked repetition from
// becoming a comparable baseline. Identity is host-owned, not a signed proof.
func (r Report) ValidateComplete() error {
	i := r.Identity
	if i.Protocol != ProtocolVersion || len(i.CorpusSHA256) != 64 || len(i.PromptSHA256) != 64 || i.ToolProtocol == "" || i.Model == "" || i.Mode == "" || len(i.EndpointSHA256) != 64 || i.Repetitions < 1 || i.Repetitions > 3 || len(i.Scenarios) == 0 || len(i.Scenarios) > 64 || len(r.Runs) != len(i.Scenarios)*i.Repetitions {
		return errors.New("agenteval: incomplete report identity or repetitions")
	}
	if i.HistoryMode != "" && i.HistoryMode != "none" && i.HistoryMode != "published" {
		return errors.New("agenteval: unknown history treatment")
	}
	if (i.HistoryMode == "") != (i.HistoryFixtureSHA256 == "") || (i.HistoryFixtureSHA256 != "" && len(i.HistoryFixtureSHA256) != 64) {
		return errors.New("agenteval: missing history fixture identity")
	}
	historyViews := map[string]crawlagent.HistoryRecord{}
	if i.HistoryFixtureSHA256 != "" {
		if _, err := hex.DecodeString(i.HistoryFixtureSHA256); err != nil {
			return errors.New("agenteval: invalid history fixture digest")
		}
	}
	for _, digest := range []string{i.CorpusSHA256, i.PromptSHA256, i.EndpointSHA256} {
		if _, err := hex.DecodeString(digest); err != nil {
			return errors.New("agenteval: invalid identity digest")
		}
	}
	allowed, seen, ids := map[string]bool{}, map[string]map[int]bool{}, map[string]bool{}
	denominators := map[string][3]int{}
	for _, s := range i.Scenarios {
		if s == "" || allowed[s] {
			return errors.New("agenteval: duplicate scenario")
		}
		allowed[s] = true
		seen[s] = map[int]bool{}
	}
	for _, run := range r.Runs {
		if i.HistoryMode != "published" && len(run.Audit.History) > 0 {
			return errors.New("agenteval: unexpected historical disclosure")
		}
		if i.HistoryMode == "published" {
			if len(run.Audit.History) > 1 || (len(run.Audit.History) == 0 && (!run.InfrastructureFailure || run.Audit.ModelCalls != 0)) {
				return errors.New("agenteval: history treatment not observed")
			}
			for _, h := range run.Audit.History {
				if !validHistoryRecord(h) || h.Stage != run.Scenario {
					return errors.New("agenteval: inconsistent history audit")
				}
				if old, ok := historyViews[run.Scenario]; ok && (old.ViewSHA256 != h.ViewSHA256 || old.ViewBytes != h.ViewBytes) {
					return errors.New("agenteval: historical view changed between repetitions")
				}
				historyViews[run.Scenario] = h
			}
		}
		if !allowed[run.Scenario] || run.Repetition < 1 || run.Repetition > i.Repetitions || seen[run.Scenario][run.Repetition] || run.RunID == "" || ids[run.RunID] || run.ElapsedMS < 0 {
			return errors.New("agenteval: duplicate or missing run")
		}
		for _, c := range []Count{run.Scores.Samples, run.Scores.Fields, run.Scores.Negative, run.Audit.Tools} {
			if c.Total < 0 || c.Matched < 0 || c.Matched > c.Total {
				return errors.New("agenteval: invalid metric counts")
			}
		}
		if run.Scores.Samples.Total == 0 || run.Scores.Fields.Total == 0 || run.Scores.Negative.Total == 0 || run.Scores.Negative.Total >= run.Scores.Samples.Total {
			return errors.New("agenteval: fixed denominators missing")
		}
		d := [3]int{run.Scores.Samples.Total, run.Scores.Fields.Total, run.Scores.Negative.Total}
		if old, ok := denominators[run.Scenario]; ok && old != d {
			return errors.New("agenteval: repetitions have different denominators")
		}
		denominators[run.Scenario] = d
		if run.InputBytes < 0 || run.Scores.ExecutionFailures < 0 || run.Scores.ExecutionFailures > run.Scores.Samples.Total || run.Audit.InfrastructureFailures < 0 || run.Audit.ModelCalls < 0 || run.Audit.Corrections < 0 || (run.Audit.Tokens != nil && (run.Audit.Tokens.Input < 0 || run.Audit.Tokens.Output < 0)) {
			return errors.New("agenteval: invalid diagnostic metrics")
		}
		if !run.InfrastructureFailure && (run.Scores.ExecutionFailures > 0 || run.Audit.Incomplete || run.Audit.InfrastructureFailures > 0) {
			return errors.New("agenteval: execution failure not marked")
		}
		if run.ReadyForReview && (run.InfrastructureFailure || run.Audit.Incomplete || run.Scores.ExecutionFailures != 0 || run.Status != agentstore.AwaitingReview || run.Scores.Samples.Matched != run.Scores.Samples.Total || run.Scores.Fields.Matched != run.Scores.Fields.Total || run.Scores.Negative.Matched != run.Scores.Negative.Total || run.Candidate.ID == "" || run.Candidate.Version == "" || run.CandidateDigest == "" || run.Failure != "" || run.TrainingReportID == "" || run.HoldoutReportID == "") {
			return errors.New("agenteval: invalid review eligibility")
		}
		if run.EstimatedMicros != nil && (i.PriceVersion == "" || i.Currency == "" || run.Audit.Tokens == nil || *run.EstimatedMicros < 0) {
			return errors.New("agenteval: cost without versioned known usage")
		}
		seen[run.Scenario][run.Repetition], ids[run.RunID] = true, true
	}
	return nil
}

type Spread struct{ Min, Max, Mean float64 }
type Summary struct {
	Scenario                                                    string
	SampleRate, FieldRate, NegativeRate, Corrections, ElapsedMS Spread
	ToolErrorRate                                               *Spread
	InfrastructureFailures                                      int
	ModelCalls, InputBytes, HistoryBytes                        Spread
}

func spread(values []float64) Spread {
	s := Spread{Min: math.Inf(1), Max: math.Inf(-1)}
	for _, v := range values {
		s.Min = math.Min(s.Min, v)
		s.Max = math.Max(s.Max, v)
		s.Mean += v
	}
	s.Mean /= float64(len(values))
	return s
}

func (r Report) Summaries() ([]Summary, error) {
	if err := r.ValidateComplete(); err != nil {
		return nil, err
	}
	var result []Summary
	for _, name := range r.Identity.Scenarios {
		var samples, fields, negative, tools, corrections, elapsed, calls, input, history []float64
		s := Summary{Scenario: name}
		for _, run := range r.Runs {
			if run.Scenario == name {
				samples = append(samples, run.Scores.Samples.Fraction())
				fields = append(fields, run.Scores.Fields.Fraction())
				negative = append(negative, run.Scores.Negative.Fraction())
				// No attempted tool is an undefined error rate, not evidence of zero errors.
				if run.Audit.Tools.Total > 0 {
					tools = append(tools, run.Audit.Tools.Fraction())
				}
				corrections = append(corrections, float64(run.Audit.Corrections))
				elapsed = append(elapsed, float64(run.ElapsedMS))
				calls = append(calls, float64(run.Audit.ModelCalls))
				input = append(input, float64(run.InputBytes))
				historyBytes := 0
				for _, h := range run.Audit.History {
					historyBytes += h.ViewBytes
				}
				history = append(history, float64(historyBytes))
				if run.InfrastructureFailure {
					s.InfrastructureFailures++
				}
			}
		}
		s.SampleRate = spread(samples)
		s.FieldRate = spread(fields)
		s.NegativeRate = spread(negative)
		s.Corrections = spread(corrections)
		s.ElapsedMS = spread(elapsed)
		s.ModelCalls = spread(calls)
		s.InputBytes = spread(input)
		s.HistoryBytes = spread(history)
		if len(tools) > 0 {
			v := spread(tools)
			s.ToolErrorRate = &v
		}
		result = append(result, s)
	}
	return result, nil
}

type Comparison struct{ Before, After []Summary }

func Compare(before, after Report) (Comparison, error) {
	if err := before.ValidateComplete(); err != nil {
		return Comparison{}, err
	}
	if err := after.ValidateComplete(); err != nil {
		return Comparison{}, err
	}
	if !reflect.DeepEqual(before.Identity, after.Identity) {
		return Comparison{}, errors.New("agenteval: protocol, corpus, model or budget differs")
	}
	// Also reject changed metric denominators even with a copied identity.
	a, b := append([]Run(nil), before.Runs...), append([]Run(nil), after.Runs...)
	less := func(rs []Run) func(int, int) bool {
		return func(i, j int) bool {
			if rs[i].Scenario == rs[j].Scenario {
				return rs[i].Repetition < rs[j].Repetition
			}
			return rs[i].Scenario < rs[j].Scenario
		}
	}
	sort.Slice(a, less(a))
	sort.Slice(b, less(b))
	for i := range a {
		if a[i].Scores.Samples.Total != b[i].Scores.Samples.Total || a[i].Scores.Fields.Total != b[i].Scores.Fields.Total || a[i].Scores.Negative.Total != b[i].Scores.Negative.Total {
			return Comparison{}, errors.New("agenteval: metric denominators differ")
		}
		if before.Identity.HistoryMode == "published" && len(a[i].Audit.History) == 1 && len(b[i].Audit.History) == 1 {
			old, current := a[i].Audit.History[0], b[i].Audit.History[0]
			if old.ViewSHA256 != current.ViewSHA256 || old.ViewBytes != current.ViewBytes {
				return Comparison{}, errors.New("agenteval: historical view differs")
			}
		}
	}
	x, _ := before.Summaries()
	y, _ := after.Summaries()
	return Comparison{Before: x, After: y}, nil
}
