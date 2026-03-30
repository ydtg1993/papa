package fetcher

import (
	"context"
	"github.com/chromedp/chromedp"
	"github.com/ydtg1993/papa/internal/crawler"
	"time"
)

type FetchCatalog struct {
}

func (f *FetchCatalog) GetStage() string {
	return "catalog"
}

func (*FetchCatalog) FetchHandler(ctx context.Context, task *crawler.Task, engine *crawler.Engine) error {
	timeout := 60 * time.Second
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	browser, err := engine.BrowserPool.Get(reqCtx)
	if err != nil {
		return err
	}
	defer engine.BrowserPool.Put(browser)

	err = chromedp.Run(browser.Ctx,
		chromedp.Navigate(task.URL),
		chromedp.WaitVisible("body", chromedp.ByQuery),
	)
	if err != nil {
		return err
	}

	err = chromedp.Run(browser.Ctx,
		chromedp.WaitVisible(`div.elevation-3:first-child`, chromedp.ByQuery),
	)
	if err != nil {
		return err
	}

	err = chromedp.Run(browser.Ctx,
		chromedp.Click(`div.elevation-3:first-child`, chromedp.ByQuery),
	)
	if err != nil {
		return err
	}
	return nil
}
