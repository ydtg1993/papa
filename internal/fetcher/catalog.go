package fetcher

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/go-rod/rod"
	"github.com/ydtg1993/papa/internal/crawler"
	"log"
	"time"
)

type FetchCatalog struct {
}

func (f *FetchCatalog) GetStage() string {
	return "catalog"
}

func (*FetchCatalog) FetchHandler(ctx context.Context, task *crawler.Task, engine *crawler.Engine) error {
	browserWrapper, err := engine.GetBrowserPool().Get(ctx)
	if err != nil {
		return err
	}
	defer engine.GetBrowserPool().Put(browserWrapper)

	// 1. 创建空白页面（不自动导航）
	page := browserWrapper.Browser.MustPage("")
	defer page.Close()

	if err := page.Timeout(10 * time.Second).Navigate(task.URL); err != nil {
		return err
	}
	page.MustWaitLoad()

	// 执行滚动加载
	// 定义常量
	const (
		maxScrolls     = 3               // 最大滚动次数，防止无限循环
		waitTimeMs     = 1500            // 每次滚动后等待新内容加载的时间（毫秒）
		scrollInterval = 2 * time.Second // 额外间隔（可选）
	)
	_, err = page.Evaluate(&rod.EvalOptions{
		JS: `
			window.scrollOnce = function scrollOnce(lastHeight) {
				window.scrollTo({ top: document.documentElement.scrollHeight, behavior: 'smooth' });
				const currentHeight = document.documentElement.scrollHeight;
				const heightIncreased = currentHeight > lastHeight;
				const reachedBottom = (lastHeight > 0 && !heightIncreased);
				window.dispatchEvent(new Event('scroll'));
				return { currentHeight, reachedBottom, heightIncreased };
			};
		`,
		ByValue: true,
	})
	if err != nil {
		log.Fatalf("注入 scrollOnce 函数失败: %v", err)
	}
	var lastHeight int
	for i := 0; i < maxScrolls; i++ {
		// 调用 scrollOnce
		evalOpts := &rod.EvalOptions{
			JS:      "window.scrollOnce",
			JSArgs:  []interface{}{lastHeight},
			ByValue: true,
		}
		res, err := page.Evaluate(evalOpts)
		if err != nil {
			log.Printf("第 %d 次滚动执行出错: %v", i+1, err)
			break
		}

		var result struct {
			CurrentHeight   int  `json:"currentHeight"`
			ReachedBottom   bool `json:"reachedBottom"`
			HeightIncreased bool `json:"heightIncreased"`
		}
		jsonData, err := json.Marshal(res.Value)
		if err != nil {
			log.Printf("JSON 编码失败: %v", err)
			break
		}
		if err := json.Unmarshal(jsonData, &result); err != nil {
			log.Printf("解析返回值失败: %v", err)
			break
		}
		fmt.Printf("高度: %d, 到底: %v\n", result.CurrentHeight, result.ReachedBottom)

		time.Sleep(scrollInterval)
	}
	return nil
}
