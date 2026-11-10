package onboarding

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"maps"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

type Service struct {
	mu           sync.Mutex
	opts         Options
	providers    map[string]Provider
	processes    map[string]*process
	owned        map[account.Key]bool
	active       map[account.Key]bool // held until all secret/browser cleanup completes
	slots        chan struct{}
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	closed       bool
	eventMu      sync.RWMutex
	eventHandler func(account.HealthEvent)
	events       chan account.HealthEvent
}
type process struct {
	mu              sync.Mutex
	view            Progress
	owner           string
	profile         Profile
	method          Method
	inputs          map[string]string
	remember        bool
	version         uint64
	registered      bool
	ctx             context.Context
	cancel          context.CancelFunc
	releaseIdentity func()
	commands        chan command
	flow            Flow
	remote          *Remote
	candidateRef    string
	inputsRef       string
	ended           time.Time
}
type command struct {
	action string
	input  map[string]string
}

func New(opts Options) (*Service, error) {
	if opts.Manager == nil || opts.Profiles == nil || opts.Secrets == nil {
		return nil, ErrNotConfigured
	}
	if opts.MaxActive == 0 {
		opts.MaxActive = 4
	}
	if opts.MaxRetained == 0 {
		opts.MaxRetained = 128
	}
	if opts.TTL == 0 {
		opts.TTL = 10 * time.Minute
	}
	if opts.StepTimeout == 0 {
		opts.StepTimeout = 30 * time.Second
	}
	if opts.Retention == 0 {
		opts.Retention = 15 * time.Minute
	}
	if opts.MaxActive < 1 || opts.MaxActive > 64 || opts.MaxRetained < opts.MaxActive || opts.MaxRetained > 4096 || opts.TTL < time.Second || opts.TTL > time.Hour || opts.StepTimeout < time.Millisecond || opts.StepTimeout > opts.TTL || opts.Retention < time.Second {
		return nil, errors.New("invalid account onboarding budget")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{opts: opts, providers: map[string]Provider{}, processes: map[string]*process{}, owned: map[account.Key]bool{}, active: map[account.Key]bool{}, slots: make(chan struct{}, opts.MaxActive), ctx: ctx, cancel: cancel, events: make(chan account.HealthEvent, 64)}
	s.wg.Add(1)
	go s.deliverEvents()
	return s, nil
}

// SetEventListener receives sanitized terminal outcomes on one worker with a
// 64-event queue. Slow callbacks cannot block a login; overflowing notifications
// are dropped. Callbacks must return for fully drained shutdown.
func (s *Service) SetEventListener(fn func(account.HealthEvent)) {
	s.eventMu.Lock()
	s.eventHandler = fn
	s.eventMu.Unlock()
}
func (s *Service) deliverEvents() {
	defer s.wg.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		case event := <-s.events:
			s.eventMu.RLock()
			fn := s.eventHandler
			s.eventMu.RUnlock()
			if fn != nil {
				func() { defer func() { _ = recover() }(); fn(event) }()
			}
		}
	}
}
func (s *Service) emit(key account.Key, kind, reason string) {
	event := account.HealthEvent{Key: key, Kind: kind, Reason: reason, At: time.Now().UTC()}
	select {
	case s.events <- event:
	default:
	}
}

func (s *Service) Manager() *account.Manager { return s.opts.Manager }

