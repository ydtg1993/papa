package fetcher

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/go-rod/rod"
	"github.com/ydtg1993/papa/internal/crawler"
	"github.com/ydtg1993/papa/internal/models"
	"strings"
	"time"
)

type FetchCatalog struct {
}

func (f *FetchCatalog) GetStage() string {
	return "catalog"
}

func (*FetchCatalog) FetchHandler(ctx context.Context, task *crawler.Task, engine *crawler.Engine) error {
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

	logger := engine.GetLoggerSet().Fetcher
	// 定义常量
	const (
		maxScrolls     = 3               // 最大滚动次数，防止无限循环
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
		return fmt.Errorf("注入 scrollOnce 函数失败: %v", err)
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
			logger.Errorf("第 %d 次滚动执行出错: %v", i+1, err)
			break
		}

		var result struct {
			CurrentHeight   int  `json:"currentHeight"`
			ReachedBottom   bool `json:"reachedBottom"`
			HeightIncreased bool `json:"heightIncreased"`
		}
		jsonData, err := json.Marshal(res.Value)
		if err != nil {
			break
		}
		if err := json.Unmarshal(jsonData, &result); err != nil {
			break
		}
		time.Sleep(scrollInterval)
	}
	elements, err := page.Elements("div.topic-list-box")
	if err != nil {
		return fmt.Errorf("获取列表元素失败: %w", err)
	}

	var dataTasks []*models.CrawlerTask
	detail := &FetchDetail{}
	// 遍历动漫列表 采集url 封面 标签 标题
	for _, element := range elements {
		// 1. 提取链接和 URL
		aElem, err := element.Element("a.topic-list-item")
		if err != nil {
			// 如果连 a 标签都没有，跳过这个 item
			continue
		}
		href, err := aElem.Attribute("href")
		if err != nil || href == nil {
			continue
		}
		fullURL := *href
		// 2. 提取封面图 src
		// 优先找 amp-img，再找 img
		coverSrc := ""
		ampImg, err := element.Element("amp-img")
		if err == nil {
			src, _ := ampImg.Attribute("src")
			if src != nil {
				coverSrc = *src
			}
		}
		if coverSrc == "" {
			img, err := element.Element("img")
			if err == nil {
				src, _ := img.Attribute("src")
				if src != nil {
					coverSrc = *src
				}
			}
		}
		// 3. 提取标题（.h3）
		titleElem, err := element.Element(".h3")
		title := ""
		if err == nil {
			title = titleElem.MustText()
		}
		// 4. 提取作者（.topic-list-item--author）
		authorElem, err := element.Element(".topic-list-item--author")
		author := ""
		if err == nil {
			author = strings.TrimSpace(authorElem.MustText())
		}

		// 5. 提取所有分类（.tag）
		tagElems, err := element.Elements(".tag")
		var tags []string
		if err == nil {
			for _, tagElem := range tagElems {
				text := strings.TrimSpace(tagElem.MustText())
				if text != "" {
					tags = append(tags, text)
				}
			}
		}

		downloadResult := engine.GetFiledown().Download(ctx, coverSrc, "", nil)
		if downloadResult.Error != nil {
			logger.Errorf("下载图片失败 %s", coverSrc)
		}
		// 转为 JSON 存入 datatypes.JSON
		jsonData, err := json.Marshal(models.DetailContent{
			Cover:    downloadResult.OutputFile,
			CoverURL: coverSrc,
			Title:    title,
			Author:   author,
			Tags:     tags,
			SeriesContent: models.SeriesContent{
				Series:    make(map[string]string),
				Downloads: map[string]bool{},
			},
		})
		dataTasks = append(dataTasks, &models.CrawlerTask{
			PID:     uint(task.ID),
			Stage:   detail.GetStage(), //子任务分发到detail阶段
			URL:     fullURL,
			Title:   title,
			Content: jsonData,
		})
		if err != nil {
			return fmt.Errorf("JSON 编码失败: %w", err)
		}
	}
	if err := engine.GetDB().CreateInBatches(dataTasks, 100).Error; err != nil {
		return fmt.Errorf("批量新增任务失败: %w", err)
	}
	for _, dataTask := range dataTasks {
		err := engine.SubmitTask(
			&crawler.Task{
				ID:    int(dataTask.ID),
				PID:   task.ID,
				Stage: dataTask.Stage,
				URL:   dataTask.URL,
			})
		if err != nil {
			engine.GetLoggerSet().Engine.Error(fmt.Errorf("详情任务提交失败: %w; task ID: %d", err, dataTask.ID))
		}
	}
	engine.GetDB().Model(&models.CrawlerTask{}).Where("id = ?", task.ID).Update("status", models.TaskStatusSuccess)
	return nil
}
