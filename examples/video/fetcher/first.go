package fetcher

import (
	"context"
	"time"

	"github.com/ydtg1993/papa"
)

type FetchFirst struct{}

func (f *FetchFirst) GetStage() string {
	return "first" // 必须与 config.yaml 的 crawler.stages 的 key 保持一致
}

func (*FetchFirst) FetchHandler(ctx context.Context, task *papa.Task, engine *papa.Engine) error {
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

	// 爬取页面逻辑 TODO

	return nil
}
