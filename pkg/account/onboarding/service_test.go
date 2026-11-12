package onboarding

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

type fixtureProvider struct {
	steps    atomic.Int32
	closes   atomic.Int32
	block    bool
	badCheck bool
}

func (p *fixtureProvider) Descriptor() Descriptor {
	return Descriptor{Site: "fixture", Label: "Synthetic login", Methods: []Method{{ID: "password", Kind: "password", Label: "Password", Fields: []Field{{Name: "password", Label: "Password", Secret: true, Required: true, Persist: true}, {Name: "subject", Label: "Subject", Required: true}}}, {ID: "code", Kind: "input", Label: "Code", Fields: []Field{{Name: "subject", Label: "Subject", Required: true}}}, {ID: "scan", Kind: "qr", Label: "QR", Fields: []Field{{Name: "subject", Label: "Subject", Required: true}}}, {ID: "import", Kind: "import", Label: "Import", Fields: []Field{{Name: "json", Label: "JSON", Required: true, Secret: true}}}}}
}
func (p *fixtureProvider) Config(ctx context.Context, profile Profile, read func(context.Context) (map[string]string, error)) (account.Config, error) {
	return account.Config{Origins: []string{"https://fixture.invalid"}, Login: func(context.Context, *account.Credentials) error { return errors.New("interactive login required") }, Check: func(ctx context.Context, c *account.Credentials) (bool, error) {
		return c.Values["verified"] == "yes" && !p.badCheck, nil
	}}, nil
}
func (p *fixtureProvider) Start(ctx context.Context, profile Profile, method string, input map[string]string) (Flow, error) {
	if p.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	subject := input["subject"]
	if method == "import" {
		var in struct {
			Subject string `json:"subject"`
			Token   string `json:"token"`
		}
		if err := json.Unmarshal([]byte(input["json"]), &in); err != nil || in.Token == "" {
			return nil, errors.New("invalid import contains sensitive text")
		}
		subject = in.Subject
	}
	return &fixtureFlow{provider: p, method: method, subject: subject}, nil
}
func (p *fixtureProvider) Verify(ctx context.Context, profile Profile, c *account.Credentials) (Identity, error) {
	return Identity{Subject: c.Values["subject"], Display: "fixture-***", HTTP: true, Scopes: []string{"read"}}, nil
}

type fixtureFlow struct {
	provider        *fixtureProvider
	method, subject string
	step            int
}

func (f *fixtureFlow) Next(ctx context.Context, input map[string]string) (Step, error) {
	f.provider.steps.Add(1)
	f.step++
	if f.step == 1 && f.method == "code" {
		return Step{State: "waiting_input", Fields: []Field{{Name: "code", Label: "One-time code", Required: true, Secret: true}}}, nil
	}
	if f.step == 1 && f.method == "scan" {
		return Step{State: "waiting_scan", ImageDataURL: "data:image/png;base64,aGVsbG8="}, nil
	}
	if f.method == "code" && input["code"] != "123456" {
		return Step{}, errors.New("wrong sensitive OTP")
	}
	return Step{Candidate: &account.Credentials{Session: htmlfetch.NewSession(), Headers: map[string]string{"Authorization": "Bearer private-candidate-token"}, Values: map[string]string{"subject": f.subject, "verified": "yes"}}}, nil
}
func (f *fixtureFlow) Close() error { f.provider.closes.Add(1); return nil }

