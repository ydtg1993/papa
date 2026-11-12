package app

import (
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/internal/sitedecl"
)

// 站点/阶段声明与规划的类型、纯校验/推导/解析、下载器覆盖自查与进程级登记表已下沉到
// internal/sitedecl（见 APP_STRUCTURE.md §3.3）。这里只保留公开兼容别名 + 落到引擎的装配方法。

// SiteSpec 一个站点的声明（Key 必填）。多站就是多个 SiteSpec。
type SiteSpec = sitedecl.SiteSpec

// StageSpec 一个阶段的完整声明：fetcher + 并发/队列/间隔/重试 + 有没有入口任务。
type StageSpec = sitedecl.StageSpec

// BreakerSpec 站点自己的熔断阈值（覆盖 crawler.breaker 的默认值）。
type BreakerSpec = sitedecl.BreakerSpec

// RetrySpec 重试声明（MaxAttempts / Backoff）。
type RetrySpec = sitedecl.RetrySpec

// RegisterSite 登记一个站点声明（在站点文件的 init() 里调）。
var RegisterSite = sitedecl.RegisterSite

// Sites 返回框架收集到的全部站点声明（顺序 = 文件名字典序）。
var Sites = sitedecl.Sites

// CheckSites 校验一批站点与阶段声明（不连库、不建池、不改状态）。
var CheckSites = sitedecl.CheckSites

// DescribeDownloadOverlays 列出各站点/阶段改过的下载器配置。
var DescribeDownloadOverlays = sitedecl.DescribeDownloadOverlays

// RegisterSites 注册站点与阶段。声明有问题**启动即失败**（与配置校验层同一个取向：
// 该在启动时炸掉的东西，别等跑到某条任务上才变成另一种行为）。
//
// 校验与推导在 sitedecl.PlanSites 里**一次做完**（纯函数，不碰引擎），这里只负责落地。两个附带好处：
// ① 有问题的声明**一个字节都不落地**（原来是一边注册一边炸，前面几个站已经进去了）；
// ② 同一个推导给 CheckSites 用 —— "不起库先自查一遍"与"真启动"看到的是同一套判据，不会漂。
func (a *App) RegisterSites(sites ...SiteSpec) {
	plans, err := sitedecl.PlanSites(a.Config, sites)
	if err != nil {
		panic(err)
	}
	for _, p := range plans {
		if p.Site.Key != "" {
			a.sites[p.Site.Key] = p.Site
			a.Engine.SetSite(p.Site)
			a.Engine.SetSiteDownloads(p.Site.Key, p.Downloads)
		}
		if p.Breaker != nil {
			a.Engine.SetSiteBreaker(p.Site.Key, *p.Breaker)
		}
		for _, sp := range p.Stages {
			if err := a.applyStagePlan(sp); err != nil {
				panic(err)
			}
		}
	}
}

// applyStagePlan 落地一条计划（注册阶段 + 打它带来的那句话）。
func (a *App) applyStagePlan(plan sitedecl.StagePlan) error {
	if err := a.Engine.AddStage(plan.Stage, plan.Config, plan.Fetcher, plan.Submit); err != nil {
		return err
	}
	if plan.Note == "" {
		return nil
	}
	if plan.Warn {
		a.Logger.Engine.Warn(plan.Note)
		return nil
	}
	a.Logger.Engine.Info(plan.Note)
	return nil
}

// Site 取某个站点的声明（BaseURL 等）。handler 里通常用 `engine.Site(task.Site)`。
func (a *App) Site(key string) (core.Site, bool) {
	s, ok := a.sites[key]
	return s, ok
}
