package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/internal/workerpool"
	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/egress"
	"github.com/ydtg1993/papa/v4/pkg/events"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

func TestSiteChallengeTaskExecutesOnceWithoutAccountSwitch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("cf-mitigated", "challenge")
		w.WriteHeader(401)
	}))
	defer srv.Close()
	e, _, task, eventsOut := persistenceFixture(t)
	manager := account.NewManager(htmlfetch.NewClient(htmlfetch.Config{Network: egress.Config{PrivateOrigins: []string{srv.URL}}}))
	for _, id := range []string{"first", "backup"} {
		if err := manager.Register(account.Key{Site: task.Site, ID: id}, account.Config{Origins: []string{srv.URL}, CheckInterval: time.Hour, Login: func(_ context.Context, c *account.Credentials) error {
			c.Headers["Authorization"] = "saved-credential"
			return nil
		}, Check: func(context.Context, *account.Credentials) (bool, error) { return true, nil }}); err != nil {
			t.Fatal(err)
		}
	}
	pools, err := account.NewPools(context.Background(), manager, account.NewMemoryPoolStore(), account.PoolOptions{ScanInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer pools.Close()
	e.SetAccountManager(manager)
	key := account.PoolKey{Site: task.Site, Name: "primary"}
	if _, err = pools.Upsert(context.Background(), account.PoolConfig{Key: key, Members: []string{"first", "backup"}}, 0); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	before, err := pools.Acquire(ctx, key, account.HTTP)
	if err != nil {
		t.Fatal(err)
	}
	task.AccountPool = key.Name
	info := e.stages[stageKeyOf(task)]
	info.config.MaxAttempts, info.config.Backoff = 3, time.Hour
	calls := 0
	info.fetcher = stageRunVersionFetcher(func(ctx context.Context, _ *Task, _ *Engine) error {
		calls++
		if a, ok := account.AttemptFromContext(ctx); !ok || a.Account != before.Account {
			t.Fatal("task lost fixed account")
		}
		_, err := e.FetchHTTP(ctx, htmlfetch.Request{URL: srv.URL})
		return err
	})
	err = e.stageHandler(info)(ctx, task)
	if !errors.Is(err, core.ErrSiteChallenge) || core.Retryable(err) || calls != 1 || account.IsAttemptFailure(err) {
		t.Fatalf("challenge retried or misclassified: calls=%d err=%v", calls, err)
	}
	drainTestEvents(t, e)
	if countTaskEvents(eventsOut, events.TypeTaskFailed) != 1 {
		t.Fatal("failed terminal event missing")
	}
	after, err := pools.Acquire(ctx, key, account.HTTP)
	if err != nil {
		t.Fatal(err)
	}
	if after.Account != before.Account || after.SelectionVersion != before.SelectionVersion || after.ManagementVersion != before.ManagementVersion || after.CredentialVersion != before.CredentialVersion {
		t.Fatalf("challenge changed account: before=%+v after=%+v", before, after)
	}
}

func TestSiteChallengeErrorQueueCanCompensateLater(t *testing.T) {
	f := newFakeTaskDB()
	setRows(f, 1, "stub", models.TaskStatusFailed)
	f.rows[0]["error"] = (&core.SiteChallengeError{StatusCode: 403, Evidence: "cf-mitigated: challenge"}).Error()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := queueEngine(t, f, pool)
	e.cfg.ErrorQueue = config.ErrorQueueConfig{BatchSize: 10, WorkerCount: 1, MaxRetry: 3}
	n, err := e.ProcessErrorQueue()
	if err != nil || n != 1 {
		t.Fatalf("later compensation blocked: n=%d err=%v", n, err)
	}
	main, urgent := pool.QueueDepths()
	if main+urgent != 1 {
		t.Fatal("failed challenge not queued for later compensation")
	}
}
