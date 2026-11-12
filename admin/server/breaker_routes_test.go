package server

import (
	"github.com/ydtg1993/papa/v4/core"
	"io/fs"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestBreakerEndpoints(t *testing.T) {
	paused := map[string]bool{"sitea": true, "siteb": false} // sitea 被闸住，siteb 正常
	var resumedBy []string
	var stageResumed []string

	status := func(site string) core.BreakerStatus {
		return core.BreakerStatus{
			Enabled: true, Site: site, Paused: paused[site], PausedAt: time.Now(),
			Reason: "窗口内终态失败数达到阈值", Stage: site + "-catalog",
			Failures: 50, Threshold: 50, Window: 5 * time.Minute, InWindow: 12,
		}
	}
	newMon := func() *Monitor {
		return NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{VerifyToken: allowUnrestrictedTestRequests,
			BreakerStatuses: func() map[string]core.BreakerStatus {
				return map[string]core.BreakerStatus{"sitea": status("sitea"), "siteb": status("siteb")}
			},
			ResumeBreaker: func(site, stage string) bool {
				if !paused[site] {
					return false
				}
				if stage != "" {
					// 阶段级放行：只清那一个阶段那条（这里记一笔就够断言）
					stageResumed = append(stageResumed, site+"/"+stage)
					return true
				}
				paused[site] = false
				return true
			},
			OnBreakerResume: func(operator, site, stage string) {
				resumedBy = append(resumedBy, operator+"@"+site+"/"+stage)
			},
		})
	}
	reset := func() {
		paused = map[string]bool{"sitea": true, "siteb": false}
		resumedBy = nil
		stageResumed = nil
	}

	t.Run("GET 返回各站点的状态，按站点名排序", func(t *testing.T) {
		reset()
		rr := serve(newMon(), http.MethodGet, "/api/breaker", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
		}
		body := decodeJSON(t, rr)
		list, ok := body["breakers"].([]any)
		if !ok || len(list) != 2 {
			t.Fatalf("breakers = %v", body["breakers"])
		}
		first, _ := list[0].(map[string]any)
		if first["site"] != "sitea" || first["paused"] != true || first["stage"] != "sitea-catalog" {
			t.Fatalf("第一条应当是 sitea：%v", first)
		}
		if first["in_window"] != float64(12) {
			t.Fatalf("in_window = %v, want 12（实时值，和触发快照的 failures 不是一回事）", first["in_window"])
		}
		second, _ := list[1].(map[string]any)
		if second["site"] != "siteb" || second["paused"] != false {
			t.Fatalf("第二条应当是 siteb：%v", second)
		}
	})

	t.Run("状态也随 /api/monitor 一起返回", func(t *testing.T) {
		reset()
		rr := serve(newMon(), http.MethodGet, "/api/monitor", nil)
		body := decodeJSON(t, rr)
		list, ok := body["breakers"].([]any)
		if !ok || len(list) != 2 {
			t.Fatalf("monitor 响应里应带 breakers 数组，实得 %v", body["breakers"])
		}
	})

	t.Run("不点名 = 全部放行，回调逐站点收到操作人", func(t *testing.T) {
		reset()
		rr := serve(newMon(), http.MethodPost, "/api/breaker/resume", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
		}
		body := decodeJSON(t, rr)
		if body["resumed"] != true {
			t.Fatalf("resumed = %v, want true", body["resumed"])
		}
		if scopes, _ := body["resumed_scopes"].([]any); len(scopes) != 1 || scopes[0] != "sitea" {
			t.Fatalf("resumed_scopes = %v，应当只有被闸住的 sitea", body["resumed_scopes"])
		}
		// 测试显式注入的校验桩返回空操作人 —— 这里要的是"回调被调了一次、且带上了站点"。
		if len(resumedBy) != 1 || resumedBy[0] != "@sitea/" {
			t.Fatalf("OnBreakerResume 收到 %v，want [@sitea/]（操作人@站点/阶段）", resumedBy)
		}
	})

	t.Run("点名站点只放行那一个", func(t *testing.T) {
		reset()
		paused["siteb"] = true // 两个都被闸住
		rr := serve(newMon(), http.MethodPost, "/api/breaker/resume?scope=siteb", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
		}
		if scopes, _ := decodeJSON(t, rr)["resumed_scopes"].([]any); len(scopes) != 1 || scopes[0] != "siteb" {
			t.Fatalf("resumed_scopes = %v，应当只有 siteb", scopes)
		}
		if !paused["sitea"] {
			t.Fatal("sitea 不该被顺手放行")
		}
	})

	t.Run("未知 scope：400 而不是静默放行全部", func(t *testing.T) {
		reset()
		rr := serve(newMon(), http.MethodPost, "/api/breaker/resume?scope=ghost", nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rr.Code)
		}
		if !paused["sitea"] {
			t.Fatal("报错时不该把 sitea 放行了")
		}
	})

	t.Run("本来就没暂停：resumed=false 而不是报错", func(t *testing.T) {
		reset()
		paused["sitea"] = false
		rr := serve(newMon(), http.MethodPost, "/api/breaker/resume", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（重复点击恢复不该看起来像失败）", rr.Code)
		}
		body := decodeJSON(t, rr)
		if body["resumed"] != false {
			t.Fatalf("resumed = %v, want false", body["resumed"])
		}
		if len(resumedBy) != 0 {
			t.Fatal("没真的放行就不该记操作日志")
		}
	})

	t.Run("GET 不接受写方法", func(t *testing.T) {
		if rr := serve(newMon(), http.MethodPost, "/api/breaker", nil); rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", rr.Code)
		}
	})
	t.Run("点名 stage 放行：交给宿主的也是那个阶段", func(t *testing.T) {
		reset()
		rr := serve(newMon(), http.MethodPost, "/api/breaker/resume", strings.NewReader(`{"scope":"sitea","stage":"catalog"}`))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
		}
		if body := decodeJSON(t, rr); body["resumed"] != true || body["stage"] != "catalog" {
			t.Fatalf("回执应当说清是哪个阶段：%v", body)
		}
		if len(stageResumed) != 1 || stageResumed[0] != "sitea/catalog" {
			t.Fatalf("应当把 stage 交给宿主：%v", stageResumed)
		}
		if len(resumedBy) != 1 || resumedBy[0] != "@sitea/catalog" {
			t.Fatalf("审计要带上阶段：%v", resumedBy)
		}
	})

}