// WrapContext composes application request governance with the caller's context
// factory. It affects future processes; existing login scopes remain frozen.
func (s *Service) WrapContext(wrapper func(context.Context, string) context.Context) {
	if wrapper == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.opts.Context
	s.opts.Context = func(ctx context.Context, site string) context.Context {
		if previous != nil {
			ctx = previous(ctx, site)
		}
		return wrapper(ctx, site)
	}
}
func (s *Service) RegisterProvider(p Provider) error {
	if p == nil {
		return ErrNotConfigured
	}
	d := p.Descriptor()
	if !validText(d.Site, 256) || !validText(d.Label, 256) || len(d.Methods) == 0 || len(d.Methods) > 16 {
		return errors.New("invalid login provider declaration")
	}
	seen := map[string]bool{}
	for _, m := range d.Methods {
		if !validText(m.ID, 64) || !validText(m.Label, 256) || seen[m.ID] {
			return errors.New("invalid login method")
		}
		seen[m.ID] = true
		switch m.Kind {
		case "password", "input", "qr", "import", "remote":
		default:
			return errors.New("unsupported login method kind")
		}
		if err := validateFields(m.Fields); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrGone
	}
	if _, ok := s.providers[d.Site]; ok {
		return ErrConflict
	}
	s.providers[d.Site] = p
	return nil
}
func validText(v string, n int) bool {
	return strings.TrimSpace(v) != "" && len(v) <= n && !strings.ContainsAny(v, "\r\n\x00")
}
func validateFields(fs []Field) error {
	if len(fs) > 32 {
		return errors.New("too many login fields")
	}
	seen := map[string]bool{}
	for _, f := range fs {
		if !validText(f.Name, 64) || !validText(f.Label, 256) || seen[f.Name] {
			return errors.New("invalid login field")
		}
		seen[f.Name] = true
	}
	return nil
}
func validateInput(fs []Field, in map[string]string) error {
	known := map[string]Field{}
	for _, f := range fs {
		known[f.Name] = f
		if f.Required && in[f.Name] == "" {
			return errors.New("required login input missing")
		}
	}
	total := 0
	for k, v := range in {
		if _, ok := known[k]; !ok {
			return errors.New("undeclared login input")
		}
		total += len(k) + len(v)
	}
	if total > 1<<20 {
		return errors.New("login input too large")
	}
	return nil
}
func (s *Service) Providers() []Descriptor {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Descriptor, 0, len(s.providers))
	for _, p := range s.providers {
		d := p.Descriptor()
		d.Methods = append([]Method(nil), d.Methods...)
		for i := range d.Methods {
			d.Methods[i].Fields = append([]Field(nil), d.Methods[i].Fields...)
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Site < out[j].Site })
	return out
}
func (s *Service) Profiles(ctx context.Context) ([]Profile, error) { return s.opts.Profiles.List(ctx) }
func (s *Service) provider(site string) (Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrGone
	}
	p := s.providers[site]
	if p == nil {
		return nil, ErrNotConfigured
	}
	return p, nil
}

// SaveDraft never registers the account with Manager, so pools cannot use it.
func (s *Service) SaveDraft(ctx context.Context, p Profile, expected uint64) (Profile, error) {
	if _, err := s.provider(p.Key.Site); err != nil {
		return p, err
	}
	if !validText(p.Key.ID, 256) || !validText(p.Label, 256) || len(p.ExpectedSubject) > 256 {
		return p, errors.New("invalid account profile")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, err := s.opts.Profiles.Get(ctx, p.Key)
	if expected == 0 {
		if !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				return p, ErrConflict
			}
			return p, err
		}
		if _, e := s.opts.Manager.Version(p.Key); !errors.Is(e, account.ErrUnknown) {
			return p, ErrConflict
		}
		p.State = "draft"
		p.Identity = nil
		p.SecretRef = ""
		p.InputsRef = ""
		p.VerifiedAt = time.Time{}
	} else {
		if err != nil {
			return p, err
		}
		if old.Revision != expected {
			return p, ErrConflict
		}
		if old.State == "ready" && p.ExpectedSubject != old.ExpectedSubject {
			return p, ErrIdentity
		}
		p.State, p.Identity, p.SecretRef, p.InputsRef, p.VerifiedAt = old.State, old.Identity, old.SecretRef, old.InputsRef, old.VerifiedAt
	}
	p.Source = "oa"
	p.Revision = expected + 1
	p.UpdatedAt = time.Now().UTC()
	err = s.opts.Profiles.SaveCAS(ctx, p, expected)
	return p, err
}

