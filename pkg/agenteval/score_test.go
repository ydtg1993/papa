package agenteval

import (
	"encoding/json"
	"net/url"
	"testing"

	"github.com/ydtg1993/papa/v4/pkg/agentstore"
	"github.com/ydtg1993/papa/v4/pkg/crawlagent"
	"github.com/ydtg1993/papa/v4/pkg/crawlrule"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

func fixedFixtures() []crawlrule.Fixture {
	u, _ := url.Parse("https://fixture.test/one")
	return []crawlrule.Fixture{
		{Name: "positive", Response: &htmlfetch.Response{URL: u, StatusCode: 200, Body: []byte(`{"items":[{"title":"good","summary":"right"}]}`)}, Expect: crawlrule.Expectations{RowCount: 1, Fields: []crawlrule.FieldExpectation{{Pointer: "/title", Value: "good"}, {Pointer: "/summary", Value: "right"}}}},
		{Name: "empty", Response: &htmlfetch.Response{URL: u, StatusCode: 200, Body: []byte(`{"items":[]}`)}, Expect: crawlrule.Expectations{Kind: "empty"}},
	}
}

func TestScoreFixedDenominatorsAndPartialFields(t *testing.T) {
	fixtures := fixedFixtures()
	missing, err := Score(nil, fixtures)
	if err != nil || missing.Samples != (Count{0, 2}) || missing.Fields != (Count{0, 2}) || missing.Negative != (Count{0, 1}) {
		t.Fatalf("missing candidate inflated score: %+v %v", missing, err)
	}
	rule := crawlrule.Rule{SchemaVersion: 1, ID: "score", Version: "v1", Format: "json", Rows: "/items", Fields: []crawlrule.Field{{Name: "title", Pointer: "/title", Validation: crawlrule.Validation{Kind: "string", Required: true}}, {Name: "summary", Pointer: "/absent", Validation: crawlrule.Validation{Kind: "string", Required: true}}}}
	c, err := crawlrule.Compile(rule)
	if err != nil {
		t.Fatal(err)
	}
	score, err := Score(c, fixtures)
	if err != nil || score.Samples != (Count{1, 2}) || score.Fields != (Count{1, 2}) || score.Negative != (Count{1, 1}) {
		t.Fatalf("partial field accuracy: %+v %v", score, err)
	}
	fixtures[1].Response.Body = []byte(`{"items":`)
	score, err = Score(c, fixtures)
	if err != nil || score.Negative.Matched != 0 || score.ExecutionFailures != 1 || score.Samples.Total != 2 {
		t.Fatalf("parse failure accepted/dropped: %+v %v", score, err)
	}
	if _, err = Score(c, fixtures[1:]); err == nil {
		t.Fatal("all-negative evaluation accepted")
	}
}

func TestAuditUnknownUsageAndReservedTools(t *testing.T) {
	steps := []agentstore.Step{
		{Kind: "model", Status: agentstore.StepSucceeded, OutputJSON: `{"usage":{"Known":true,"InputTokens":7,"OutputTokens":3}}`},
		{Kind: "tool", Status: agentstore.StepFailed, InputJSON: `{"tool":"submit_candidate"}`, OutputJSON: `{"error":"invalid_action"}`},
		{Kind: "tool", Status: agentstore.StepSucceeded, InputJSON: `{"tool":"submit_candidate"}`},
		{Kind: "tool", Status: agentstore.StepUnknown, InputJSON: `{"tool":"fetch_page"}`},
	}
	m := MeasureAudit(steps)
	if m.Tokens == nil || m.Tokens.Input != 7 || m.Tools != (Count{2, 3}) || m.Corrections != 1 || !m.Incomplete || m.InfrastructureFailures != 0 {
		t.Fatalf("audit metrics: %+v", m)
	}
	steps = append(steps, agentstore.Step{Kind: "model", Status: agentstore.StepSucceeded, OutputJSON: `{"usage":{"Known":false}}`})
	if m = MeasureAudit(steps); m.Tokens != nil || m.ModelCalls != 2 {
		t.Fatalf("unknown usage became zero: %+v", m)
	}
	steps = append(steps, agentstore.Step{Kind: "tool", Status: agentstore.StepFailed, InputJSON: `{"tool":"fetch_page"}`, OutputJSON: `{"error":"operation_failed"}`})
	if m = MeasureAudit(steps); m.InfrastructureFailures != 1 {
		t.Fatalf("recovered transport failure hidden: %+v", m)
	}
}

func reportFixture() Report {
	r := Report{Identity: Identity{Protocol: ProtocolVersion, CorpusSHA256: crawlagent.PromptDigest(), PromptSHA256: crawlagent.PromptDigest(), ToolProtocol: crawlagent.ToolProtocolVersion, Mode: "replay", Model: "mock", EndpointSHA256: crawlagent.PromptDigest(), Scenarios: []string{"a"}, Repetitions: 3}}
	for i := 1; i <= 3; i++ {
		r.Runs = append(r.Runs, Run{Scenario: "a", Repetition: i, RunID: string(rune('a' + i)), Status: agentstore.AwaitingReview, Scores: Scores{Samples: Count{i, 3}, Fields: Count{i, 4}, Negative: Count{1, 1}}, ElapsedMS: int64(i * 10)})
	}
	return r
}

func cloneReport(r Report) Report {
	b, _ := json.Marshal(r)
	var out Report
	_ = json.Unmarshal(b, &out)
	return out
}

func TestCompareIdentityDenominatorsAndAllRepetitions(t *testing.T) {
	before := reportFixture()
	after := cloneReport(before)
	cmp, err := Compare(before, after)
	if err != nil || cmp.After[0].SampleRate.Min != 1.0/3 || cmp.After[0].SampleRate.Max != 1 || cmp.After[0].ElapsedMS.Mean != 20 || cmp.After[0].ToolErrorRate != nil {
		t.Fatalf("comparison/spread: %+v %v", cmp, err)
	}
	for _, mutate := range []func(*Report){
		func(r *Report) { r.Runs = r.Runs[:2] }, func(r *Report) { r.Runs[2] = r.Runs[1] },
		func(r *Report) { r.Identity.Model = "different" }, func(r *Report) { r.Identity.Budgets.MaxModelCalls++ }, func(r *Report) { r.Identity.PromptSHA256 = "changed" },
		func(r *Report) { r.Runs[0].Scores.Fields.Total++ }, func(r *Report) { r.Runs[0].ReadyForReview = true },
		func(r *Report) { zero := int64(0); r.Runs[0].EstimatedMicros = &zero },
	} {
		after = cloneReport(before)
		mutate(&after)
		if _, err := Compare(before, after); err == nil {
			t.Fatalf("incomparable/forged report accepted: %+v", after)
		}
	}
}
