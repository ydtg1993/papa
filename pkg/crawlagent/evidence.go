package crawlagent

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/andybalholm/cascadia"
	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
	"github.com/ydtg1993/papa/v3/pkg/rulestore"
	"github.com/ydtg1993/papa/v3/pkg/webaccess"
)

func (e *execution) loadFixtures(ctx context.Context) error {
	training := append([]string(nil), e.goal.EvidenceSamples...)
	fixtureCount := len(training)
	for _, stage := range e.goal.Stages {
		if stage.HoldoutSet != "" {
			set, err := e.agent.config.Repository.SampleSet(ctx, stage.HoldoutSet)
			if err != nil {
				return ErrEvidence
			}
			for _, f := range set.Fixtures {
				fixtureCount++
				if fixtureCount > 64 {
					return ErrBudget
				}
				s, err := e.agent.config.Repository.Sample(ctx, f.SampleID)
				if err != nil {
					return ErrEvidence
				}
				e.hiddenDigests[s.BodySHA256] = true
				if err := e.hideURL(s.URL); err != nil {
					return err
				}
				body, readErr := e.agent.config.Blobs.Get(ctx, s.BodySHA256, rulestore.MaxSampleBodyBytes)
				if readErr != nil || len(body) != s.BodyBytes || hashBytes(body) != s.BodySHA256 {
					return ErrEvidence
				}
				if err := checkEvidenceBudget(body); err != nil {
					return err
				}
				clean, cleanErr := rulestore.SanitizeSample(rulestore.SampleInput{URL: s.URL, StatusCode: s.StatusCode, Rendered: s.Rendered, Body: body}, e.agent.config.SamplePolicy)
				if cleanErr != nil {
					return ErrEvidence
				}
				e.hiddenDigests[clean.BodySHA256] = true
				if err := e.hideURL(clean.URL); err != nil {
					return err
				}
			}
		}
		if stage.TrainingSet != "" {
			set, err := e.agent.config.Repository.SampleSet(ctx, stage.TrainingSet)
			if err != nil {
				return ErrEvidence
			}
			for _, f := range set.Fixtures {
				fixtureCount++
				if fixtureCount > 64 {
					return ErrBudget
				}
				training = append(training, f.SampleID)
			}
		}
	}
	seen := map[string]bool{}
	for _, id := range training {
		if seen[id] {
			continue
		}
		seen[id] = true
		s, err := e.agent.config.Repository.Sample(ctx, id)
		if err != nil {
			return ErrEvidence
		}
		hidden, hiddenErr := e.isHiddenURL(s.URL)
		if e.hiddenDigests[s.BodySHA256] || hidden || hiddenErr != nil {
			return ErrInvalid
		}
		if len(e.evidence) >= 64 {
			return ErrBudget
		}
		// Confirm persisted training evidence is still complete before exposing it.
		body, err := e.agent.config.Blobs.Get(ctx, s.BodySHA256, rulestore.MaxSampleBodyBytes)
		if err != nil || len(body) != s.BodyBytes || hashBytes(body) != s.BodySHA256 {
			return ErrEvidence
		}
		// Retention approval and disclosure to a model are separate decisions.
		// Reapply this analysis's policy even for an existing training sample.
		if _, err := e.saveEvidence(ctx, webaccess.Response{URL: s.URL, StatusCode: s.StatusCode, Rendered: s.Rendered, Body: body}); err != nil {
			return err
		}
	}
	for _, seed := range e.goal.Seeds {
		hidden, hiddenErr := e.isHiddenURL(seed)
		if hidden || hiddenErr != nil {
			return ErrInvalid
		}
	}
	return nil
}

func fromSample(s rulestore.Sample, kind string) Evidence {
	return Evidence{ID: s.ID, URL: s.URL, BodySHA256: s.BodySHA256, BodyBytes: s.BodyBytes, StatusCode: s.StatusCode, Rendered: s.Rendered, ContentType: clip(kind, 256)}
}

func (e *execution) saveEvidence(ctx context.Context, response webaccess.Response) (Evidence, error) {
	if len(e.evidence) >= 64 || len(response.Body) > rulestore.MaxSampleBodyBytes {
		return Evidence{}, ErrBudget
	}
	if err := checkEvidenceBudget(response.Body); err != nil {
		return Evidence{}, err
	}
	hidden, hiddenErr := e.isHiddenURL(response.URL)
	if hidden || hiddenErr != nil {
		return Evidence{}, ErrEvidence
	}
	s, err := e.agent.config.Repository.AddSample(ctx, rulestore.SampleInput{URL: response.URL, StatusCode: response.StatusCode, Rendered: response.Rendered, Body: response.Body, EvidenceID: e.run.ID}, e.agent.config.SamplePolicy, e.agent.config.Actor)
	if err != nil {
		return Evidence{}, ErrEvidence
	}
	hidden, hiddenErr = e.isHiddenURL(s.URL)
	if e.hiddenDigests[s.BodySHA256] || hidden || hiddenErr != nil {
		return Evidence{}, ErrEvidence
	}
	// A custom redactor may change the tree; validate the actual retained bytes.
	body, err := e.agent.config.Blobs.Get(ctx, s.BodySHA256, rulestore.MaxSampleBodyBytes)
	if err != nil || len(body) != s.BodyBytes || hashBytes(body) != s.BodySHA256 {
		return Evidence{}, ErrEvidence
	}
	if err := checkEvidenceBudget(body); err != nil {
		return Evidence{}, err
	}
	item := fromSample(s, response.ContentType)
	e.evidence = append(e.evidence, item)
	return item, nil
}

