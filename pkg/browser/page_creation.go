package browser

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// createManagedPage owns the target as soon as Chrome returns its ID. Rod's
// Browser.Page cleans up a failed attachment with the original context, which
// cannot close the target when attachment fails because that context expired.
func (b *Browser) createManagedPage(ctx context.Context) (*rod.Page, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	controller := b.Browser.Context(ctx)
	// Creation is a side effect: dropping its reply on cancellation loses the
	// only target ID we may safely close. Allow a bounded acknowledgement grace
	// after cancellation, then close the known target without attaching/running it.
	creation, stopCreation := context.WithCancel(context.WithoutCancel(ctx))
	creationDone := make(chan struct{})
	stopGrace := context.AfterFunc(ctx, func() {
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		select {
		case <-creationDone:
		case <-timer.C:
			stopCreation()
		}
	})
	target, err := (proto.TargetCreateTarget{URL: "about:blank", BrowserContextID: controller.BrowserContextID}).Call(b.Browser.Context(creation))
	close(creationDone)
	stopGrace()
	stopCreation()
	if err != nil {
		return nil, errors.Join(ctx.Err(), err)
	}
	var page *rod.Page
	err = ctx.Err()
	if err == nil {
		page, err = controller.PageFromTarget(target.TargetID)
	}
	if err == nil {
		return page, nil
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if _, closeErr := (proto.TargetCloseTarget{TargetID: target.TargetID}).Call(b.Browser.Context(cleanup)); closeErr != nil {
		err = errors.Join(err, fmt.Errorf("close partially created page: %w", closeErr))
	}
	return nil, err
}
