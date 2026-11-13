package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/admin/access"
)

type blockedShutdownWhitelist struct{ entered, release chan struct{} }

func (r blockedShutdownWhitelist) LoadStringsContext(context.Context, string) ([]string, error) {
	close(r.entered)
	<-r.release
	return nil, nil
}

func TestShutdownRunRetainsDatabaseUntilAllUsersDrain(t *testing.T) {
	for _, blocked := range []string{"http", "whitelist", "none"} {
		t.Run(blocked, func(t *testing.T) {
			a, _ := runtimeTestApp(t)
			a.Config.Server.ShutdownTimeout = 20 * time.Millisecond
			sqlDB, err := a.DB.DB()
			if err != nil {
				t.Fatal(err)
			}
			defer sqlDB.Close()
			var finish func()
			switch blocked {
			case "http":
				entered, release, result := make(chan struct{}), make(chan struct{}), make(chan error, 1)
				a.httpRequests = newRequestDrain()
				srv := httptest.NewServer(a.httpRequests.wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
					close(entered)
					<-release
					result <- sqlDB.Ping() // The handler still needs its shared dependency.
				})))
				defer srv.Close()
				a.httpSrv = srv.Config
				go func() {
					res, err := srv.Client().Get(srv.URL)
					if err == nil {
						res.Body.Close()
					}
				}()
				<-entered
				finish = func() {
					close(release)
					if err := <-result; err != nil {
						t.Errorf("handler lost DB before completion: %v", err)
					}
				}
			case "whitelist":
				reader := blockedShutdownWhitelist{make(chan struct{}), make(chan struct{})}
				a.whitelistReload = access.NewWhitelistReloader(reader, a.Logger.Sys, func([]string) {}, time.Millisecond)
				a.whitelistReload.Start(context.Background())
				<-reader.entered
				finish = func() {
					close(reader.release)
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					defer cancel()
					if !a.whitelistReload.Stop(ctx) {
						t.Error("released reload did not drain")
					}
				}
			}
			if finish != nil {
				defer finish()
			}
			a.shutdownRun(false) // Same coordinator used by Run and startup failure.
			err = sqlDB.Ping()
			if blocked == "none" && err == nil {
				t.Error("fully drained App did not close DB")
			}
			if blocked != "none" && err != nil {
				t.Errorf("%s is still using DB, but App closed it: %v", blocked, err)
			}
		})
	}
}