func newFixtureService(t *testing.T, provider *fixtureProvider, profiles Store, ttl time.Duration) (*Service, *account.Manager, string) {
	t.Helper()
	dir := t.TempDir()
	secrets, err := account.NewFileStore(dir, []byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	manager := account.NewManager(htmlfetch.NewClient(htmlfetch.Config{}))
	s, err := New(Options{Manager: manager, Profiles: profiles, Secrets: secrets, TTL: ttl, StepTimeout: time.Second, MaxActive: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RegisterProvider(provider); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, manager, dir
}
func draftFixture(t *testing.T, s *Service, id string) Profile {
	t.Helper()
	p, err := s.SaveDraft(context.Background(), Profile{Key: account.Key{Site: "fixture", ID: id}, Label: "Fixture " + id, ExpectedSubject: "alice"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func waitState(t *testing.T, s *Service, id string, states ...string) Progress {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		p, err := s.Get("operator", id)
		if err != nil {
			t.Fatal(err)
		}
		for _, state := range states {
			if p.State == state {
				if p.State == "succeeded" || p.State == "failed" || p.State == "cancelled" || p.State == "expired" {
					s.mu.Lock()
					cleaning := s.active[p.Key]
					s.mu.Unlock()
					if cleaning {
						continue
					}
				}
				return p
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	p, _ := s.Get("operator", id)
	t.Fatalf("progress did not reach %v: %+v", states, p)
	return Progress{}
}
func startFixture(t *testing.T, s *Service, p Profile, method string, input map[string]string, remember bool) Progress {
	t.Helper()
	v, err := s.Start(context.Background(), "operator", p.Key, p.Revision, method, input, remember)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func confirmFixture(t *testing.T, s *Service, id string) Progress {
	t.Helper()
	v := waitState(t, s, id, "confirm_identity")
	if _, err := s.Act("operator", id, v.Revision, "confirm", nil); err != nil {
		t.Fatal(err)
	}
	return waitState(t, s, id, "succeeded", "failed")
}

func TestStartReturnsInitialSnapshotWhenProviderFailsImmediately(t *testing.T) {
	s, _, _ := newFixtureService(t, &fixtureProvider{}, NewMemoryStore(), 5*time.Second)
	p := draftFixture(t, s, "fast-failure")
	for i := 0; i < 32; i++ {
		initial := startFixture(t, s, p, "import", map[string]string{"json": "invalid"}, false)
		if initial.State != "running" || initial.Revision != 1 || initial.Failure != "" {
			t.Fatalf("Start returned worker progress instead of its initial snapshot: %+v", initial)
		}
		failed := waitState(t, s, initial.ID, "failed")
		if failed.Revision <= initial.Revision || initial.State != "running" {
			t.Fatalf("progress snapshots are not independent: initial=%+v failed=%+v", initial, failed)
		}
	}
}

func TestLoginCandidateIdentityConfirmationEncryptionAndRestart(t *testing.T) {
	profiles := NewMemoryStore()
	s, m, dir := newFixtureService(t, &fixtureProvider{}, profiles, 5*time.Second)
	p := draftFixture(t, s, "first")
	if _, err := m.Version(p.Key); !errors.Is(err, account.ErrUnknown) {
		t.Fatalf("draft registered: %v", err)
	}
	v := startFixture(t, s, p, "password", map[string]string{"subject": "alice", "password": "private-password"}, true)
	waitState(t, s, v.ID, "confirm_identity")
	if _, err := m.Version(p.Key); !errors.Is(err, account.ErrUnknown) {
		t.Fatal("unconfirmed candidate registered")
	}
	if result := confirmFixture(t, s, v.ID); result.State != "succeeded" {
		t.Fatal(result)
	}
	p, err := profiles.Get(context.Background(), p.Key)
	if err != nil || p.State != "ready" || p.SecretRef == "" || p.InputsRef == "" {
		t.Fatalf("profile %+v %v", p, err)
	}
	for _, st := range m.Status() {
		if st.Key == p.Key && !st.HTTPReady {
			t.Fatal("verified candidate not runtime ready")
		}
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		raw, _ := os.ReadFile(filepath.Join(dir, f.Name()))
		for _, secret := range []string{"private-password", "private-candidate-token", "alice"} {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("plaintext %s", secret)
			}
		}
	}
	public, _ := json.Marshal(p)
	if strings.Contains(string(public), p.SecretRef) || strings.Contains(string(public), p.InputsRef) || strings.Contains(string(public), "private-password") {
		t.Fatal("profile leaks secret material")
	}
	s.Close()
	m2 := account.NewManager(htmlfetch.NewClient(htmlfetch.Config{}))
	s2, err := New(Options{Manager: m2, Profiles: profiles, Secrets: s.opts.Secrets})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	_ = s2.RegisterProvider(&fixtureProvider{})
	if err = s2.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = m2.Version(p.Key); err != nil {
		t.Fatal(err)
	}
	if _, err = s2.Get("operator", v.ID); !errors.Is(err, ErrGone) {
		t.Fatal("restart revived temporary process")
	}
	loaded, err := (&publicationStore{s: s2, key: p.Key}).Load(context.Background(), p.Key)
	if err != nil || loaded.HTTP.Headers["Authorization"] != "Bearer private-candidate-token" {
		t.Fatalf("encrypted restart %v", err)
	}
}

func TestStructuredInputQRImportDuplicateAndPermissions(t *testing.T) {
	for _, method := range []string{"code", "scan", "import"} {
		t.Run(method, func(t *testing.T) {
			provider := &fixtureProvider{}
			s, m, _ := newFixtureService(t, provider, NewMemoryStore(), 5*time.Second)
			p := draftFixture(t, s, "account")
			input := map[string]string{"subject": "alice"}
			if method == "import" {
				input = map[string]string{"json": `{"subject":"alice","token":"opaque"}`}
			}
			v := startFixture(t, s, p, method, input, false)
			if _, err := s.Get("other", v.ID); !errors.Is(err, ErrDenied) {
				t.Fatal(err)
			}
			if _, err := s.Act("other", v.ID, 1, "cancel", nil); !errors.Is(err, ErrDenied) {
				t.Fatal(err)
			}
			if method != "import" {
				v = waitState(t, s, v.ID, "waiting_input", "waiting_scan")
				next := map[string]string{}
				if method == "code" {
					next["code"] = "123456"
				}
				if _, err := s.Act("operator", v.ID, v.Revision, "continue", next); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Act("operator", v.ID, v.Revision, "continue", next); !errors.Is(err, ErrConflict) {
					t.Fatalf("duplicate input accepted: %v", err)
				}
			}
			if result := confirmFixture(t, s, v.ID); result.State != "succeeded" {
				t.Fatal(result)
			}
			if _, err := m.Version(p.Key); err != nil {
				t.Fatal(err)
			}
			want := int32(2)
			if method == "import" {
				want = 1
			}
			if provider.steps.Load() != want {
				t.Fatalf("steps=%d", provider.steps.Load())
			}
		})
	}
}

type failingProfileStore struct {
	Store
	fail atomic.Bool
}

func (f *failingProfileStore) SaveCAS(ctx context.Context, p Profile, v uint64) error {
	if f.fail.Load() {
		return errors.New("database unavailable")
	}
	return f.Store.SaveCAS(ctx, p, v)
}
func TestFailedUpdateAndIdentityMismatchPreserveOldCredentials(t *testing.T) {
	profiles := &failingProfileStore{Store: NewMemoryStore()}
	provider := &fixtureProvider{}
	s, m, _ := newFixtureService(t, provider, profiles, 5*time.Second)
	p := draftFixture(t, s, "old")
	v := startFixture(t, s, p, "password", map[string]string{"subject": "alice", "password": "old"}, false)
	if r := confirmFixture(t, s, v.ID); r.State != "succeeded" {
		t.Fatal(r)
	}
	oldVersion, _ := m.Version(p.Key)
	old, _ := profiles.Get(context.Background(), p.Key)
	for _, mode := range []string{"identity", "database", "check"} {
		t.Run(mode, func(t *testing.T) {
			p, _ = profiles.Get(context.Background(), p.Key)
			subject := "alice"
			if mode == "identity" {
				subject = "mallory"
			}
			v = startFixture(t, s, p, "password", map[string]string{"subject": subject, "password": "replacement"}, false)
			if mode == "identity" {
				r := waitState(t, s, v.ID, "failed")
				if r.Failure != "identity_mismatch" {
					t.Fatal(r)
				}
			} else {
				waitState(t, s, v.ID, "confirm_identity")
				profiles.fail.Store(mode == "database")
				provider.badCheck = mode == "check"
				r := confirmFixture(t, s, v.ID)
				if r.State != "failed" {
					t.Fatal(r)
				}
				profiles.fail.Store(false)
				provider.badCheck = false
			}
			version, _ := m.Version(p.Key)
			current, _ := profiles.Get(context.Background(), p.Key)
			if version != oldVersion || current.SecretRef != old.SecretRef || current.Revision != old.Revision {
				t.Fatalf("failed update changed active state: version=%d profile=%+v", version, current)
			}
		})
	}
}

func TestLoginCancelTimeoutCapacityAndCodeRegistrationConflict(t *testing.T) {
	s, m, _ := newFixtureService(t, &fixtureProvider{block: true}, NewMemoryStore(), time.Second)
	p := draftFixture(t, s, "a")
	p2 := draftFixture(t, s, "b")
	v := startFixture(t, s, p, "password", map[string]string{"subject": "alice", "password": "x"}, false)
	if _, err := s.Start(context.Background(), "operator", p2.Key, p2.Revision, "password", map[string]string{"subject": "alice", "password": "x"}, false); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	waitState(t, s, v.ID, "expired")
	if _, err := s.Act("operator", v.ID, 1, "continue", nil); !errors.Is(err, ErrGone) {
		t.Fatal(err)
	}
	v = startFixture(t, s, p, "password", map[string]string{"subject": "alice", "password": "x"}, false)
	if _, err := s.Act("operator", v.ID, 1, "cancel", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Act("operator", v.ID, 1, "cancel", nil); err != nil {
		t.Fatal("cancel must be idempotent", err)
	}
	waitState(t, s, v.ID, "cancelled")
	cfg, _ := (&fixtureProvider{}).Config(context.Background(), Profile{}, nil)
	key := account.Key{Site: "fixture", ID: "code"}
	if err := m.Register(key, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveDraft(context.Background(), Profile{Key: key, Label: "collision"}, 0); !errors.Is(err, ErrConflict) {
		t.Fatal("code registration overwritten", err)
	}
}

func TestProviderDeclarationAndMissingSecretsFailClosed(t *testing.T) {
	if _, err := New(Options{Manager: account.NewManager(nil), Profiles: NewMemoryStore()}); !errors.Is(err, ErrNotConfigured) {
		t.Fatal(err)
	}
	s, _, _ := newFixtureService(t, &fixtureProvider{}, NewMemoryStore(), 5*time.Second)
	p := draftFixture(t, s, "a")
	if _, err := s.Start(context.Background(), "operator", p.Key, p.Revision, "oauth-not-declared", nil, false); err == nil {
		t.Fatal("undeclared method accepted")
	}
	if _, err := s.Start(context.Background(), "operator", p.Key, p.Revision, "password", map[string]string{"password": "x", "subject": "alice", "token": "unknown"}, false); err == nil {
		t.Fatal("undeclared input accepted")
	}
}

func TestProcessRetentionBoundAndPasswordOptOut(t *testing.T) {
	profiles := NewMemoryStore()
	s, _, _ := newFixtureService(t, &fixtureProvider{}, profiles, 5*time.Second)
	s.opts.MaxRetained = 2
	p := draftFixture(t, s, "bounded")
	first := ""
	for i := 0; i < 6; i++ {
		p, _ = profiles.Get(context.Background(), p.Key)
		v := startFixture(t, s, p, "password", map[string]string{"subject": "alice", "password": "opt-in"}, i == 0)
		if first == "" {
			first = v.ID
		}
		if got := confirmFixture(t, s, v.ID); got.State != "succeeded" {
			t.Fatal(got)
		}
		deadline := time.Now().Add(time.Second)
		for len(s.slots) > 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
	}
	s.mu.Lock()
	count := len(s.processes)
	s.mu.Unlock()
	if count > 2 {
		t.Fatalf("retained %d processes", count)
	}
	if _, err := s.Get("operator", first); !errors.Is(err, ErrGone) {
		t.Fatal("old terminal process retained", err)
	}
	p, _ = profiles.Get(context.Background(), p.Key)
	if p.InputsRef != "" {
		t.Fatal("password opt-out retained old saved password")
	}
}

type uncooperativeProvider struct {
	fixtureProvider
	release chan struct{}
}

func (p *uncooperativeProvider) Start(ctx context.Context, profile Profile, method string, in map[string]string) (Flow, error) {
	<-p.release
	return nil, ctx.Err()
}
func TestShutdownReturnsWithinDrainBudget(t *testing.T) {
	secrets, _ := account.NewFileStore(t.TempDir(), []byte("01234567890123456789012345678901"))
	s, err := New(Options{Manager: account.NewManager(htmlfetch.NewClient(htmlfetch.Config{})), Profiles: NewMemoryStore(), Secrets: secrets})
	if err != nil {
		t.Fatal(err)
	}
	provider := &uncooperativeProvider{release: make(chan struct{})}
	_ = s.RegisterProvider(provider)
	p := draftFixture(t, s, "slow")
	startFixture(t, s, p, "password", map[string]string{"subject": "alice", "password": "x"}, false)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err = s.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("shutdown ignored caller deadline", err)
	}
	close(provider.release)
	s.Close()
}

type slowCleanupSecrets struct {
	account.Store
	entered chan struct{}
	release chan struct{}
	once    atomic.Bool
}

func (s *slowCleanupSecrets) Delete(ctx context.Context, key account.Key) error {
	if strings.Contains(key.ID, "\x00candidate-") && s.once.CompareAndSwap(false, true) {
		close(s.entered)
		<-s.release
	}
	return s.Store.Delete(ctx, key)
}
func TestTemporaryStagingOwnershipSurvivesTerminalUntilCleanup(t *testing.T) {
	profiles := NewMemoryStore()
	s, _, _ := newFixtureService(t, &fixtureProvider{}, profiles, 5*time.Second)
	secrets := &slowCleanupSecrets{Store: s.opts.Secrets, entered: make(chan struct{}), release: make(chan struct{})}
	s.opts.Secrets = secrets
	p := draftFixture(t, s, "staging")
	v := startFixture(t, s, p, "password", map[string]string{"subject": "alice", "password": "x"}, false)
	v = waitState(t, s, v.ID, "confirm_identity")
	if _, err := s.Act("operator", v.ID, v.Revision, "confirm", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-secrets.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("cleanup not entered")
	}
	defer closeUnlessClosed(secrets.release)
	p, _ = profiles.Get(context.Background(), p.Key)
	if _, err := s.Start(context.Background(), "operator", p.Key, p.Revision, "password", map[string]string{"subject": "alice", "password": "next"}, false); !errors.Is(err, ErrBusy) {
		t.Fatal("new process overwrote slot during old cleanup", err)
	}
	close(secrets.release)
	waitState(t, s, v.ID, "succeeded")
	// Simulate ciphertext left by a process crash. Only known temporary slots
	// are swept; the verified profile's immutable generation is retained.
	state := &account.StoredState{HTTP: &account.CredentialState{Values: map[string]string{"temporary": "secret"}}}
	ref, err := s.saveStaging(context.Background(), p.Key, "inputs", state)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	m := account.NewManager(htmlfetch.NewClient(htmlfetch.Config{}))
	restarted, err := New(Options{Manager: m, Profiles: profiles, Secrets: secrets.Store})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	_ = restarted.RegisterProvider(&fixtureProvider{})
	if err = restarted.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = secrets.Load(context.Background(), secretKey(p.Key, ref)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("restart retained staging", err)
	}
	if _, err = secrets.Load(context.Background(), secretKey(p.Key, p.SecretRef)); err != nil {
		t.Fatal("restart removed durable generation", err)
	}
}
func closeUnlessClosed(c chan struct{}) {
	select {
	case <-c:
	default:
		close(c)
	}
}

func TestLoginOutcomeNotificationsAreSanitizedAndNonblocking(t *testing.T) {
	s, _, _ := newFixtureService(t, &fixtureProvider{}, NewMemoryStore(), 5*time.Second)
	blocked, release := make(chan struct{}), make(chan struct{})
	defer closeUnlessClosed(release)
	s.SetEventListener(func(e account.HealthEvent) {
		if e.Kind != "login-failed" || e.Reason != "identity_mismatch" {
			t.Errorf("unexpected event %+v", e)
		}
		close(blocked)
		<-release
	})
	p := draftFixture(t, s, "event")
	v := startFixture(t, s, p, "password", map[string]string{"subject": "wrong-user", "password": "private-password"}, false)
	waitState(t, s, v.ID, "failed")
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("no failure notification")
	}
	started := time.Now()
	for i := 0; i < 1000; i++ {
		s.emit(p.Key, "login-failed", "identity_mismatch")
	}
	if time.Since(started) > time.Second {
		t.Fatal("slow notifier blocked login event producer")
	}
	if len(s.events) > 64 {
		t.Fatal("unbounded event queue")
	}
	s.SetEventListener(nil)
	close(release)
}

func TestEncryptedGenerationCannotCrossAccountBoundary(t *testing.T) {
	s, _, _ := newFixtureService(t, &fixtureProvider{}, NewMemoryStore(), 5*time.Second)
	a := account.Key{Site: "fixture", ID: "one"}
	b := account.Key{Site: "fixture", ID: "two"}
	ref, err := s.saveSecret(context.Background(), a, &account.StoredState{HTTP: &account.CredentialState{Values: map[string]string{"token": "private"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.opts.Secrets.Load(context.Background(), secretKey(b, ref)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("another account consumed generation reference", err)
	}
	if _, err = s.opts.Secrets.Load(context.Background(), secretKey(a, ref)); err != nil {
		t.Fatal(err)
	}
}

func TestCancelDiscardsQRCodeAndInputPrompt(t *testing.T) {
	s, _, _ := newFixtureService(t, &fixtureProvider{}, NewMemoryStore(), 5*time.Second)
	p := draftFixture(t, s, "qr-cancel")
	v := startFixture(t, s, p, "scan", map[string]string{"subject": "alice"}, false)
	v = waitState(t, s, v.ID, "waiting_scan")
	if v.ImageDataURL == "" {
		t.Fatal("missing fixture QR")
	}
	v, err := s.Act("operator", v.ID, v.Revision, "cancel", nil)
	if err != nil || v.ImageDataURL != "" || len(v.Fields) != 0 || v.Remote {
		t.Fatalf("cancel retained interactive material %+v %v", v, err)
	}
}
