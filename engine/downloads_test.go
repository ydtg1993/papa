package engine

import (
	"testing"

	"github.com/ydtg1993/papa/v3/pkg/middleware/filedown"
	"github.com/ydtg1993/papa/v3/pkg/middleware/m3u8"
)

// 下载器覆盖层的解析：**阶段 > 站点 > 基座（框架默认那份）**；没任何覆盖的阶段直接用基座。
//
// 这是"配置只有两层"那条设计的落点：覆盖出来的实例是 `With` 出来的（配置独立、内部状态共享
// —— 共享那半边由 m3u8/filedown 包自己的测试钉住，这里钉的是"取到哪一份"）。
func TestDownloadOverlayResolution(t *testing.T) {
	e := stageDepEngine(t)
	e.InitDownloaders() // 框架在 Run 里做的同一件事
	base := e.GetM3U8()
	if base == nil {
		t.Fatal("InitDownloaders 之后基座就该在")
	}

	siteDir := "site-a"
	stageConcurrent := 9
	e.SetSiteDownloads("a", SiteDownloads{
		M3U8:     &m3u8.Overlay{OutputDir: &siteDir},
		FileDown: &filedown.Overlay{OutputDir: &siteDir},
	})

	cfgA1 := stageDepCfg
	cfgA1.Site = "a"
	cfgA2 := stageDepCfg
	cfgA2.Site = "a"
	cfgA2.M3U8 = &m3u8.Overlay{MaxConcurrent: &stageConcurrent}
	cfgB := stageDepCfg
	cfgB.Site = "b"

	e.AddStage("a1", cfgA1, &recordingFetcher{stage: "a1"}, nil)
	e.AddStage("a2", cfgA2, &recordingFetcher{stage: "a2"}, nil)
	e.AddStage("b1", cfgB, &recordingFetcher{stage: "b1"}, nil)
	e.ApplyRegisterStage()

	// 阶段 a1：只有站点层覆盖 → 站点那份（目录变了、其余还是基座的值）
	a1 := e.M3U8ForTask(&Task{Stage: "a1"})
	if a1 == nil || a1 == base {
		t.Fatal("站点层覆盖过的阶段应当拿到一份覆盖实例，而不是基座那份")
	}
	if got := a1.Config().OutputDir; got != siteDir {
		t.Fatalf("站点层的 OutputDir 没生效：%q", got)
	}
	if got, want := a1.Config().MaxConcurrent, base.Config().MaxConcurrent; got != want {
		t.Fatalf("站点没改的项应当保留基座的值，实得 %d want %d", got, want)
	}
	if got := e.FiledownForStage("a1").Config().OutputDir; got != siteDir {
		t.Fatalf("文件下载器同样要按站点解析：%q", got)
	}

	// 阶段 a2：阶段层再叠一层 → 站点层与阶段层同时生效（阶段赢）
	a2 := e.M3U8ForStage("a2")
	if got := a2.Config().MaxConcurrent; got != stageConcurrent {
		t.Fatalf("阶段层的 MaxConcurrent 没生效：%d", got)
	}
	if got := a2.Config().OutputDir; got != siteDir {
		t.Fatalf("阶段没写的项应当保留站点那层的值：%q", got)
	}

	// 阶段 b1：一句都没覆盖 → 直接用基座那份（不额外建实例）
	if e.M3U8ForStage("b1") != base {
		t.Fatal("没有任何覆盖的阶段应当直接用基座那份")
	}
	if e.FiledownForStage("b1") != e.GetFiledown() {
		t.Fatal("文件下载器同理")
	}

	// 取用入口：按任务取与按阶段取是同一份；任务为空 / 阶段没覆盖都回落基座
	if e.M3U8ForTask(&Task{Stage: "a2"}) != a2 {
		t.Fatal("M3U8ForTask 应当按 task.Stage 解析到同一份")
	}
	if e.M3U8ForTask(nil) != base || e.M3U8ForStage("ghost") != base {
		t.Fatal("取不到阶段时应当回落基座那份")
	}
	// 站点层单独一份（给只知道站点的调用方）
	if got := e.M3U8ForSite("a").Config().OutputDir; got != siteDir {
		t.Fatalf("M3U8ForSite 没生效：%q", got)
	}
	if e.M3U8ForSite("ghost") != base {
		t.Fatal("站点没覆盖过时应当回落基座那份")
	}
}

// 手工拼起来的引擎（没调 InitDownloaders，正常启动路径不会这样）：取用返回 nil，
// 调用方按老样子判空 —— 与 GetM3U8 / GetFiledown 同语义。
func TestDownloadOverlayWithoutBase(t *testing.T) {
	dir := "somewhere"
	e := stageDepEngine(t)
	e.SetSiteDownloads("a", SiteDownloads{M3U8: &m3u8.Overlay{OutputDir: &dir}})
	cfg := stageDepCfg
	cfg.Site = "a"
	cfg.M3U8 = &m3u8.Overlay{OutputDir: &dir}
	e.AddStage("a1", cfg, &recordingFetcher{stage: "a1"}, nil)
	e.ApplyRegisterStage()

	if got := e.M3U8ForStage("a1"); got != nil {
		t.Fatalf("没有基座时不该凭空造一份出来，实得 %+v", got)
	}
	if got := e.FiledownForTask(&Task{Stage: "a1"}); got != nil {
		t.Fatalf("文件下载器同理，实得 %+v", got)
	}
}

// 基座由框架备好：不写任何声明也有得用（新项目开箱即用，不必在 main.go 里手写两行），且幂等。
func TestInitDownloadersProvidesBase(t *testing.T) {
	e := stageDepEngine(t)
	if e.GetFiledown() != nil || e.GetM3U8() != nil {
		t.Fatal("新引擎刚开始两个下载器都该是 nil")
	}
	e.InitDownloaders()
	m3u8Base, filedownBase := e.GetM3U8(), e.GetFiledown()
	if m3u8Base == nil || filedownBase == nil {
		t.Fatal("InitDownloaders 之后两个基座都该在")
	}
	e.InitDownloaders()
	if e.GetM3U8() != m3u8Base || e.GetFiledown() != filedownBase {
		t.Fatal("重复 InitDownloaders 不该把已经建好的基座换掉")
	}
}
