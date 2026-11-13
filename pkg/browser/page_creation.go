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
	controller := b.Browser.Context(ctx)
	target, err := (proto.TargetCreateTarget{URL: "about:blank", BrowserContextID: controller.BrowserContextID}).Call(controller)
	if err != nil {
		return nil, err
	}
	page, err := controller.PageFromTarget(target.TargetID)
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
