package fetcher

import (
	"context"
	"fmt"
	"github.com/chromedp/chromedp"
	"github.com/ydtg1993/papa/internal/crawler"
	"github.com/ydtg1993/papa/internal/loggers"
	"log"
	"time"
)

type FetchCatalog struct {
}

func (f *FetchCatalog) GetStage() string {
	return "catalog"
}

func (*FetchCatalog) FetchHandler(ctx context.Context, task *crawler.Task, engine *crawler.Engine) error {
	timeout := 60 * time.Second
	reqCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	browser, err := engine.BrowserPool.Get(reqCtx)
	if err != nil {
		loggers.BrowserLogger.Errorf("get browser failed: %s", err)
	}
	defer engine.BrowserPool.Put(browser)

	err = chromedp.Run(browser.Ctx,
		chromedp.Navigate(task.URL),
		chromedp.WaitVisible("body", chromedp.ByQuery),
	)
	if err != nil {
		return fmt.Errorf("navigate failed: %w", err)
	}
	log.Println("navigated successfully")

	err = chromedp.Run(browser.Ctx,
		chromedp.WaitVisible(`div.elevation-3:first-child`, chromedp.ByQuery),
	)
	if err != nil {
		return fmt.Errorf("wait element failed: %w", err)
	}
	log.Println("element visible")

	err = chromedp.Run(browser.Ctx,
		chromedp.Click(`div.elevation-3:first-child`, chromedp.ByQuery),
	)
	if err != nil {
		return fmt.Errorf("double click failed: %w", err)
	}

	return nil
}
