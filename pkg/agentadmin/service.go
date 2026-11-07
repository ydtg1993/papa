package agentadmin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/ydtg1993/papa/v3/pkg/crawlagent"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
	"github.com/ydtg1993/papa/v3/pkg/rulequality"
	"github.com/ydtg1993/papa/v3/pkg/rulestore"
)

type profile struct {
	view    ProfileView
	agent   *crawlagent.Agent
	rules   rulequality.ReanalysisRules
	archive string
	policy  htmlfetch.SamplePolicy
	goal    func(context.Context, rulequality.Report) (crawlagent.Goal, error)
}
type job struct {
	cancel   context.CancelFunc
	operator string
}
type Service struct {
	cfg        Config
	profiles   map[string]profile
	ctx        context.Context
	cancel     context.CancelFunc
	mu         sync.Mutex
	jobs       map[string]job
	recoveries map[string]context.CancelFunc
	recent     map[string]string
	order      []string
	closing    bool
	done       chan struct{}
}

func code(s string, max int) bool {
	if s == "" || len(s) > max {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_.:", r)) {
			return false
		}
	}
	return true
}
func textValue(s string, max int) bool {
	return s != "" && len(s) <= max && strings.TrimSpace(s) == s && utf8.ValidString(s) && !strings.ContainsAny(s, "\x00\r\n")
}
func revision(s string) (uint64, error) {
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil || v == 0 || strconv.FormatUint(v, 10) != s {
		return 0, ErrInvalid
	}
	return v, nil
}
func source(p profile) string { return "oa-agent:" + p.view.ID + ":" + p.view.Revision }
func New(cfg Config) (*Service, error) {
	if cfg.Audit == nil {
		return nil, ErrInvalid
	}
	if cfg.MaxConcurrent == 0 {
		cfg.MaxConcurrent = 1
	}
	if cfg.MaxConcurrent < 1 || cfg.MaxConcurrent > 4 || len(cfg.Profiles) > 32 {
		return nil, ErrInvalid
	}
	s := &Service{cfg: cfg, profiles: map[string]profile{}, jobs: map[string]job{}, recoveries: map[string]context.CancelFunc{}, recent: map[string]string{}, done: make(chan struct{})}
	for _, in := range cfg.Profiles {
		if cfg.Quality == nil || cfg.Rules == nil || !code(in.ID, 64) || !code(in.Revision, 64) || strings.Contains(in.ID+in.Revision, ":") || !textValue(in.Label, 128) || in.Goal == nil || in.ArchiveDir == "" {
			return nil, ErrInvalid
		}
		if _, ok := s.profiles[in.ID]; ok {
			return nil, ErrInvalid
		}
		rules, ok := in.Agent.Repository.(rulequality.ReanalysisRules)
		if !ok {
			return nil, ErrInvalid
		}
		in.Agent.Audit = cfg.Audit
		in.Agent.Actor = rulestore.Actor{Operator: "profile-validation", Source: "oa-agent"}
		a, err := crawlagent.New(in.Agent)
		if err != nil {
			return nil, err
		}
		policy := in.Agent.SamplePolicy
		policy.Headers = append([]string(nil), policy.Headers...)
		policy.QueryKeys = append([]string(nil), policy.QueryKeys...)
		s.profiles[in.ID] = profile{ProfileView{in.ID, in.Revision, in.Label}, a, rules, in.ArchiveDir, policy, in.Goal}
	}
	s.cfg.Profiles = nil // private frozen profiles own capabilities
	s.ctx, s.cancel = context.WithCancel(context.Background())
	return s, nil
}
func (s *Service) Capabilities() Capabilities {
	c := Capabilities{Profiles: []ProfileView{}, MaxConcurrent: s.cfg.MaxConcurrent, Quality: s.cfg.Quality != nil, Rules: s.cfg.Rules != nil, Reconcile: s.cfg.WorkerStopped != nil}
	for _, p := range s.profiles {
		c.Profiles = append(c.Profiles, p.view)
	}
	sort.Slice(c.Profiles, func(i, j int) bool { return c.Profiles[i].ID < c.Profiles[j].ID })
	return c
}
func (s *Service) build(ctx context.Context, sel Selection, operator string) (preview Preview, bridge *rulequality.Reanalyzer, input rulequality.ReanalysisInput, err error) {
	defer func() {
		if recover() != nil {
			preview = Preview{}
			bridge = nil
			input = rulequality.ReanalysisInput{}
			err = ErrInvalid
		}
	}()
	if ctx == nil || !textValue(operator, 128) || !code(sel.ReportID, 128) || !code(sel.ProfileID, 64) || len(sel.EvidenceIDs) < 1 || len(sel.EvidenceIDs) > 8 || s.cfg.Quality == nil {
		return preview, nil, input, ErrInvalid
	}
	p, ok := s.profiles[sel.ProfileID]
	if !ok {
		return preview, nil, input, ErrDisabled
	}
	report, err := s.cfg.Quality.Report(ctx, sel.ReportID)
	if err != nil {
		return preview, nil, input, err
	}
	actor := rulestore.Actor{Operator: operator, Source: source(p)}
	a, err := p.agent.ForActor(actor)
	if err != nil {
		return preview, nil, input, err
	}
	goal, err := p.goal(ctx, report)
	if err != nil {
		return preview, nil, input, ErrInvalid
	}
	plan, err := a.Plan(goal)
	if err != nil {
		return preview, nil, input, err
	}
	bridge, err = rulequality.NewReanalyzer(rulequality.ReanalysisConfig{Store: s.cfg.Quality, Agent: a, Audit: s.cfg.Audit, Rules: p.rules, ArchiveDir: p.archive, SamplePolicy: p.policy, Actor: actor})
	if err != nil {
		return preview, nil, input, err
	}
	input, _, err = bridge.Preview(ctx, sel.ReportID, rulequality.ReanalysisInput{Goal: goal, EvidenceIDs: sel.EvidenceIDs})
	if err != nil {
		return preview, nil, input, err
	}
	sel.EvidenceIDs = append([]string(nil), input.EvidenceIDs...)
	raw, err := json.Marshal(struct {
		Selection Selection
		Profile   ProfileView
		Plan      crawlagent.Plan
		Input     rulequality.ReanalysisInput
		ReportSHA string
	}{sel, p.view, plan, input, report.InputSHA256})
	if err != nil || len(raw) > 256<<10 {
		return preview, nil, input, ErrInvalid
	}
	sum := sha256.Sum256(raw)
	// Seeds have already passed the disclosure policy. Context stays server-side.
	plan.Goal.Context = ""
	plan.Goal.EvidenceSamples = nil
	preview = Preview{sel, p.view, hex.EncodeToString(sum[:]), plan}
	return preview, bridge, input, nil
}
func (s *Service) Preview(ctx context.Context, sel Selection, operator string) (Preview, error) {
	p, _, _, err := s.build(ctx, sel, operator)
	return p, err
}
func (s *Service) Prepare(ctx context.Context, in PrepareInput, operator string) (RequestView, error) {
	if !code(in.OperationID, 128) {
		return RequestView{}, ErrInvalid
	}
	p, b, _, err := s.build(ctx, in.Selection, operator)
	if err != nil {
		return RequestView{}, err
	}
	if p.Digest != in.Digest {
		return RequestView{}, ErrReviewChanged
	}
	r, err := b.Prepare(ctx, in.ReportID, in.OperationID)
	if err != nil {
		return RequestView{}, err
	}
	return s.requestView(r), nil
}
func (s *Service) requestView(r rulequality.Request) RequestView {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, active := s.jobs[r.ID]
	return RequestView{r.ID, r.OperationID, r.ReportID, r.RunID, r.Operator, r.Source, r.Status, strconv.FormatUint(r.Revision, 10), active, s.recent[r.ID], r.CreatedAt, r.UpdatedAt}
}
func (s *Service) Request(ctx context.Context, id string) (RequestView, error) {
	if s.cfg.Quality == nil {
		return RequestView{}, ErrDisabled
	}
	r, err := s.cfg.Quality.Request(ctx, id)
	if err != nil {
		return RequestView{}, err
	}
	return s.requestView(r), nil
}
func (s *Service) Operation(ctx context.Context, id string) (RequestView, error) {
	if s.cfg.Quality == nil {
		return RequestView{}, ErrDisabled
	}
	r, err := s.cfg.Quality.RequestByOperation(ctx, id)
	if err != nil {
		return RequestView{}, err
	}
	return s.requestView(r), nil
}
func (s *Service) Requests(ctx context.Context, before, reportID string) (RequestPage, error) {
	out := RequestPage{Items: []RequestView{}}
	if s.cfg.Quality == nil {
		return out, ErrDisabled
	}
	p, err := s.cfg.Quality.Requests(ctx, before, reportID)
	if err != nil {
		return out, err
	}
	out.Next = p.Next
	for _, r := range p.Items {
		out.Items = append(out.Items, s.requestView(r))
	}
	return out, nil
}
func (s *Service) String() string {
	return fmt.Sprintf("agentadmin.Service{profiles:%d}", len(s.profiles))
}

// Close cancels only this service's workers and waits for their audit cleanup.
// On timeout the host must keep DB/blob dependencies alive until process exit.
func (s *Service) Close(ctx context.Context) error {
	s.mu.Lock()
	if !s.closing {
		s.closing = true
		s.cancel()
		for _, j := range s.jobs {
			j.cancel()
		}
		for _, cancel := range s.recoveries {
			cancel()
		}
		if len(s.jobs) == 0 && len(s.recoveries) == 0 {
			close(s.done)
		}
	}
	s.mu.Unlock()
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
