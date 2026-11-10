package tasksource

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/ydtg1993/oao"
	"github.com/ydtg1993/papa/v4/engine"
)

type compensationActions struct {
	stubActions
	version   uint64
	id        uint
	reprocess int
}

func (s *compensationActions) RetryTaskVersioned(_ context.Context, id uint, reprocess int, version uint64) error {
	s.id, s.version, s.reprocess = id, version, reprocess
	return nil
}

func (s *compensationActions) ReleaseTaskCompensation(_ context.Context, id uint, version uint64) error {
	s.id, s.version = id, version
	return nil
}

func TestCompensationReleaseActionValidatesAndForwardsVersion(t *testing.T) {
	acts := &compensationActions{}
	table := Table(nil, acts)
	var action oao.Action
	for _, a := range table.Actions {
		if a.Key == "compensation_release" {
			action = a
		}
	}
	if action.Handler == nil {
		t.Fatal("missing manual release action")
	}
	if err := action.Handler(context.Background(), oao.ActionRequest{ID: "7"}); err == nil {
		t.Fatal("missing version accepted")
	}
	if acts.id != 0 {
		t.Fatal("invalid release reached backend")
	}
	if err := action.Handler(context.Background(), oao.ActionRequest{ID: "7", Row: map[string]any{"compensation_version": "12"}}); err != nil {
		t.Fatal(err)
	}
	if acts.id != 7 || acts.version != 12 {
		t.Fatal("release lost CAS identity")
	}
	var mapped *oao.ActionError
	if !errors.As(toOaoError(engine.ErrCompensationBlocked), &mapped) || mapped.Status != http.StatusConflict {
		t.Fatal("blocked compensation should be an actionable conflict")
	}
}

func TestTaskRetryCarriesCompensationVersionWhenSupported(t *testing.T) {
	acts := &compensationActions{}
	action := Table(nil, acts).Actions[0]
	request := oao.ActionRequest{ID: "7", Row: map[string]any{"reprocess": "3"}}
	if err := action.Handler(context.Background(), request); err == nil {
		t.Fatal("missing compensation version accepted")
	}
	request.Row["compensation_version"] = "12"
	if err := action.Handler(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if acts.id != 7 || acts.version != 12 || acts.reprocess != 3 || len(acts.stubActions.gotID) != 0 {
		t.Fatal("versioned retry fell back to legacy endpoint")
	}
}
