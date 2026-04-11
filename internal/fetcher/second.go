package fetcher

import (
	"context"
	"github.com/ydtg1993/papa/internal/crawler"
	"time"
)

type FetchSecond struct {
}

func (f *FetchSecond) GetStage() string {
	return "second" //必须与配置中自定义的保持一致
}

func (f *FetchSecond) FetchHandler(ctx context.Context, task *crawler.Task, engine *crawler.Engine) error {
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
