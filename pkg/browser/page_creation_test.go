package browser

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/cdp"
)

type canceledPageCreationCDP struct {
	cancel                   context.CancelFunc
	created, closed, bounded bool
}

func (c *canceledPageCreationCDP) Event() <-chan *cdp.Event { return nil }
func (c *canceledPageCreationCDP) Call(ctx context.Context, _ string, method string, _ interface{}) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch method {
	case "Target.createTarget":
		c.created = true
		return []byte(`{"targetId":"partial-target"}`), nil
	case "Target.attachToTarget":
		c.cancel()
		return nil, ctx.Err()
	case "Target.closeTarget":
		c.closed = true
		deadline, ok := ctx.Deadline()
		c.bounded = ok && time.Until(deadline) <= 3*time.Second
		return []byte(`{"success":true}`), nil
	}
	return []byte(`{}`), nil
}
func TestPageCreationCancellationClosesPartialTarget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &canceledPageCreationCDP{cancel: cancel}
	b := &Browser{Browser: rod.New().Client(client).NoDefaultDevice()}
	page, err := b.newPageBaseContext(ctx, PageOptions{})
	if page != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("page=%v err=%v", page, err)
	}
	if !client.created || !client.closed || !client.bounded {
		t.Fatalf("partial target cleanup: created=%v closed=%v bounded=%v", client.created, client.closed, client.bounded)
	}
}
