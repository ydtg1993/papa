package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Exercise the public registration path and HTTP response, including old callers
// that only supply Group. Sections must never rename or discard business groups.
func TestCustomPageSectionsHTTP(t *testing.T) {
	pages := []Page{
		{Key: "flow-page", Label: "任务", Group: "业务甲", Section: "flow"},
		{Key: "monitor-page", Label: "监控", Group: "业务甲", Section: "monitor"},
		{Key: "data-page", Label: "数据", Group: "业务乙", Section: "data"},
		{Key: "system-page", Label: "设置", Group: "业务乙", Section: "system"},
		{Key: "legacy-page", Label: `旧页 <>&"`, Group: `旧组 <>&"`},
		{Key: "bare-page"},
		{Key: "unknown-page", Label: "未知分类", Group: "保留原组", Section: "future"},
		{Key: "invalid-page", Group: "保留原组", Section: `<script>bad</script>`},
	}
	a := &App{}
	for _, p := range pages {
		p.Script = `Papa.page("` + p.Key + `", function(){});`
		a.UsePage(p)
	}
	mux := http.NewServeMux()
	a.mountCustomRoutes(mux, nil)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/pages", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	var got struct {
		Pages []map[string]string `json:"pages"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Pages) != len(pages) {
		t.Fatalf("pages = %v, want %d entries", got.Pages, len(pages))
	}
	wantSections := []string{"flow", "monitor", "data", "system", "data", "data", "data", "data"}
	for i, p := range pages {
		t.Run(p.Key, func(t *testing.T) {
			label, group := p.Label, p.Group
			if label == "" {
				label = p.Key
			}
			if group == "" {
				group = "General"
			}
			for field, want := range map[string]string{"key": p.Key, "label": label, "group": group, "section": wantSections[i]} {
				if value := got.Pages[i][field]; value != want {
					t.Errorf("%s = %q, want %q", field, value, want)
				}
			}
			if _, leaked := got.Pages[i]["script"]; leaked {
				t.Error("page metadata must not include the rendering script")
			}
		})
	}
}
