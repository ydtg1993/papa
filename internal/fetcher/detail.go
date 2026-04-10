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

type FetchDetail struct {
}

func (f *FetchDetail) GetStage() string {
	return "detail"
}

func (f *FetchDetail) FetchHandler(ctx context.Context, task *crawler.Task, engine *crawler.Engine) error {
	bw, err := engine.GetBrowserPool().Get(ctx)
	if err != nil {
		return err
	}
	defer engine.GetBrowserPool().Put(bw)
	// 1. 创建空白页面（不自动导航）
	page := bw.Browser.MustPage("")
	defer page.Close()
	fullURL := engine.GetConfig().Crawler.Target + task.URL
	if err := page.Timeout(10 * time.Second).Navigate(fullURL); err != nil {
		return err
	}
	page.MustWaitLoad()
	page.MustElement(".detail-right__volumes")

	elements, err := page.Elements("a.goto-chapter")
	if err != nil {
		return fmt.Errorf("未找到剧集链接: %w", err)
	}
	video := &FetchVideo{}
	seriesMap := make(map[string]string)
	downloadMap := make(map[string]bool)
	var dataTasks []*models.CrawlerTask
	for _, el := range elements {
		// 获取标题（title 属性或 span 内文本）
		var title string
		title = GetAttr(el, "title")
		if title == "" {
			continue // 没有标题的跳过
		}
		// 获取链接
		var href string
		href = GetAttr(el, "href")
		if href == "" {
			continue
		}

		// 存入映射
		seriesMap[title] = href
		downloadMap[href] = false

		dataTasks = append(dataTasks, &models.CrawlerTask{
			PID:   uint(task.ID),
			Stage: video.GetStage(), //子任务分发到video阶段
			URL:   href,
			Title: title,
		})
	}

	if len(seriesMap) == 0 {
		return fmt.Errorf("未提取到任何剧集")
	}
	//更新detail记录 增加剧集内容
	var detailRecord models.CrawlerTask
	engine.GetDB().Model(&models.CrawlerTask{}).
		Where("id = ?", task.ID).First(&detailRecord)
	var content models.DetailContent
	if len(detailRecord.Content) > 0 {
		if err := json.Unmarshal(detailRecord.Content, &content); err != nil {
			return fmt.Errorf("解析现有 detailRecord 失败: %w", err)
		}
	}
	content.Series = seriesMap
	content.Downloads = downloadMap
	newJSON, err := json.Marshal(content)
	if err != nil {
		return fmt.Errorf("序列化 detailRecord 失败: %w", err)
	}
	detailRecord.Content = newJSON
	engine.GetDB().Save(&detailRecord)
	//剧集入库
	if err := engine.GetDB().CreateInBatches(dataTasks, 100).Error; err != nil {
		return fmt.Errorf("批量新增任务失败: %w", err)
	}
	// 5. 批量提交子任务到下一阶段（video）
	for _, dataTask := range dataTasks {
		err := engine.SubmitTask(
			&crawler.Task{
				ID:    int(dataTask.ID),
				PID:   task.ID,
				Stage: dataTask.Stage,
				URL:   dataTask.URL,
			})
		if err != nil {
			subErr := fmt.Errorf("剧集任务提交失败: %w; task ID: %d \r", err, dataTask.ID)
			dataTask.Status = models.TaskStatusFailed
			dataTask.Error += subErr.Error()
			engine.GetDB().Save(dataTask)
		}
	}

	return nil
}

func GetAttr(el *rod.Element, name string) (val string) {
	val = ""
	if titlePtr, err := el.Attribute(name); err == nil && titlePtr != nil {
		val = strings.TrimSpace(*titlePtr)
	}
	return
}
