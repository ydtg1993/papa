// Package papa 是一个可复用的爬虫框架。
// 在 main.go 中 import 本包，用 New 创建应用、RegisterStage 注册各阶段 fetcher 即可运行。
//
// 本文件是**应用门面**：App 及其初始化选项、后台扩展点（表格页 / 自定义页 / 路由）。
// 任务与引擎见 task.go，错误与告警见 errors.go。
package papa

import (
	"github.com/ydtg1993/papa/v2/config"
	"github.com/ydtg1993/papa/v2/internal/app"
)

// App 应用容器，封装配置、日志、数据库、爬虫引擎。
type App = app.App

// Option 应用初始化选项，传给 New。
type Option = app.Option

// Router 业务后台路由表，在 App.UseRouter 的回调里声明路径、方法与中间件。
type Router = app.Router

// Middleware 一个 HTTP 中间件：func(http.Handler) http.Handler。
// 业务中间件与后台鉴权（框架注入的那个）是同一个类型，可以串成一条链。
type Middleware = app.Middleware

// Page 一个自定义后台页，传给 App.UsePage。
type Page = app.Page

// Config 全局配置。
type Config = config.Config

// WithConfigPath 指定配置文件路径（缺省读 PAPA_CONFIG 环境变量，再回退 configs/config.yaml）
var WithConfigPath = app.WithConfigPath

// WithModels 追加需要建表的业务模型；框架自带的表不用登记。
// 它们会在 App.Migrate()（脚手架里的 `make migrate`）时一起建。
var WithModels = app.WithModels

// New 创建应用实例，完成配置加载、日志、数据库、引擎的初始化。
func New(opts ...Option) (*App, error) {
	return app.NewApp(opts...)
}
