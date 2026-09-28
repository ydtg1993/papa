// Package papa 是一个可复用的爬虫框架。
// 在 main.go 中 import 本包，用 New 创建应用、RegisterStage 注册各阶段 fetcher 即可运行。
package papa

import (
	"github.com/ydtg1993/papa/v2/app"
	"github.com/ydtg1993/papa/v2/config"
	"github.com/ydtg1993/papa/v2/crawler"
	"github.com/ydtg1993/papa/v2/models"
)

// App 应用容器，封装配置、日志、数据库、爬虫引擎。
type App = app.App

// Option 应用初始化选项，传给 New。
type Option = app.Option

// WithConfigPath 指定配置文件路径（缺省读 PAPA_CONFIG 环境变量，再回退 configs/config.yaml）。
var WithConfigPath = app.WithConfigPath

// WithModels 追加需要自动迁移的用户模型（框架默认迁移 CrawlerTask）。
var WithModels = app.WithModels

// Fetcher 爬虫业务逻辑接口。
type Fetcher = crawler.Fetcher

// Task 任务结构。
type Task = crawler.Task

// Engine 爬虫引擎。
type Engine = crawler.Engine

// StageConfig 阶段配置。
type StageConfig = crawler.StageConfig

// Config 全局配置。
type Config = config.Config

// CrawlerTask 任务表模型（fetcher 写结果时用 db.Model(&papa.CrawlerTask{}) 定位）。
type CrawlerTask = models.CrawlerTask

// New 创建应用实例，完成配置加载、日志、数据库、引擎的初始化。
func New(opts ...Option) (*App, error) {
	return app.NewApp(opts...)
}