func TestBreakerPauseEndpoint(t *testing.T) {
	paused := map[string]bool{"sitea": false, "siteb": false}
	var pausedBy []string
	var stagePaused []string
	newMon := func() *Monitor {
		return NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{VerifyToken: allowUnrestrictedTestRequests,
			BreakerStatuses: func() map[string]core.BreakerStatus {
				return map[string]core.BreakerStatus{"sitea": {Site: "sitea"}, "siteb": {Site: "siteb"}}
			},
			PauseBreaker: func(site, stage string) bool {
				if paused[site] {
					return false
				}
				if stage != "" {
					stagePaused = append(stagePaused, site+"/"+stage)
					return true
				}
				paused[site] = true
				return true
			},
			OnBreakerPause: func(operator, site, stage string) {
				pausedBy = append(pausedBy, operator+"@"+site+"/"+stage)
			},
		})
	}
	reset := func() {
		paused = map[string]bool{"sitea": false, "siteb": false}
		pausedBy = nil
		stagePaused = nil
	}

	t.Run("点名暂停：只停那一站，并记一条审计", func(t *testing.T) {
		reset()
		rr := serve(newMon(), http.MethodPost, "/api/breaker/pause", strings.NewReader(`{"scope":"sitea"}`))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
		}
		if body := decodeJSON(t, rr); body["paused"] != true {
			t.Fatalf("paused = %v, want true", body["paused"])
		}
		if !paused["sitea"] || paused["siteb"] {
			t.Fatalf("只该停 sitea：%v", paused)
		}
		// 测试请求没走鉴权中间件，操作人是空串 —— 这里要的是"回调被调了一次、且带上了站点"
		if len(pausedBy) != 1 || pausedBy[0] != "@sitea/" {
			t.Fatalf("OnBreakerPause 收到 %v，want [@sitea/]（操作人@站点/阶段）", pausedBy)
		}
	})

	t.Run("本来就在暂停态：paused=false 且不记审计", func(t *testing.T) {
		reset()
		paused["sitea"] = true
		rr := serve(newMon(), http.MethodPost, "/api/breaker/pause", strings.NewReader(`{"scope":"sitea"}`))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（重复点击不该看起来像失败）", rr.Code)
		}
		if body := decodeJSON(t, rr); body["paused"] != false {
			t.Fatalf("paused = %v, want false", body["paused"])
		}
		if len(pausedBy) != 0 {
			t.Fatalf("没有状态变更就不该记审计：%v", pausedBy)
		}
	})

	t.Run("不点名：400（与 resume 相反：不点名不会停掉全部）", func(t *testing.T) {
		reset()
		rr := serve(newMon(), http.MethodPost, "/api/breaker/pause", nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rr.Code)
		}
		if paused["sitea"] || paused["siteb"] {
			t.Fatalf("400 时谁都不该被停：%v", paused)
		}
	})

	t.Run("未知站点：400", func(t *testing.T) {
		reset()
		rr := serve(newMon(), http.MethodPost, "/api/breaker/pause", strings.NewReader(`{"scope":"ghost"}`))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rr.Code)
		}
	})

	t.Run("点名 stage：只停那一个阶段，整站状态不动", func(t *testing.T) {
		reset()
		rr := serve(newMon(), http.MethodPost, "/api/breaker/pause", strings.NewReader(`{"scope":"sitea","stage":"catalog"}`))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
		}
		body := decodeJSON(t, rr)
		if body["paused"] != true || body["stage"] != "catalog" {
			t.Fatalf("回执应当说清是哪个阶段：%v", body)
		}
		if len(stagePaused) != 1 || stagePaused[0] != "sitea/catalog" {
			t.Fatalf("应当把 stage 交给宿主：%v", stagePaused)
		}
		if paused["sitea"] {
			t.Fatal("点名 stage 时不该把整站也停掉")
		}
		if len(pausedBy) != 1 || pausedBy[0] != "@sitea/catalog" {
			t.Fatalf("审计要带上阶段：%v", pausedBy)
		}
	})

	t.Run("给了 stage 却没给 scope：400（阶段属于某个站）", func(t *testing.T) {
		reset()
		rr := serve(newMon(), http.MethodPost, "/api/breaker/pause", strings.NewReader(`{"stage":"catalog"}`))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rr.Code)
		}
		if len(stagePaused) != 0 {
			t.Fatalf("400 时不该动任何东西：%v", stagePaused)
		}
	})

	t.Run("没配回调：404", func(t *testing.T) {
		reset()
		m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{VerifyToken: allowUnrestrictedTestRequests})
		rr := serve(m, http.MethodPost, "/api/breaker/pause", strings.NewReader(`{"scope":"a"}`))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rr.Code)
		}
	})
}

func TestResumeCrawlingRejectsStageWithoutSite(t *testing.T) {
	js, err := fs.ReadFile(staticFS, "static/mo.js")
	if err != nil {
		t.Fatalf("read mo.js: %v", err)
	}
	if !strings.Contains(string(js), "if (stage && !site)") {
		t.Error("mo.js 的 resumeCrawling 没有拦住「有 stage 没 site」的调用 —— 会发成 {}、被服务端当成全部放行")
	}
}
