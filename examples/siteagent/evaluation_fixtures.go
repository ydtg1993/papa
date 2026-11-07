package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"

	"github.com/ydtg1993/papa/v3/pkg/crawlagent"
	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
)

const evaluationCorpusVersion = "synthetic-18-v1"
const evaluationRenderDelayMS = 800
const evaluationGoalContext = "Extract the declared required fields from each record. Preserve pagination when present. Inspect both training pages; return a reusable rule. Leave publication to the host."

type evaluationFixture struct {
	Name, Path, Body string
	Expect           crawlrule.Expectations
}

type evaluationScenario struct {
	ID, Transport     string
	Fields            []crawlagent.FieldGoal
	Oracle            crawlrule.Rule
	Training, Holdout []evaluationFixture
}

// The corpus has no live-site expectations. Relative fixture URLs keep its
// identity stable across ephemeral local ports. Oracle rules are Replay-only.
func evaluationCorpus() []evaluationScenario {
	text := crawlrule.Validation{Kind: "string", Required: true, MinLength: 1}
	var scenarios []evaluationScenario
	for _, id := range []string{"static-html", "rendered-dom", "json-pagination"} {
		s := evaluationScenario{ID: id, Transport: "http", Fields: []crawlagent.FieldGoal{{Name: "title", Kind: "string", Required: true, Description: "Title from the record"}, {Name: "summary", Kind: "string", Required: true, Description: "Summary from the record"}}}
		s.Oracle = crawlrule.Rule{SchemaVersion: 1, Format: "html", Rows: "article.item", Fields: []crawlrule.Field{{Name: "title", Selector: "h2", Validation: text}, {Name: "summary", Selector: ".summary", Validation: text}}}
		if id == "rendered-dom" {
			s.Transport = "browser"
			s.Oracle.Rows = "section.card"
		}
		if id == "json-pagination" {
			s.Oracle = crawlrule.Rule{SchemaVersion: 1, Format: "json", Rows: "/items", Fields: []crawlrule.Field{{Name: "title", Pointer: "/title", Validation: text}, {Name: "summary", Pointer: "/summary", Validation: text}}, Next: &crawlrule.Next{Pointer: "/next"}}
		}
		for _, split := range []string{"train", "holdout"} {
			words := []string{"Copper", "Silver"}
			if split == "holdout" {
				words = []string{"Indigo", "Saffron"}
			}
			var fixtures []evaluationFixture
			for i, word := range words {
				f := evaluationFixture{Name: fmt.Sprintf("%s-%d", split, i+1), Path: fmt.Sprintf("/%s/%s?page=%d", id, split, i+1)}
				title, summary := id+" "+word, word+" fixed description"
				f.Expect = expected(title, "summary", summary)
				if id == "json-pagination" {
					next := ""
					if i == 0 {
						next = "/" + id + "/" + split + "?page=2"
						f.Expect.NextURL = next
					}
					body, _ := json.Marshal(map[string]any{"items": []any{map[string]string{"title": title, "summary": summary}}, "next": next})
					f.Body = string(body)
				} else if id == "rendered-dom" {
					f.Body = `<section class="card"><h2>` + title + `</h2><p class="summary">` + summary + `</p></section>`
				} else {
					f.Body = `<article class="item"><h2>` + title + `</h2><p class="summary">` + summary + `</p></article>`
				}
				fixtures = append(fixtures, f)
			}
			empty := evaluationFixture{Name: split + "-empty", Path: "/" + id + "/" + split + "/empty", Body: "<main>" + id + " " + split + " empty</main>", Expect: crawlrule.Expectations{Kind: "empty"}}
			if id == "json-pagination" {
				empty.Body = `{"items":[],"next":"","marker":"` + split + ` empty"}`
			}
			fixtures = append(fixtures, empty)
			if split == "train" {
				s.Training = fixtures
			} else {
				s.Holdout = fixtures
			}
		}
		scenarios = append(scenarios, s)
	}
	return scenarios
}

func evaluationDigest(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic("invalid host evaluation manifest")
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func corpusDigest() string {
	return evaluationDigest(struct {
		Version     string
		GoalContext string
		DelayMS     int
		Scenarios   []evaluationScenario
	}{evaluationCorpusVersion, evaluationGoalContext, evaluationRenderDelayMS, evaluationCorpus()})
}

func evaluationFixtures(base string, s evaluationScenario, fs []evaluationFixture) []crawlrule.Fixture {
	out := make([]crawlrule.Fixture, 0, len(fs))
	for _, f := range fs {
		u, _ := url.Parse(base + f.Path)
		want := f.Expect
		if want.NextURL != "" {
			want.NextURL = base + want.NextURL
		}
		status := 200
		if s.Transport == "browser" {
			status = 0
		}
		out = append(out, crawlrule.Fixture{Name: f.Name, Rendered: s.Transport == "browser", Response: &htmlfetch.Response{URL: u, StatusCode: status, Body: []byte(f.Body)}, Expect: want})
	}
	return out
}

// Only authored training pages are served. Hidden pages are unavailable even
// if a model guesses their URL; existing Agent hidden URL/digest checks also apply.
func newEvaluationSite() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		if r.URL.Path == "/robots.txt" {
			_, _ = io.WriteString(w, "User-agent: *\nAllow: /\n")
			return
		}
		for _, s := range evaluationCorpus() {
			for _, f := range s.Training {
				if r.URL.RequestURI() != f.Path {
					continue
				}
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				body := f.Body
				if s.ID == "json-pagination" {
					w.Header().Set("Content-Type", "application/json")
				}
				if s.Transport == "browser" {
					encoded, _ := json.Marshal(body)
					body = fmt.Sprintf(`<html><body><main id="mount"></main><script>setTimeout(()=>{document.getElementById('mount').innerHTML=%s},%d)</script></body></html>`, encoded, evaluationRenderDelayMS)
				}
				_, _ = io.WriteString(w, body)
				return
			}
		}
		http.NotFound(w, r)
	}))
}

// Avoid endpoints, secrets or raw error strings in report identity. The model
// adapter separately validates the endpoint; this digest only identifies it.
func evaluationEndpoint(base string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("evaluation requires a credential-free HTTPS base URL")
	}
	return evaluationDigest(strings.TrimRight(u.String(), "/")), nil
}
