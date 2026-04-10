package fetcher

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/go-rod/rod"
	"github.com/ydtg1993/papa/internal/crawler"
	"github.com/ydtg1993/papa/internal/models"
	"strconv"
	"strings"
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
	fullURL := engine.GetConfig().Crawler.Target + task.URL

	//================监听m3u8================/
	// 用于存储捕获到的 m3u8 URL
	var m3u8URL string

	// 方式一：使用 Router 拦截请求（推荐，可精确匹配）
	router := page.HijackRequests()
	defer router.Stop()

	// 拦截所有请求，或者只拦截 .m3u8 的请求
	router.MustAdd("*", func(ctx *rod.Hijack) {
		// 检查请求 URL 是否以 .m3u8 结尾或包含 .m3u8?
		if strings.Contains(ctx.Request.URL().Path, ".m3u8") {
			m3u8URL = ctx.Request.URL().String()
			// 可选：记录日志或提前中断页面加载
		}
		// 放行请求，不影响页面正常加载
		ctx.ContinueRequest(nil)
	})

	go router.Run()

	// 方式二：使用 EachEvent 监听网络响应（适合后期捕获）
	// wait := page.EachEvent(func(e *rod.EventFetchResponseReceived) {
	//     if strings.Contains(e.Request.URL, ".m3u8") {
	//         m3u8URL = e.Request.URL
	//     }
	// })
	// defer wait()

	// 导航到目标页面
	if err := page.Timeout(10 * time.Second).Navigate(fullURL); err != nil {
		return err
	}
	page.MustWaitLoad()
	// 额外等待一段时间，让异步发起的 m3u8 请求有机会被捕获
	time.Sleep(3 * time.Second)
	if m3u8URL == "" {
		return fmt.Errorf("task ID:%d  m3u8未捕获到", task.ID)
	}

	download := engine.GetM3U8().Download(ctx, m3u8URL, strconv.Itoa(task.ID), "")
	if download.Error != nil {
		return fmt.Errorf("视频下载失败: %w", download.Error)
	}

	var parentTask models.CrawlerTask
	if err := engine.GetDB().First(&parentTask, task.PID).Error; err != nil {
		return fmt.Errorf("查找父任务失败: %w", err)
	}
	// 解析父任务的 Content 为 DetailContent
	var detailContent models.DetailContent
	if len(parentTask.Content) > 0 {
		if err := json.Unmarshal(parentTask.Content, &detailContent); err != nil {
			return fmt.Errorf("解析父任务 Content 失败: %w", err)
		}
	}
	// 将当前视频 URL 标记为已下载
	detailContent.SeriesContent.Downloads[task.URL] = true
	engine.GetDB().Save(&detailContent)

	videoContent := models.VideoContent{
		Source: m3u8URL,
		Dir:    download.OutputFile, // 可根据需要设置
	}
	contentJSON, _ := json.Marshal(videoContent)
	engine.GetDB().Create(models.CrawlerTask{
		PID:     parentTask.ID,
		URL:     m3u8URL,
		Stage:   "", //最终任务不提交队列
		Status:  models.TaskStatusSuccess,
		Content: contentJSON,
	})
	return nil
}
