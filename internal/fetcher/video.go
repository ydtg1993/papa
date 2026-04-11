package fetcher

import (
	"context"
	"github.com/ydtg1993/papa/internal/crawler"
	"time"
)

type FetchVideo struct {
}

func (f *FetchVideo) GetStage() string {
	return "video"
}

func (f *FetchVideo) FetchHandler(ctx context.Context, task *crawler.Task, engine *crawler.Engine) error {
	bw, err := engine.GetBrowserPool().Get(ctx)
	if err != nil {
		return err
	}
	defer engine.GetBrowserPool().Put(bw)

	// 1. 创建空白页面（不自动导航）
	page := bw.Browser.MustPage("")
	defer page.Close()

	if err := page.Timeout(10 * time.Second).Navigate(task.URL); err != nil {
		return err
	}
	page.MustWaitLoad()

	//爬取页面逻辑 TODO

	return nil
}