// Load registers only previously verified profiles. It performs no DDL and no
// login. Missing providers, missing ciphertext or code-registered key conflicts
// fail startup rather than silently replacing a different account source.
func (s *Service) Load(ctx context.Context) error {
	s.mu.Lock()
	started := len(s.processes) > 0
	s.mu.Unlock()
	if started {
		return errors.New("account onboarding load must precede login processes")
	}
	profiles, err := s.opts.Profiles.List(ctx)
	if err != nil {
		return err
	}
	for _, p := range profiles {
		if p.SecretRef == stagingRef(p.Key, "candidate") || p.InputsRef == stagingRef(p.Key, "inputs") {
			return errors.New("durable account profile references temporary staging")
		}
		// At most one process exists per account. Known staging keys make
		// interrupted encrypted candidates reclaimable after a process restart.
		for _, kind := range []string{"candidate", "inputs"} {
			if err = s.opts.Secrets.Delete(ctx, secretKey(p.Key, stagingRef(p.Key, kind))); err != nil && !errors.Is(err, os.ErrNotExist) {
				return &account.Failure{Code: "onboarding-staging-cleanup-failed"}
			}
		}
		if p.State != "ready" {
			continue
		}
		if p.Source != "oa" || p.SecretRef == "" || p.Identity == nil {
			return errors.New("invalid verified account profile")
		}
		provider, err := s.provider(p.Key.Site)
		if err != nil {
			return err
		}
		s.mu.Lock()
		owned := s.owned[p.Key]
		s.mu.Unlock()
		if owned {
			continue
		}
		if _, err = s.opts.Manager.Version(p.Key); !errors.Is(err, account.ErrUnknown) {
			return ErrConflict
		}
		if _, err = s.opts.Secrets.Load(ctx, secretKey(p.Key, p.SecretRef)); err != nil {
			return &account.Failure{Code: "onboarding-state-load-failed"}
		}
		cfg, err := s.runtimeConfig(ctx, provider, p)
		if err != nil {
			return err
		}
		if err = s.opts.Manager.Register(p.Key, cfg); err != nil {
			return err
		}
		s.mu.Lock()
		s.owned[p.Key] = true
		s.mu.Unlock()
	}
	return nil
}
func (s *Service) Close() {
	_ = s.Shutdown(context.Background())
}

