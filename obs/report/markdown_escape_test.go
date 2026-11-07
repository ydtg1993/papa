package report

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/obs"
)

func TestMarkdownEscapesDynamicStructure(t *testing.T) {
	const payload = "text|cell\r\n## injected\n<img src=x> [go](https://evil.test) `code` &amp;"
	rep := sampleReport()
	rep.Run.RunIDs = []string{payload}
	rep.Run.From, rep.Run.To = time.Unix(0, 0), time.Unix(1, 0)
	rep.VerdictReason = payload
	rep.Environment = map[string]string{payload: payload}
	rep.Degraded = []string{payload}
	rep.Inputs.PapaVersion, rep.Inputs.PapaCommit, rep.Inputs.ConfigFile = payload, payload, payload
	rep.Inputs.ThresholdsSource, rep.Inputs.Thresholds = payload, map[string]string{payload: payload}
	rep.Checks = []obs.Check{{ID: payload, Severity: obs.Severity(payload), Skipped: true, SkipReason: payload, Message: payload, Detail: payload}}
	rep.Problems = []obs.Problem{{ID: payload, Title: payload, Severity: obs.Severity(payload), Scope: payload,
		Evidence:   []obs.Evidence{{Type: payload, Ref: payload, Excerpt: payload}},
		Hypotheses: []obs.Hypothesis{{Text: payload, Confidence: payload, Verify: payload, FixHint: payload}}, Acceptance: payload}}
	rep.Sections = []obs.SectionReport{{Title: payload, Summary: payload, Tables: []obs.Table{{Title: payload, Columns: []string{payload, "normal"}, Rows: [][]string{{payload, "value"}}}}}}
	rep.Engine.Sites = []obs.SiteState{{Key: payload, Paused: true}}
	for name, render := range map[string]func(*obs.Report) string{"report": Markdown, "ai-brief": AIBriefMarkdown} {
		t.Run(name, func(t *testing.T) {
			md := render(rep)
			for _, injected := range []string{"\n## injected", "<img src=x>", "[go](https://evil.test)", "text|cell"} {
				if strings.Contains(md, injected) {
					t.Errorf("dynamic content escaped Markdown context: %q", injected)
				}
			}
			for _, line := range strings.Split(md, "\n") {
				if strings.HasPrefix(line, "|") && strings.Count(line, "|") != 6 && strings.Count(line, "|") != 3 {
					t.Errorf("dynamic pipe changed table width: %q", line)
				}
			}
		})
	}
}

func TestMarkdownEvidenceCannotCloseCodeSpan(t *testing.T) {
	for _, ref := range []string{"`[go](https://evil.test)", "`` <img src=x> | next", "backslash\\`[go](https://evil.test)"} {
		rep := sampleReport()
		rep.Problems[0].Evidence[0].Ref = ref
		for name, md := range map[string]string{"report": Markdown(rep), "ai-brief": AIBriefMarkdown(rep)} {
			if strings.Contains(md, "[go](https://evil.test)") || strings.Contains(md, "<img src=x>") || strings.Contains(md, "| next") {
				t.Errorf("%s evidence escaped code span: %q", name, ref)
			}
		}
	}
}

func TestMarkdownEscapingDoesNotChangeJSONOrSource(t *testing.T) {
	const value = "plain|cell\n[read](https://example.test) `code` <b> &"
	rep := sampleReport()
	rep.Problems[0].Title = value
	_ = Markdown(rep)
	_ = AIBriefMarkdown(rep)
	for name, encode := range map[string]func(*obs.Report) ([]byte, error){"report": JSON, "ai-brief": AIBriefJSON} {
		raw, err := encode(rep)
		if err != nil {
			t.Fatal(err)
		}
		var decoded struct {
			Problems []obs.Problem `json:"problems"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.Problems[0].Title != value {
			t.Errorf("%s JSON contains Markdown escaping: %q", name, decoded.Problems[0].Title)
		}
	}
	if rep.Problems[0].Title != value {
		t.Fatal("Markdown rendering mutated source")
	}
}

func TestMarkdownInlineTextPreservesReadableValues(t *testing.T) {
	for _, value := range []string{"engine_idle", "engine.log:12", "v3.8.0", "pending 1200", "papa rod --url x", "约 15h"} {
		if got := markdownText(value); got != value {
			t.Errorf("ordinary text changed: %q -> %q", value, got)
		}
		if got := markdownCode(value); got != "`"+value+"`" {
			t.Errorf("ordinary code changed: %q -> %q", value, got)
		}
	}
	for _, value := range []string{"\t- injected", "1. injected", "1) injected", "---", "_emphasis_", "&#91;go&#93;(https://evil.test)"} {
		got := markdownText(value)
		if got == strings.TrimSpace(value) || strings.ContainsAny(got, "\r\n\t") {
			t.Errorf("inline boundary not protected: %q -> %q", value, got)
		}
	}
}