func (e *execution) readEvidence(ctx context.Context, id string) (Evidence, []byte, error) {
	if id == "latest" && len(e.evidence) > 0 {
		id = e.evidence[len(e.evidence)-1].ID
	}
	for _, item := range e.evidence {
		if item.ID != id {
			continue
		}
		hidden, hiddenErr := e.isHiddenURL(item.URL)
		if e.hiddenDigests[item.BodySHA256] || hidden || hiddenErr != nil {
			return Evidence{}, nil, ErrEvidence
		}
		body, err := e.agent.config.Blobs.Get(ctx, item.BodySHA256, rulestore.MaxSampleBodyBytes)
		if err != nil || len(body) != item.BodyBytes || hashBytes(body) != item.BodySHA256 {
			return Evidence{}, nil, ErrEvidence
		}
		if err := checkEvidenceBudget(body); err != nil {
			return Evidence{}, nil, err
		}
		return item, body, nil
	}
	return Evidence{}, nil, ErrEvidence
}

func (e *execution) inspect(ctx context.Context, a action) (any, error) {
	item, body, err := e.readEvidence(ctx, a.EvidenceID)
	if err != nil {
		return nil, err
	}
	if a.Selector != "" {
		if a.Pointer != "" {
			return nil, ErrAction
		}
		if err := checkSelector(a.Selector); err != nil {
			return nil, err
		}
		if err := checkHTMLTreeBudget(body); err != nil {
			return nil, err
		}
		matcher, err := cascadia.Compile(a.Selector)
		if err != nil {
			return nil, ErrAction
		}
		doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
		if err != nil {
			return nil, ErrEvidence
		}
		nodes := doc.FindMatcher(matcher)
		values := []string{}
		nodes.EachWithBreak(func(i int, n *goquery.Selection) bool {
			if i >= 16 {
				return false
			}
			v := n.Text()
			if a.Attribute != "" {
				v, _ = n.Attr(a.Attribute)
			}
			values = append(values, clip(v, 1024))
			return true
		})
		return map[string]any{"evidence_id": item.ID, "matches": nodes.Length(), "values": values, "truncated": nodes.Length() > 16}, nil
	}
	if a.Attribute != "" {
		return nil, ErrAction
	}
	if err := checkPointerComplexity(a.Pointer); err != nil {
		return nil, err
	}
	var value any
	if htmlfetch.DecodeJSON(body, &value) != nil {
		return nil, ErrEvidence
	}
	value, ok := lookupPointer(value, a.Pointer)
	if !ok {
		return map[string]any{"evidence_id": item.ID, "found": false}, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, ErrEvidence
	}
	if len(data) > 16<<10 {
		return map[string]any{"evidence_id": item.ID, "found": true, "preview": clip(string(data), 16<<10), "truncated": true}, nil
	}
	return map[string]any{"evidence_id": item.ID, "found": true, "value": value}, nil
}

// Match the actual evaluator, not the response's apparent format: a model can
// select HTML against a JSON string containing markup, or JSON against HTML.
func checkEvaluationBudget(rule crawlrule.Rule, body []byte) error {
	if len(body) > maxAgentEvidenceBytes {
		return ErrBudget
	}
	if rule.Format == "html" {
		return checkHTMLTreeBudget(body)
	}
	return checkJSONTreeBudget(body)
}

func (e *execution) checkFixtureEvaluation(ctx context.Context, ref rulestore.Reference, setID string) error {
	v, err := e.agent.config.Repository.Get(ctx, ref)
	if err != nil {
		return ErrEvidence
	}
	if err := checkRuleComplexity(v.Rule); err != nil {
		return err
	}
	set, err := e.agent.config.Repository.SampleSet(ctx, setID)
	if err != nil {
		return ErrEvidence
	}
	if len(set.Fixtures) > 64 {
		return ErrBudget
	}
	for _, fixture := range set.Fixtures {
		s, err := e.agent.config.Repository.Sample(ctx, fixture.SampleID)
		if err != nil {
			return ErrEvidence
		}
		body, err := e.agent.config.Blobs.Get(ctx, s.BodySHA256, maxAgentEvidenceBytes)
		if err != nil || len(body) != s.BodyBytes || hashBytes(body) != s.BodySHA256 {
			return ErrEvidence
		}
		if err := checkEvaluationBudget(v.Rule, body); err != nil {
			return err
		}
	}
	return nil
}

func clip(value string, n int) string {
	if len(value) > n {
		return strings.ToValidUTF8(value[:n], "�")
	}
	return value
}

func lookupPointer(value any, pointer string) (any, bool) {
	if pointer == "" {
		return value, true
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, false
	}
	parts := strings.Split(pointer[1:], "/")
	if len(parts) > 64 {
		return nil, false
	}
	for _, part := range parts {
		for i := 0; i < len(part); i++ {
			if part[i] == '~' {
				if i+1 >= len(part) || (part[i+1] != '0' && part[i+1] != '1') {
					return nil, false
				}
				i++
			}
		}
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		switch v := value.(type) {
		case map[string]any:
			var ok bool
			value, ok = v[part]
			if !ok {
				return nil, false
			}
		case []any:
			if part == "" || (len(part) > 1 && part[0] == '0') {
				return nil, false
			}
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(v) {
				return nil, false
			}
			value = v[i]
		default:
			return nil, false
		}
	}
	return value, true
}
