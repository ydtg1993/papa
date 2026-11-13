package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/pkg/loggers"
)

func TestHTTPShutdownReportsLiveHandlerAfterConnectionClose(t *testing.T) {
	entered, release, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
	drain := newRequestDrain()
	srv := httptest.NewServer(drain.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		defer close(exited)
		<-release
	})))
	defer srv.Close()
	defer close(release)
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		if res, err := srv.Client().Get(srv.URL); err == nil {
			res.Body.Close()
		}
	}()
	<-entered
	a := &App{httpSrv: srv.Config, httpRequests: drain, Config: &config.Config{Server: config.ServerConfig{ShutdownTimeout: 20 * time.Millisecond}}, Logger: &loggers.LoggerSet{Sys: quietLogger()}}
	if a.shutdownHTTP() {
		t.Fatal("connection close must not report a live handler as drained")
	}
	<-clientDone
	select {
	case <-exited:
		t.Fatal("fixture handler unexpectedly completed")
	default:
	}
}

func TestHTTPShutdownWaitsForHijackedHandlerAndClosesAdmission(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	drain := newRequestDrain()
	srv := httptest.NewServer(drain.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
		close(entered)
		<-release
	})))
	defer srv.Close()
	defer close(release)
	go func() {
		res, err := srv.Client().Get(srv.URL)
		if err == nil {
			res.Body.Close()
		}
	}()
	<-entered
	a := &App{httpSrv: srv.Config, httpRequests: drain, Config: &config.Config{Server: config.ServerConfig{ShutdownTimeout: 20 * time.Millisecond}}, Logger: &loggers.LoggerSet{Sys: quietLogger()}}
	if a.shutdownHTTP() {
		t.Fatal("net/http Shutdown excludes hijacked connections, but their handlers must still drain")
	}
	w := httptest.NewRecorder()
	drain.wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("new handler admitted after stop") })).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatal(w.Code)
	}
}

func TestRequestDrainWaitsForCompletionAndIsIdempotent(t *testing.T) {
	drain := newRequestDrain()
	entered, release, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(exited)
		drain.wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { close(entered); <-release })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	}()
	<-entered
	drain.stop()
	drain.stop()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if drain.wait(ctx) {
		t.Fatal("unfinished handler reported drained")
	}
	close(release)
	<-exited
	if !drain.wait(ctx) {
		t.Fatal("completed handler must be recognized even at deadline")
	}
}
