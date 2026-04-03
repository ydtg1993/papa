package fetcher

import (
	"context"
	"github.com/go-rod/rod/lib/proto"
	"github.com/ydtg1993/papa/internal/crawler"
	"strings"
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

	browserWrapper, err := engine.GetBrowserPool().Get(reqCtx)
	if err != nil {
		return err
	}
	defer engine.GetBrowserPool().Put(browserWrapper)

	// 1. 创建空白页面（不自动导航）
	page := browserWrapper.Browser.MustPage("")
	defer page.Close()
	time.Sleep(3 * time.Second)

	m3u8Chan := make(chan string, 1)
	go page.EachEvent(func(e *proto.NetworkRequestWillBeSent) {
		url := e.Request.URL
		// 匹配视频分段或清单文件
		if strings.Contains(url, ".m3u8") {
			select {
			case m3u8Chan <- url:
			default:
			}
		}
	})()

	// 3. 导航到目标 URL
	task.URL = "https://hanime.tv/videos/hentai/my-mother-1"
	if err := page.Timeout(timeout).Navigate(task.URL); err != nil {
		return err
	}
	if err := page.Timeout(timeout).WaitLoad(); err != nil {
		return err
	}

	element, err := page.Element(".play-btn")
	if err != nil {
		_ = element.Click(proto.InputMouseButtonLeft, 1)
	}
	// 多重触发播放
	page.MustEval(`() => {
    const btn = document.querySelector('.play-btn, .vjs-big-play-button');
        if (btn) btn.click();
        setTimeout(() => {
            const v = document.querySelector('video');
            if (v && v.paused) v.play();
        }, 1000);
}`)

	// 6. 等待 M3U8 链接被捕获
	select {
	case m3u8URL := <-m3u8Chan:
		engine.LoggerSet.Sys.Infof("捕获到 M3U8 链接: %s", m3u8URL)
		// 可以继续下载或返回
	case <-time.After(30 * time.Second):
		engine.LoggerSet.Sys.Warn("未捕获到 M3U8 链接")
	}

	// 保持页面一段时间以便观察（可选）
	<-time.After(5 * time.Second)
	return nil
}
