package breakers

import (
	"testing"
	"time"
)

func TestAuditDistinguishesSiteStageAndUnverifiedForce(t *testing.T) {
	at := time.Unix(123, 0)
	for _, stage := range []string{"", "detail"} {
		resume, pause := ResumeEvent("operator", "fixture", stage, at), PauseEvent("operator", "fixture", stage, at)
		if resume.Table != "breaker" || resume.Action != "resume" || pause.Action != "pause" || resume.Operator != "operator" || resume.At != at {
			t.Fatal("action audit envelope changed")
		}
		for _, values := range []map[string]any{resume.Values, pause.Values} {
			if values["scope"] != "fixture" {
				t.Fatal(values)
			}
			if got, exists := values["stage"]; exists != (stage != "") || exists && got != stage {
				t.Fatal(values)
			}
		}
		if stage == "" && resume.Values["note"] != "请求重试整站恢复或解除普通人工暂停；不跳过自动验证" {
			t.Fatal("site resume lost its verification semantics")
		}
		if stage != "" && resume.Values["note"] != "手动放行被暂停的抓取（只放行这个阶段）" {
			t.Fatal("stage audit changed scope")
		}
	}
	forced := ForceResumeEvent("operator", "fixture", at)
	if forced.Action != "force_resume" || forced.Values["note"] != "人工强制解除保护；未验证健康，保留失败计数" {
		t.Fatal("forced release audit must not claim health verification")
	}
	forced.Values["scope"] = "changed"
	if ForceResumeEvent("operator", "fixture", at).Values["scope"] != "fixture" {
		t.Fatal("audit records share mutable values")
	}
}
