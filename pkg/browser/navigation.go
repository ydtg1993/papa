package browser

import (
	"context"
	"fmt"
	"time"

	"github.com/go-rod/rod"
)

// NavigatePage navigates an existing managed page (for example an authenticated
// account page), preserving its current CDP request governor and origin scope.
// Interception rules must be chosen when the page is created; replacing them on
// a live page could bypass request limits or strand in-flight network events.
func NavigatePage(ctx context.Context, page *rod.Page, rawURL string, opts PageOptions) (err error) {
	if err := validateNavigation(rawURL); err != nil {
		return err
	}
	if err := ConfigurePage(ctx, page, opts); err != nil {
		return err
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	navCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	page = page.Context(navCtx)
	challenge, stopChallenge := observeSiteChallenge(page)
	defer stopChallenge()
	defer func() { err = challenge.prefer(err) }()
	if err := page.Navigate(rawURL); err != nil {
		return fmt.Errorf("navigate: %w", err)
	}
	if err := page.WaitLoad(); err != nil {
		return fmt.Errorf("wait load: %w", err)
	}
	if err := challenge.prefer(nil); err != nil {
		return err
	}
	if err := applyWaits(page, opts); err != nil {
		return err
	}
	if opts.Wait > 0 {
		select {
		case <-navCtx.Done():
			return navCtx.Err()
		case <-time.After(opts.Wait):
		}
	}
	if opts.Interact != nil {
		return opts.Interact(navCtx, page)
	}
	return nil
}

// ConfigurePage applies options to an existing page without navigating or
// replacing its installed interception/governance handlers.
func ConfigurePage(ctx context.Context, page *rod.Page, opts PageOptions) error {
	if page == nil {
		return fmt.Errorf("navigate page: page required")
	}
	if opts.Block != nil || len(opts.GovernanceOrigins) > 0 {
		return fmt.Errorf("navigate page: interception and governance origins must be set when creating the page")
	}
	if len(opts.RemoveHeaders) > 0 {
		if state := lookupPageIdentity(page); state != nil {
			if err := validateIdentityHeaders(state.snapshot, nil, opts.RemoveHeaders, "PageOptions"); err != nil {
				return err
			}
		}
		return fmt.Errorf("navigate page: inherited headers cannot be removed without recreating the page")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	navCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	page = page.Context(navCtx)
	identityHandled, err := configureIdentityPage(navCtx, page, opts)
	if err != nil {
		return err
	}
	if !identityHandled && opts.Device != nil {
		if err := page.Emulate(*opts.Device); err != nil {
			return fmt.Errorf("emulate device: %w", err)
		}
	}
	if !identityHandled && opts.Headers != nil {
		if _, err := page.SetExtraHeaders(mergeExtraHeaders(nil, opts.Headers, nil)); err != nil {
			return fmt.Errorf("set page headers: %w", err)
		}
	}
	if len(opts.Cookies) > 0 {
		if err := page.SetCookies(opts.Cookies); err != nil {
			return fmt.Errorf("set page cookies: %w", err)
		}
	}
	return nil
}