// Shutdown cancels every temporary workflow and waits within the caller's
// drain budget. A provider that ignores cancellation cannot hold app shutdown.
func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.cancel()
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) Start(ctx context.Context, owner string, key account.Key, revision uint64, method string, input map[string]string, remember bool) (Progress, error) {
	if !validText(owner, 256) {
		return Progress{}, ErrDenied
	}
	provider, err := s.provider(key.Site)
	if err != nil {
		return Progress{}, err
	}
	var selected *Method
	for _, m := range provider.Descriptor().Methods {
		if m.ID == method {
			v := m
			selected = &v
			break
		}
	}
	if selected == nil {
		return Progress{}, errors.New("login method not declared by site")
	}
	if err = validateInput(selected.Fields, input); err != nil {
		return Progress{}, err
	}
	profile, err := s.opts.Profiles.Get(ctx, key)
	if err != nil {
		return Progress{}, err
	}
	if profile.Revision != revision {
		return Progress{}, ErrConflict
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Progress{}, ErrGone
	}
	if s.active[key] {
		return Progress{}, ErrBusy
	}
	now := time.Now()
	for id, p := range s.processes {
		p.mu.Lock()
		active := p.ended.IsZero()
		expired := !active && now.Sub(p.ended) > s.opts.Retention
		p.mu.Unlock()
		if expired {
			delete(s.processes, id)
		} else if active && p.profile.Key == key {
			return Progress{}, ErrBusy
		}
	}
	if len(s.processes) >= s.opts.MaxRetained {
		oldestID := ""
		var oldest time.Time
		for id, p := range s.processes {
			p.mu.Lock()
			ended := p.ended
			p.mu.Unlock()
			if !ended.IsZero() && (oldestID == "" || ended.Before(oldest)) {
				oldestID, oldest = id, ended
			}
		}
		if oldestID == "" {
			return Progress{}, ErrBusy
		}
		delete(s.processes, oldestID)
	}
	version, e := s.opts.Manager.Version(key)
	registered := e == nil
	if registered && !s.owned[key] {
		return Progress{}, ErrConflict
	}
	if e != nil && !errors.Is(e, account.ErrUnknown) {
		return Progress{}, e
	}
	select {
	case s.slots <- struct{}{}:
	default:
		return Progress{}, ErrBusy
	}
	id, err := randomID()
	if err != nil {
		<-s.slots
		return Progress{}, err
	}
	run, cancel := context.WithTimeout(s.ctx, s.opts.TTL)
	if s.opts.Context != nil {
		run = s.opts.Context(run, key.Site)
	}
	run, releaseIdentity, err := s.opts.Manager.PinIdentity(run, key)
	if err != nil {
		cancel()
		<-s.slots
		return Progress{}, &account.Failure{Code: "login-identity-unavailable"}
	}
	p := &process{view: Progress{ID: id, Key: key, Revision: 1, State: "running", ExpiresAt: now.Add(s.opts.TTL)}, owner: owner, profile: profile, method: *selected, inputs: maps.Clone(input), remember: remember, version: version, registered: registered, ctx: run, cancel: cancel, releaseIdentity: releaseIdentity, commands: make(chan command, 1)}
	s.processes[id] = p
	s.active[key] = true
	s.wg.Add(1)
	go s.run(p, provider)
	return p.view, nil
}
func (s *Service) lookup(owner, id string) (*process, error) {
	if owner == "" {
		return nil, ErrDenied
	}
	s.mu.Lock()
	p := s.processes[id]
	s.mu.Unlock()
	if p == nil {
		return nil, ErrGone
	}
	if p.owner != owner {
		return nil, ErrDenied
	}
	return p, nil
}
func copyProgress(v Progress) Progress {
	v.Fields = append([]Field(nil), v.Fields...)
	if v.Identity != nil {
		i := *v.Identity
		i.Scopes = append([]string(nil), i.Scopes...)
		v.Identity = &i
	}
	return v
}
func (s *Service) Get(owner, id string) (Progress, error) {
	p, err := s.lookup(owner, id)
	if err != nil {
		return Progress{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return copyProgress(p.view), nil
}
func (s *Service) Act(owner, id string, revision uint64, action string, input map[string]string) (Progress, error) {
	p, err := s.lookup(owner, id)
	if err != nil {
		return Progress{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if action == "cancel" {
		if p.ended.IsZero() {
			p.view.State = "cancelled"
			p.view.Revision++
			p.view.Fields = nil
			p.view.ImageDataURL = ""
			p.view.Remote = false
			p.ended = time.Now()
			p.cancel()
		}
		return copyProgress(p.view), nil
	}
	if !p.ended.IsZero() || p.ctx.Err() != nil {
		return copyProgress(p.view), ErrGone
	}
	if revision != p.view.Revision {
		return copyProgress(p.view), ErrConflict
	}
	switch action {
	case "continue":
		if p.view.State != "waiting_input" && p.view.State != "waiting_scan" && p.view.State != "remote" {
			return copyProgress(p.view), ErrConflict
		}
		if err = validateInput(p.view.Fields, input); err != nil {
			return copyProgress(p.view), err
		}
	case "confirm":
		if p.view.State != "confirm_identity" || len(input) > 0 {
			return copyProgress(p.view), ErrConflict
		}
	default:
		return copyProgress(p.view), errors.New("unknown login action")
	}
	p.view.State = "running"
	p.view.Fields = nil
	p.view.ImageDataURL = ""
	p.view.Revision++
	p.commands <- command{action: action, input: maps.Clone(input)}
	return copyProgress(p.view), nil
}
func (s *Service) run(p *process, provider Provider) {
	defer s.wg.Done()
	defer func() {
		defer p.releaseIdentity()
		defer func() { <-s.slots }()
		defer func() { s.mu.Lock(); delete(s.active, p.profile.Key); s.mu.Unlock() }()
		p.cancel()
		p.mu.Lock()
		flow, remote := p.flow, p.remote
		p.mu.Unlock()
		if remote != nil {
			remote.Close()
		}
		if flow != nil {
			func() { defer func() { _ = recover() }(); _ = flow.Close() }()
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if p.candidateRef != "" {
			_ = s.opts.Secrets.Delete(cleanup, secretKey(p.profile.Key, p.candidateRef))
		}
		if p.inputsRef != "" {
			_ = s.opts.Secrets.Delete(cleanup, secretKey(p.profile.Key, p.inputsRef))
		}
		p.inputs = nil
	}()
	defer func() {
		if recover() != nil {
			s.fail(p, "login_provider_failed")
		}
	}()
	// Start receives the process lifetime so browser resources can survive
	// individual Next calls. Providers bound their initial network operations.
	if p.registered {
		if err := s.opts.Manager.ReserveLogin(p.ctx, p.profile.Key); err != nil {
			s.fail(p, "login_budget_wait")
			return
		}
	}
	startTimer := time.AfterFunc(s.opts.StepTimeout, p.cancel)
	flow, err := provider.Start(p.ctx, p.profile, p.method.ID, p.inputs)
	startTimer.Stop()
	if err != nil {
		s.fail(p, "login_start_failed")
		return
	}
	p.mu.Lock()
	p.flow = flow
	p.mu.Unlock()
	if flow == nil {
		s.fail(p, "invalid_login_flow")
		return
	}
	if p.remember {
		saved := map[string]string{}
		for _, f := range p.method.Fields {
			if f.Persist && p.inputs[f.Name] != "" {
				saved[f.Name] = p.inputs[f.Name]
			}
		}
		if len(saved) > 0 {
			p.inputsRef, err = s.saveStaging(p.ctx, p.profile.Key, "inputs", &account.StoredState{HTTP: &account.CredentialState{Values: saved}})
			if err != nil {
				s.fail(p, "secret_save_failed")
				return
			}
		}
	}
	p.inputs = nil
	if !s.advance(p, provider, nil) {
		return
	}
	for {
		p.mu.Lock()
		var remoteDone <-chan struct{}
		if p.remote != nil {
			remoteDone = p.remote.Done()
		}
		p.mu.Unlock()
		select {
		case <-remoteDone:
			s.fail(p, "remote_disconnected")
			return
		case <-p.ctx.Done():
			s.fail(p, "login_expired")
			return
		case c := <-p.commands:
			if c.action == "confirm" {
				s.publish(p, provider)
				return
			}
			if !s.advance(p, provider, c.input) {
				return
			}
		}
	}
}
func (s *Service) advance(p *process, provider Provider, input map[string]string) bool {
	ctx, cancel := context.WithTimeout(p.ctx, s.opts.StepTimeout)
	defer cancel()
	step, err := p.flow.Next(ctx, input)
	if err != nil {
		s.fail(p, "login_step_failed")
		return false
	}
	if err = validateFields(step.Fields); err != nil {
		s.fail(p, "invalid_login_progress")
		return false
	}
	if step.ImageDataURL != "" && (!strings.HasPrefix(step.ImageDataURL, "data:image/png;base64,") || len(step.ImageDataURL) > 2<<20) {
		s.fail(p, "invalid_login_image")
		return false
	}
	var identity *Identity
	if step.Candidate != nil {
		i, err := provider.Verify(ctx, p.profile, step.Candidate)
		if err != nil || !validText(i.Subject, 256) || !validText(i.Display, 256) || (!i.HTTP && !i.Browser) {
			s.fail(p, "identity_verification_failed")
			return false
		}
		expected := p.profile.ExpectedSubject
		if p.profile.Identity != nil {
			expected = p.profile.Identity.Subject
		}
		if expected != "" && i.Subject != expected {
			s.fail(p, "identity_mismatch")
			return false
		}
		identity = &i
		state, err := credentialState(step.Candidate)
		if err != nil {
			s.fail(p, "invalid_login_candidate")
			return false
		}
		p.candidateRef, err = s.saveStaging(ctx, p.profile.Key, "candidate", state)
		if err != nil {
			s.fail(p, "candidate_save_failed")
			return false
		}
		step.State = "confirm_identity"
		p.mu.Lock()
		remote := p.remote
		p.remote = nil
		p.mu.Unlock()
		if remote != nil {
			remote.Close()
		}
	} else {
		switch step.State {
		case "waiting_input", "waiting_scan", "remote":
		default:
			s.fail(p, "invalid_login_progress")
			return false
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx.Err() != nil || !p.ended.IsZero() {
		return false
	}
	if step.Remote != nil {
		if p.remote != nil && p.remote != step.Remote {
			step.Remote.Close()
			return false
		}
		p.remote = step.Remote
	}
	if step.State == "remote" && p.remote == nil {
		p.view.State = "failed"
		p.view.Failure = "remote_not_attached"
		p.ended = time.Now()
		p.cancel()
		return false
	}
	p.view.State = step.State
	p.view.Revision++
	p.view.Fields = append([]Field(nil), step.Fields...)
	p.view.ImageDataURL = step.ImageDataURL
	p.view.Identity = identity
	p.view.Remote = p.remote != nil
	return true
}
func (s *Service) fail(p *process, code string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.ended.IsZero() {
		return
	}
	p.view.State = "failed"
	if p.ctx.Err() != nil {
		p.view.State = "expired"
		code = "login_expired"
	}
	p.view.Failure = code
	p.view.Revision++
	p.view.Fields = nil
	p.view.ImageDataURL = ""
	p.ended = time.Now()
	p.cancel()
	s.emit(p.profile.Key, "login-failed", code)
}
func (s *Service) publish(p *process, provider Provider) {
	ctx, cancel := context.WithTimeout(p.ctx, s.opts.StepTimeout)
	defer cancel()
	state, err := s.opts.Secrets.Load(ctx, secretKey(p.profile.Key, p.candidateRef))
	if err != nil {
		s.fail(p, "candidate_load_failed")
		return
	}
	candidate, err := restoreCredentials(state)
	if err != nil {
		s.fail(p, "candidate_load_failed")
		return
	}
	p.mu.Lock()
	identity := p.view.Identity
	p.mu.Unlock()
	target := p.profile
	target.Identity = identity
	target.VerifiedAt = time.Now().UTC()
	target.State = "ready"
	target.InputsRef = ""
	if p.inputsRef != "" {
		inputs, loadErr := s.opts.Secrets.Load(ctx, secretKey(p.profile.Key, p.inputsRef))
		if loadErr != nil {
			s.fail(p, "saved_inputs_load_failed")
			return
		}
		target.InputsRef, err = s.saveSecret(ctx, p.profile.Key, inputs)
		if err != nil {
			s.fail(p, "saved_inputs_save_failed")
			return
		}
	}
	ctx = context.WithValue(ctx, publicationKey{}, target)
	if p.registered {
		_, err = s.opts.Manager.PublishCandidate(ctx, p.profile.Key, p.version, candidate)
	} else {
		var cfg account.Config
		cfg, err = s.runtimeConfig(ctx, provider, target)
		if err == nil {
			err = s.opts.Manager.RegisterCandidate(ctx, p.profile.Key, cfg, candidate)
		}
	}
	if err != nil {
		var failure *account.Failure
		if target.InputsRef != "" && !(errors.As(err, &failure) && failure.Code == "state-save-failed") {
			_ = s.opts.Secrets.Delete(context.WithoutCancel(ctx), secretKey(p.profile.Key, target.InputsRef))
		}
		s.fail(p, "candidate_publish_failed")
		return
	}
	s.mu.Lock()
	s.owned[p.profile.Key] = true
	s.mu.Unlock()
	healthWarning := ""
	if pools := s.opts.Manager.Pools(); pools != nil {
		version, versionErr := s.opts.Manager.Version(p.profile.Key)
		if versionErr != nil || pools.CandidatePublished(ctx, p.profile.Key, version) != nil {
			healthWarning = "health_restore_pending"
		}
	}
	p.mu.Lock()
	p.view.State = "succeeded"
	p.view.Failure = healthWarning
	p.view.Revision++
	p.view.Fields = nil
	p.view.ImageDataURL = ""
	p.ended = time.Now()
	p.mu.Unlock()
	s.emit(p.profile.Key, "recovered", "oa-login-verified")
}
func randomID() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
func secretKey(key account.Key, ref string) account.Key {
	return account.Key{Site: "onboarding:" + key.Site, ID: key.ID + "\x00" + ref}
}
func stagingRef(key account.Key, kind string) string {
	sum := sha256.Sum256([]byte(key.Site + "\x00" + key.ID))
	return kind + "-" + hex.EncodeToString(sum[:])
}
func (s *Service) saveStaging(ctx context.Context, key account.Key, kind string, state *account.StoredState) (string, error) {
	ref := stagingRef(key, kind)
	state.Version = 1
	state.Key = secretKey(key, ref)
	if err := s.opts.Secrets.Save(ctx, state.Key, state); err != nil {
		return "", err
	}
	return ref, nil
}
func (s *Service) saveSecret(ctx context.Context, key account.Key, state *account.StoredState) (string, error) {
	ref, err := randomID()
	if err != nil {
		return "", err
	}
	state.Version = 1
	state.Key = secretKey(key, ref)
	if err = s.opts.Secrets.Save(ctx, state.Key, state); err != nil {
		return "", err
	}
	return ref, nil
}
func credentialState(c *account.Credentials) (*account.StoredState, error) {
	if c == nil || c.Session == nil {
		return nil, errors.New("candidate session required")
	}
	return &account.StoredState{HTTP: &account.CredentialState{Cookies: c.Session.Snapshot(), Headers: maps.Clone(c.Headers), Values: maps.Clone(c.Values)}, Browser: c.BrowserState}, nil
}
func restoreCredentials(state *account.StoredState) (*account.Credentials, error) {
	if state == nil || state.HTTP == nil {
		return nil, errors.New("candidate credentials missing")
	}
	session, err := htmlfetch.RestoreSession(state.HTTP.Cookies)
	if err != nil {
		return nil, err
	}
	return &account.Credentials{Session: session, Headers: maps.Clone(state.HTTP.Headers), Values: maps.Clone(state.HTTP.Values), BrowserState: state.Browser}, nil
}

// publicationStore makes the durable profile pointer the commit point. A
// failed write cannot replace old ciphertext; Manager publishes in memory only
// after this Store.Save succeeds. Later refreshes use the same CAS protocol.
type publicationKey struct{}
type publicationStore struct {
	s   *Service
	key account.Key
}

func (s *Service) runtimeConfig(ctx context.Context, provider Provider, p Profile) (account.Config, error) {
	read := func(ctx context.Context) (map[string]string, error) {
		latest, err := s.opts.Profiles.Get(ctx, p.Key)
		if err != nil {
			return nil, err
		}
		if latest.InputsRef == "" {
			return nil, nil
		}
		state, err := s.opts.Secrets.Load(ctx, secretKey(p.Key, latest.InputsRef))
		if err != nil {
			return nil, err
		}
		if state.HTTP == nil {
			return nil, errors.New("saved inputs missing")
		}
		return maps.Clone(state.HTTP.Values), nil
	}
	cfg, err := provider.Config(ctx, p, read)
	cfg.Store = &publicationStore{s: s, key: p.Key}
	return cfg, err
}
func (f *publicationStore) Load(ctx context.Context, key account.Key) (*account.StoredState, error) {
	if key != f.key {
		return nil, ErrIdentity
	}
	p, err := f.s.opts.Profiles.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	if p.SecretRef == "" {
		return nil, os.ErrNotExist
	}
	state, err := f.s.opts.Secrets.Load(ctx, secretKey(key, p.SecretRef))
	if err == nil {
		state.Key = key
	}
	return state, err
}
func (f *publicationStore) Save(ctx context.Context, key account.Key, state *account.StoredState) error {
	if key != f.key || state == nil {
		return ErrIdentity
	}
	p, err := f.s.opts.Profiles.Get(ctx, key)
	if err != nil {
		return err
	}
	expected := p.Revision
	oldInputs := p.InputsRef
	if target, ok := ctx.Value(publicationKey{}).(Profile); ok {
		if target.Key != key || target.Revision != expected {
			return ErrConflict
		}
		p = target
	}
	copy := *state
	ref, err := f.s.saveSecret(ctx, key, &copy)
	if err != nil {
		return err
	}
	old := p.SecretRef
	p.SecretRef = ref
	p.Revision = expected + 1
	p.UpdatedAt = time.Now().UTC()
	if err = f.s.opts.Profiles.SaveCAS(ctx, p, expected); err != nil {
		// A database connection can fail after committing its CAS. Preserve the
		// encrypted generation on uncertain errors so a committed pointer never
		// references a file we just removed. Confirmed conflicts are safe to reap.
		if errors.Is(err, ErrConflict) {
			_ = f.s.opts.Secrets.Delete(context.WithoutCancel(ctx), secretKey(key, ref))
		}
		return err
	}
	if old != "" {
		_ = f.s.opts.Secrets.Delete(context.WithoutCancel(ctx), secretKey(key, old))
	}
	if oldInputs != "" && oldInputs != p.InputsRef {
		_ = f.s.opts.Secrets.Delete(context.WithoutCancel(ctx), secretKey(key, oldInputs))
	}
	return nil
}
func (f *publicationStore) Delete(ctx context.Context, key account.Key) error {
	if key != f.key {
		return ErrIdentity
	}
	p, err := f.s.opts.Profiles.Get(ctx, key)
	if err != nil {
		return err
	}
	ref := p.SecretRef
	p.SecretRef = ""
	p.State = "draft"
	p.Identity = nil
	p.Revision++
	p.UpdatedAt = time.Now().UTC()
	if err = f.s.opts.Profiles.SaveCAS(ctx, p, p.Revision-1); err != nil {
		return err
	}
	if ref != "" {
		return f.s.opts.Secrets.Delete(ctx, secretKey(key, ref))
	}
	return nil
}
