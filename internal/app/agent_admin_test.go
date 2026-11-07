package app

import (
	"context"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/pkg/agentadmin"
	"github.com/ydtg1993/papa/v4/pkg/agentstore"
	"testing"
)

func TestAgentCenterDisabledHasNoTypedNilOrDatabaseWork(t *testing.T) {
	a := &App{Config: &config.Config{}}
	if a.agentAdministration() != nil {
		t.Fatal("typed nil administration")
	}
	if e := a.UseAgentCenter(agentadmin.Options{}); e != agentadmin.ErrInvalid {
		t.Fatal(e)
	}
	if e := a.CloseAgentCenter(context.Background()); e != nil {
		t.Fatal(e)
	}
}
func TestAgentCenterHostCanWaitForAppOwnedShutdown(t *testing.T) {
	s, e := agentadmin.New(agentadmin.Config{Audit: agentstore.NewMemoryStore()})
	if e != nil {
		t.Fatal(e)
	}
	a := &App{agentCenter: s}
	if e = a.CloseAgentCenter(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e = a.CloseAgentCenter(context.Background()); e != nil {
		t.Fatal(e)
	}
}
