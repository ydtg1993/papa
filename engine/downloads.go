package engine

import (
	"github.com/ydtg1993/papa/v4/engine/internal/fetchruntime"
	"github.com/ydtg1993/papa/v4/pkg/middleware"
	"github.com/ydtg1993/papa/v4/pkg/middleware/filedown"
	"github.com/ydtg1993/papa/v4/pkg/middleware/m3u8"
)

// 本文件是**下载器实例的解析与取用**：基座（框架默认）→ 站点一层覆盖 → 阶段一层覆盖。
//
// 三段话把设计说清（其余地方照这个来）：
//
//  1. **配置面只有两层：站点与阶段**（`SiteSpec.M3U8` / `StageSpec.M3U8`，`FileDown` 同理）。
//     基座是框架按 `DefaultConfig()` 备好的那份 —— 没有"全局那层配置"可写（`SetM3U8` /
//     `SetFiledown` 已删除）：要么在声明里按站点/阶段写清楚，要么在 fetcher 里自己造一份，
//     没有第三种下载行为覆盖。进程资源预算通过 SetDownloadResources 独立装配，不能由 Overlay 放宽。
//     没有任何覆盖的阶段**不建实例**，直接用基座那份。
//  2. **覆盖出来的实例用 `With` 建**：配置独立、**内部状态共享**（同一目标文件的串行锁、密钥缓存、
//     代理连接池、消息队列都是同一份）。所以"每个阶段一份配置"不等于"每个阶段一个孤岛"。
//  3. **想临时一份**（只给这一次调用）就自己在 fetcher 里造：`M3U8ForTask(task).With(ov)`，
//     或者干脆 `m3u8.NewDownloader(&cfg)` —— 后者是孤岛（谁也不共享），代价自负，框架不拦。

// SiteDownloads 站点级的下载器覆盖层（`App.RegisterSites` 调）。
//
// 为什么不塞进 `core.Site`：core 是**只 import 标准库**的叶子包（见它的包注释），把 m3u8/filedown
// 的配置类型放进去会让依赖图整个倒过来。所以站点级的"引擎侧配置"（下载器覆盖）走这类显式 Set。
type SiteDownloads = fetchruntime.SiteDownloads

// SetSiteDownloads 登记某站点的下载器覆盖层。site 为空是非法参数 —— 站点是必填维度，
// 没有「未归属」那一档（声明要有 Key，任务要有 site）。
// 必须在 `ApplyRegisterStage` 之前调（之后解析一次，不再变）。
// 覆盖项按登记时的值复制；启动前要更新时显式重新登记。
func (e *Engine) SetSiteDownloads(site string, d SiteDownloads) { e.downloads.Register(site, d) }

// resolveStageDownloads 启动期解析每个阶段该用哪份下载器（`ApplyRegisterStage` 里调一次）。
//
// 只在"确实有覆盖"时才 `With` 出一份：没覆盖的阶段不进 map，取用时回落到全局那份。
// 阶段那份按**阶段键**（`core.StageKey`）存：两个站的同名阶段可以各有各的覆盖。
func (e *Engine) resolveStageDownloads() {
	stages := make([]fetchruntime.StageDownloads, 0, len(e.stages))
	for _, info := range e.stages {
		stages = append(stages, fetchruntime.StageDownloads{Site: info.config.Site, Stage: info.stage, SiteDownloads: SiteDownloads{M3U8: info.config.M3U8, FileDown: info.config.FileDown}})
	}
	e.downloads.Resolve(stages, e.m3u8, e.filedown)
}

// SetDownloadResources configures per-download limits and a shared resource
// budget before InitDownloaders/Run. nil budget selects finite defaults.
func (e *Engine) SetDownloadResources(limits middleware.DownloadLimits, budget *middleware.DownloadBudget) error {
	return e.downloads.ConfigureResources(limits, budget)
}

// InitDownloaders 备好两个下载器的**基座**（框架在 Run 里调，紧挨着 SetBrowserPool / SetHTMLClient）。
//
// 为什么由框架备：它们与 `html` / `browser` 那两个客户端是同一类"框架自带组件"，而那两个都是
// 按配置自动建的 —— 下载器以前默认是 nil，于是每个项目都得在 main.go 里手写两行。现在不写就有
// `DefaultConfig()` 那份；要按站点/阶段各配一份，写在声明里（**没有全局那层**）。
// 重复调用是幂等的（已经建过就不动）。
func (e *Engine) InitDownloaders() {
	limits, budget := e.downloads.Resources()
	if e.filedown == nil {
		cfg := filedown.DefaultConfig()
		cfg.ProxyManager = e.proxy
		if e.cfg != nil {
			cfg.Network = e.cfg.Network
		}
		e.filedown = filedown.NewDownloader(cfg).WithResources(limits, budget)
	}
	if e.m3u8 == nil {
		cfg := m3u8.DefaultConfig()
		cfg.ProxyManager = e.proxy
		if e.cfg != nil {
			cfg.Network = e.cfg.Network
		}
		e.m3u8 = m3u8.NewDownloader(cfg).WithResources(limits, budget)
	}
}

/* ---------- 取用：fetcher 侧一行拿到"这一阶段该用的那个" ---------- */

// M3U8ForTask 取本任务所属阶段该用的 m3u8 下载器：**阶段覆盖 > 站点覆盖 > 基座那份**。
// 正常启动路径上基座一定有（`Run` 里 `InitDownloaders`），所以不会返回 nil；
// 手工拼起来的引擎（测试）没调它时可能为 nil —— 与 `GetM3U8()` 同语义，调用方按老样子判空。
//
// 这是 fetcher 里该用的入口（`engine.GetM3U8()` 仍是全局那份，老代码不用改）：
//
//	dl := engine.M3U8ForTask(task)
//	res := dl.Download(ctx, url, dir, name, m3u8.OptionsFromRequest(ctx, task.URL))
func (e *Engine) M3U8ForTask(task *Task) *m3u8.Downloader {
	if task == nil {
		return e.m3u8
	}
	return e.M3U8ForStage(task.Site, task.Stage)
}

// M3U8ForStage 按**站点 + 阶段**取（cron / 非任务上下文用）。
// 两个参数都要给：阶段名只在本站内唯一，"某个阶段名"在多站项目里不指向唯一一份配置。
func (e *Engine) M3U8ForStage(site, stage string) *m3u8.Downloader {
	return e.downloads.M3U8Stage(site, stage, e.m3u8)
}

// M3U8ForSite 按站点取"站点层那一份"（不叠阶段层）；没覆盖过就是全局那份。
func (e *Engine) M3U8ForSite(site string) *m3u8.Downloader { return e.downloads.M3U8Site(site, e.m3u8) }

// FiledownForTask 取本任务所属阶段该用的文件下载器（解析规则与 M3U8ForTask 相同）。
func (e *Engine) FiledownForTask(task *Task) *filedown.Downloader {
	if task == nil {
		return e.filedown
	}
	return e.FiledownForStage(task.Site, task.Stage)
}

// FiledownForStage 按**站点 + 阶段**取（与 M3U8ForStage 同一套参数：阶段名在本站内才唯一）。
func (e *Engine) FiledownForStage(site, stage string) *filedown.Downloader {
	return e.downloads.FileStage(site, stage, e.filedown)
}

// FiledownForSite 按站点取"站点层那一份"；没覆盖过就是全局那份。
func (e *Engine) FiledownForSite(site string) *filedown.Downloader {
	return e.downloads.FileSite(site, e.filedown)
}
