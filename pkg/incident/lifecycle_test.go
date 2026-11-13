package incident

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestLifecycleVerificationDistinguishesProbeFromFullProof(t *testing.T) {
	l := Lifecycle{Checks: []StageCheck{
		{Stage: "partial", State: "passed", Verification: "task", ProbeSuccesses: 1, Verified: false},
		{Stage: "first", State: "pending", Verification: "readonly", Coverage: "read and parse", ProbeSuccesses: 1},
		{Stage: "complete", State: "passed", Verification: "readonly", Coverage: "read and parse", ProbeSuccesses: 2, Verified: true},
		{Stage: "failed", State: "failed", Verification: "readonly", Coverage: "read and parse", ProbeSuccesses: 2, Detail: "late failure"},
		{Stage: "real", State: "passed", Verification: "task", TaskSuccesses: 1, Verified: true},
	}}
	clean := l.Clean()
	for i, want := range []bool{false, false, true, false, true} {
		if clean.Checks[i].Verified != want {
			t.Fatalf("wrong verification: %+v", clean.Checks[i])
		}
	}
	if l.Checks[0].VerificationNote != "" {
		t.Fatal("report cleaning changed source")
	}
	for _, part := range []string{"未验证：仍需本轮真实任务成功", "read and parse", "完整只读验证通过", "late failure"} {
		if !strings.Contains(clean.Text(), part) {
			t.Fatalf("text missing %q", part)
		}
	}
	data, err := json.Marshal(clean)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"verification":"readonly"`) || !strings.Contains(string(data), `"verified":false`) {
		t.Fatal("JSON omits verification")
	}
}

func TestLifecycleCleanDetachedBoundedAndRedacted(t *testing.T) {
	ok := true
	l := Lifecycle{Capture: Capture{ID: "one", Site: "synthetic", Failures: 1}, State: "manual", Revision: 1,
		Checks:   make([]StageCheck, MaxStageChecks+1),
		Evidence: make([]Sample, MaxSamples+1), Timeline: make([]Transition, MaxTransitions+3),
		Hypotheses: []string{"HTTP 429 token=hidden-value"}, Conclusion: "password=private-value"}
	l.Evidence[0] = Sample{Error: "HTTP 429 token=hidden-value", URL: "https://user:private-value@example.invalid/a?token=hidden-value"}
	l.Timeline[0] = Transition{Action: "initial", ProbeOK: &ok, Reason: "token=hidden-value"}
	l.Checks[0] = StageCheck{Stage: "detail", State: "failed", Detail: "token=hidden-value"}
	l.Timeline[len(l.Timeline)-1].Action = "latest"
	clean := l.Clean()
	if !clean.ChecksLimited || len(clean.Checks) != MaxStageChecks {
		t.Fatal("stage verification bounds lost")
	}
	if !clean.Active() || !clean.Limited || len(clean.Evidence) != MaxSamples || len(clean.Timeline) != MaxTransitions {
		t.Fatalf("invalid bounds: %+v", clean)
	}
	if clean.Timeline[0].Action != "initial" || clean.Timeline[len(clean.Timeline)-1].Action != "latest" {
		t.Fatal("lost initial or final transition")
	}
	encoded, _ := json.Marshal(clean)
	if strings.Contains(string(encoded), "hidden-value") || strings.Contains(string(encoded), "private-value") {
		t.Fatal("credential leaked")
	}
	if !strings.Contains(clean.Evidence[0].Error, "429") {
		t.Fatal("status removed")
	}
	*clean.Timeline[0].ProbeOK = false
	clean.Evidence[0].Error = "changed"
	clean.Hypotheses[0] = "changed"
	clean.Checks[0].Detail = "changed"
	if l.Checks[0].Detail == "changed" {
		t.Fatal("mutated live stage verification")
	}
	if !ok || l.Evidence[0].Error == "changed" || l.Hypotheses[0] == "changed" {
		t.Fatal("snapshot mutated live evidence")
	}
}

func TestLifecycleFinalTextAndAnalysis(t *testing.T) {
	now := time.Now()
	l := Lifecycle{Capture: Capture{ID: "one", Site: "synthetic", Failures: 1, At: now}, State: "recovered", Revision: 3,
		Evidence: []Sample{{Error: "HTTP 503 token=hidden-value"}}, FinalAt: now, Conclusion: "恢复已验证",
		Hypotheses: []string{"短时不可用"}, Contradictions: []string{"部分请求成功"}, Missing: []string{"服务端日志"},
		Timeline: []Transition{{At: now, From: "probing", To: "recovered", Action: "probe"}}}
	if l.Active() {
		t.Fatal("recovered still active")
	}
	if r := l.Analysis(); !r.Complete || r.Observed != 1 {
		t.Fatalf("analysis: %+v", r)
	}
	report := l.Text()
	for _, want := range []string{"恢复已验证", "反证：部分请求成功", "缺失信息：服务端日志", "结案：", "probing → recovered", "503"} {
		if !strings.Contains(report, want) {
			t.Fatalf("missing %q", want)
		}
	}
	if strings.Contains(report, "hidden-value") {
		t.Fatal("text credential leak")
	}
}

func TestLifecycleStructuredMetricsAndHypotheses(t *testing.T) {
	l := Lifecycle{Capture: Capture{ID: "one", Site: "synthetic", Failures: 5}, WindowFailures: 12, WindowSuccesses: 9, ShortFailures: 4, DistinctTasks: 3,
		Groups: []FailureGroup{{Stage: "detail", Category: "not_found", Fingerprint: "fp1", Count: 8, Tasks: 2}, {Stage: "detail", Category: "rate_limit", Fingerprint: "fp2", Count: 4, Tasks: 1}}}
	r := l.Analysis()
	joined := strings.Join(append(append(r.Hypotheses, r.Contradictions...), r.Missing...), "\n")
	for _, want := range []string{"迁移", "限流", "AutoThrottle 未开启", "仍有成功样本", "多类错误"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing structured inference %s", want)
		}
	}
	if l.Capture.Failures != 5 {
		t.Fatal("trigger capture overwritten")
	}
	for _, want := range []string{"触发瞬间失败：5", "当前窗口失败：12", "当前窗口成功：9", "短窗口失败：4", "fp1", "fp2"} {
		if !strings.Contains(l.Text(), want) {
			t.Fatalf("missing metric %s", want)
		}
	}
	l.Groups = make([]FailureGroup, MaxFailureGroups+1)
	l.Groups[0] = FailureGroup{Category: "local", Fingerprint: "token=secret-value", Count: 1}
	clean := l.Clean()
	if !clean.Limited || len(clean.Groups) != MaxFailureGroups || strings.Contains(clean.Groups[0].Fingerprint, "secret-value") {
		t.Fatal("group bounds/redaction")
	}
	clean.Groups[0].Category = "changed"
	if l.Groups[0].Category != "local" {
		t.Fatal("mutated engine group")
	}
	local := l.Analysis()
	if !strings.Contains(strings.Join(local.Contradictions, " "), "不构成目标网站宕机") {
		t.Fatal("local error misdiagnosed")
	}
}
