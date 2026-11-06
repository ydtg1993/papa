package crawlagent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ydtg1993/papa/v3/pkg/agentstore"
	"github.com/ydtg1993/papa/v3/pkg/crawlagent"
	"github.com/ydtg1993/papa/v3/pkg/crawlagent/model"
	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
	"github.com/ydtg1993/papa/v3/pkg/rulestore"
	"github.com/ydtg1993/papa/v3/pkg/webaccess"
)

// runnerRepository only supplies persistence for the unit harness. Sanitizing,
// compiling, content-addressed blobs and fixture evaluation use production APIs.
type runnerRepository struct {
	mu            sync.Mutex
	blobs         *rulestore.FileBlobs
	versions      map[rulestore.Reference]rulestore.Version
	samples       map[string]rulestore.Sample
	sets          map[string]rulestore.SampleSet
	verifications []rulestore.Verification
	created       []rulestore.Reference
	sequence      int
}

func newRunnerRepository(t *testing.T) *runnerRepository {
	t.Helper()
	b, err := rulestore.NewFileBlobs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return &runnerRepository{blobs: b, versions: map[rulestore.Reference]rulestore.Version{}, samples: map[string]rulestore.Sample{}, sets: map[string]rulestore.SampleSet{}}
}

func (r *runnerRepository) CreateDraft(ctx context.Context, rule crawlrule.Rule, actor rulestore.Actor) (rulestore.Version, error) {
	if err := ctx.Err(); err != nil {
		return rulestore.Version{}, err
	}
	c, err := crawlrule.Compile(rule)
	if err != nil {
		return rulestore.Version{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ref := rulestore.Reference{ID: rule.ID, Version: rule.Version}
	if _, exists := r.versions[ref]; exists {
		return rulestore.Version{}, rulestore.ErrConflict
	}
	v := rulestore.Version{Reference: ref, Rule: c.Rule(), State: "draft", Actor: actor}
	r.versions[ref] = v
	r.created = append(r.created, ref)
	return v, nil
}

func (r *runnerRepository) Get(ctx context.Context, ref rulestore.Reference) (rulestore.Version, error) {
	if err := ctx.Err(); err != nil {
		return rulestore.Version{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.versions[ref]
	if !ok {
		return v, rulestore.ErrNotFound
	}
	c, err := crawlrule.Compile(v.Rule)
	if err != nil {
		return rulestore.Version{}, err
	}
	v.Rule = c.Rule()
	return v, nil
}

func (r *runnerRepository) AddSample(ctx context.Context, in rulestore.SampleInput, p htmlfetch.SamplePolicy, actor rulestore.Actor) (rulestore.Sample, error) {
	s, err := rulestore.SanitizeSample(in, p)
	if err != nil {
		return rulestore.Sample{}, err
	}
	if _, err := r.blobs.Put(ctx, s.Body); err != nil {
		return rulestore.Sample{}, err
	}
	s.Body = nil
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sequence++
	out := rulestore.Sample{ID: fmt.Sprintf("sample-%d", r.sequence), SanitizedSample: s, Actor: actor}
	r.samples[out.ID] = out
	return out, nil
}

func (r *runnerRepository) Sample(ctx context.Context, id string) (rulestore.Sample, error) {
	if err := ctx.Err(); err != nil {
		return rulestore.Sample{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.samples[id]
	if !ok {
		return s, rulestore.ErrNotFound
	}
	return s, nil
}

func (r *runnerRepository) SampleSet(ctx context.Context, id string) (rulestore.SampleSet, error) {
	if err := ctx.Err(); err != nil {
		return rulestore.SampleSet{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sets[id]
	if !ok {
		return s, rulestore.ErrNotFound
	}
	data, _ := json.Marshal(s)
	var detached rulestore.SampleSet
	_ = json.Unmarshal(data, &detached)
	return detached, nil
}

func (r *runnerRepository) Verify(ctx context.Context, ref rulestore.Reference, setID string, actor rulestore.Actor) (rulestore.Verification, error) {
	v, err := r.Get(ctx, ref)
	if err != nil {
		return rulestore.Verification{}, err
	}
	set, err := r.SampleSet(ctx, setID)
	if err != nil {
		return rulestore.Verification{}, err
	}
	c, err := crawlrule.Compile(v.Rule)
	if err != nil {
		return rulestore.Verification{}, err
	}
	fixtures := make([]crawlrule.Fixture, 0, len(set.Fixtures))
	for _, f := range set.Fixtures {
		s, err := r.Sample(ctx, f.SampleID)
		if err != nil {
			return rulestore.Verification{}, err
		}
		body, err := r.blobs.Get(ctx, s.BodySHA256, rulestore.MaxSampleBodyBytes)
		if err != nil {
			return rulestore.Verification{}, err
		}
		u, err := url.Parse(s.URL)
		if err != nil {
			return rulestore.Verification{}, err
		}
		fixtures = append(fixtures, crawlrule.Fixture{Name: f.Name, Response: &htmlfetch.Response{URL: u, StatusCode: s.StatusCode, Body: body}, Rendered: s.Rendered, Expect: f.Expect})
	}
	report, verifyErr := c.Verify(fixtures)
	r.mu.Lock()
	defer r.mu.Unlock()
	out := rulestore.Verification{ID: fmt.Sprintf("report-%d", len(r.verifications)+1), Reference: ref, SetID: setID, Passed: report.Passed, Report: report, Actor: actor}
	r.verifications = append(r.verifications, out)
	return out, verifyErr
}

var runnerPolicy = htmlfetch.SamplePolicy{RedactBody: func(_ bool, b []byte) ([]byte, error) { return b, nil }}
var runnerActor = rulestore.Actor{Operator: "test-host", Source: "deterministic runner regression"}

func addRunnerFixture(t *testing.T, r *runnerRepository, setID, fixtureURL, body, expected string) rulestore.Sample {
	t.Helper()
	s, err := r.AddSample(context.Background(), rulestore.SampleInput{URL: fixtureURL, StatusCode: 200, Body: []byte(body)}, runnerPolicy, runnerActor)
	if err != nil {
		t.Fatal(err)
	}
	r.sets[setID] = rulestore.SampleSet{ID: setID, Fixtures: []rulestore.StoredFixture{{Name: setID + "-fixture", SampleID: s.ID, Expect: crawlrule.Expectations{RowCount: 1, Fields: []crawlrule.FieldExpectation{{Row: 0, Pointer: "/title", Value: expected}}}}}}
	return s
}

type runnerSource struct {
	config        webaccess.Config
	body          string
	allowedURL    string
	lastURL       string
	calls, closed atomic.Int64
	onGet         func(context.Context)
	usage         webaccess.Usage
}

func (s *runnerSource) Get(ctx context.Context, raw string) (webaccess.Response, error) {
	if err := ctx.Err(); err != nil {
		return webaccess.Response{}, err
	}
	allowed := s.allowedURL
	if allowed == "" {
		allowed = "https://site.example/list"
	}
	if raw != allowed {
		return webaccess.Response{}, webaccess.ErrPolicy
	}
	if s.config.OnRequest != nil {
		if err := s.config.OnRequest(ctx); err != nil {
			return webaccess.Response{}, err
		}
	}
	s.calls.Add(1)
	s.lastURL = raw
	if s.onGet != nil {
		s.onGet(ctx)
	}
	if err := ctx.Err(); err != nil {
		return webaccess.Response{}, err
	}
	if s.config.OnBytes != nil {
		if err := s.config.OnBytes(ctx, int64(len(s.body))); err != nil {
			return webaccess.Response{}, err
		}
	}
	s.usage.Requests++
	s.usage.Pages++
	s.usage.Bytes += int64(len(s.body))
	return webaccess.Response{URL: raw, StatusCode: 200, ContentType: "text/html", Body: []byte(s.body)}, nil
}
func (s *runnerSource) Render(ctx context.Context, raw string, _ webaccess.RenderOptions) (webaccess.Response, []webaccess.Response, error) {
	r, err := s.Get(ctx, raw)
	r.Rendered = true
	r.StatusCode = 0
	return r, nil, err
}
func (s *runnerSource) Usage() webaccess.Usage { return s.usage }
func (s *runnerSource) Close() error           { s.closed.Add(1); return nil }

func replayActions(actions ...string) *model.Replay {
	steps := make([]model.ReplayStep, len(actions))
	for i, a := range actions {
		steps[i] = model.ReplayStep{Reply: model.Reply{Content: a, Usage: model.Usage{InputTokens: 10, OutputTokens: 5, Known: true}}}
	}
	return model.NewReplay(steps...)
}

func candidateAction(selector string) string {
	r := crawlrule.Rule{SchemaVersion: 1, Format: "html", Rows: "article", Fields: []crawlrule.Field{{Name: "title", Selector: selector, Validation: crawlrule.Validation{Kind: "string", Required: true}}}}
	return runnerJSON(map[string]any{"tool": "submit_candidate", "stage": "catalog", "rule": r})
}

func runnerJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func runnerHarness(t *testing.T, p model.Provider) (crawlagent.Config, crawlagent.Goal, *runnerRepository, *runnerSource, *agentstore.MemoryStore) {
	t.Helper()
	r := newRunnerRepository(t)
	audit := agentstore.NewMemoryStore()
	source := &runnerSource{body: "<article><b>FETCHED_VALUE</b></article>"}
	cfg := crawlagent.Config{Model: p, ModelName: "local-replay", Audit: audit, Repository: r, Blobs: r.blobs, Actor: runnerActor, SamplePolicy: runnerPolicy, Web: webaccess.Config{Policy: webaccess.Policy{Origins: []webaccess.Origin{{URL: "https://site.example", PathPrefixes: []string{"/list"}}}}}, SourceFactory: func(c webaccess.Config) (crawlagent.Source, error) { source.config = c; return source, nil }}
	goal := crawlagent.Goal{Site: "example", Seeds: []string{"https://site.example/list"}, Stages: []crawlagent.StageGoal{{Stage: "catalog", RuleID: "example-catalog", Fields: []crawlagent.FieldGoal{{Name: "title", Kind: "string", Required: true}}}}}
	return cfg, goal, r, source, audit
}

func runAgent(t *testing.T, cfg crawlagent.Config, goal crawlagent.Goal) (crawlagent.Result, error) {
	t.Helper()
	a, err := crawlagent.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return a.Run(context.Background(), goal)
}

type providerFunc func(context.Context, []model.Message, int) (model.Reply, error)

func (f providerFunc) Complete(ctx context.Context, m []model.Message, n int) (model.Reply, error) {
	return f(ctx, m, n)
}

type failingAudit struct {
	agentstore.Store
	beginAt, updateAt, endAt int
	begins, updates, ends    int
}

func (s *failingAudit) BeginStep(ctx context.Context, id string, revision uint64, step agentstore.Step, usage string) (agentstore.Run, agentstore.Step, error) {
	s.begins++
	if s.begins == s.beginAt {
		return agentstore.Run{}, agentstore.Step{}, fmt.Errorf("injected audit begin failure")
	}
	return s.Store.BeginStep(ctx, id, revision, step, usage)
}
func (s *failingAudit) UpdateUsage(ctx context.Context, id string, revision uint64, usage string) (agentstore.Run, error) {
	s.updates++
	if s.updates == s.updateAt {
		return agentstore.Run{}, fmt.Errorf("injected audit usage failure")
	}
	return s.Store.UpdateUsage(ctx, id, revision, usage)
}
func (s *failingAudit) EndStep(ctx context.Context, id string, revision uint64, step agentstore.Step, usage string) (agentstore.Run, error) {
	s.ends++
	if s.ends == s.endAt {
		return agentstore.Run{}, fmt.Errorf("injected audit end failure")
	}
	return s.Store.EndStep(ctx, id, revision, step, usage)
}
