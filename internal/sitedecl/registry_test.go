package sitedecl

import (
	"strings"
	"testing"
)

// 站点文件在 init 里登记，框架收集；`Sites()` 给副本、顺序稳定（= 文件名字典序）。
func TestRegisterSiteCollectsInOrder(t *testing.T) {
	resetRegisteredSites()
	t.Cleanup(resetRegisteredSites)

	RegisterSite(SiteSpec{Key: "b", BaseURL: "https://b.example"})
	RegisterSite(SiteSpec{Key: "a", BaseURL: "https://a.example"})

	got := Sites()
	if len(got) != 2 || got[0].Key != "b" || got[1].Key != "a" {
		t.Fatalf("登记顺序应当保持，实得 %+v", got)
	}
	// 返回副本：改它不该影响登记表
	got[0].Key = "tampered"
	if again := Sites(); again[0].Key != "b" {
		t.Fatalf("Sites() 应返回副本，实得 %+v", again)
	}
}

// 同一个站点键登记两次（多半是两个文件写了同一个 Key）→ 注册时报错并点名。
func TestValidateSitesRejectsDuplicateKey(t *testing.T) {
	sites := []SiteSpec{{Key: "a"}, {Key: "b"}, {Key: "a"}}
	err := validateSites(sites)
	if err == nil {
		t.Fatal("重复的站点键应当报错")
	}
	if !strings.Contains(err.Error(), `"a"`) || !strings.Contains(err.Error(), "第 3 个") {
		t.Fatalf("报错要点名是哪个键、第几个：%v", err)
	}
	// 缺 Key 直接拒掉：那种声明会落进"默认 scope"，而新任务一律要求带站点 ——
	// 注册得上却永远收不到新任务，是个陷阱（见 validateSites）。
	err = validateSites([]SiteSpec{{Key: "a"}, {}})
	if err == nil || !strings.Contains(err.Error(), "缺 Key") {
		t.Fatalf("缺 Key 的声明应当报错：%v", err)
	}
}
