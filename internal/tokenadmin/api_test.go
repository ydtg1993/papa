package tokenadmin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// identity 代替监控后台的「白名单 + 令牌」中间件：那层由宿主包住（mon.Auth），
// 这里只测接口本身的行为。
func identity(h http.Handler) http.Handler { return h }

func newTestAPI() (*MemStore, *http.ServeMux, *[]Event) {
	store := NewMemStore()
	events := &[]Event{}
	mux := http.NewServeMux()
	NewAPI(store, func(ev Event) { *events = append(*events, ev) }, nil).Register(mux, identity)
	return store, mux, events
}

func do(t *testing.T, mux *http.ServeMux, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON（%d）：%s", w.Code, w.Body.String())
	}
	return out
}

// 新增：明文只在这次响应里回一次，且拿它当场就能通过校验（演示里"新建完拿去登录"）。
func TestCreateReturnsWorkingToken(t *testing.T) {
	store, mux, events := newTestAPI()

	w := do(t, mux, http.MethodPost, "/api/tokens", `{"operator":"张三","note":"运维机"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("新增应返回 200，得到 %d：%s", w.Code, w.Body.String())
	}
	body := decode(t, w)
	token, _ := body["token"].(string)
	if len(token) != 64 {
		t.Fatalf("明文令牌应是 64 个字符的十六进制串，得到 %q", token)
	}
	if id := body["id"]; id == nil || id.(float64) == 0 {
		t.Fatalf("响应应带上新建令牌的 id，得到 %v", body)
	}

	if op, ok := store.Verify(token); !ok || op != "张三" {
		t.Fatalf("新建的令牌应当场可用，得到 (%q, %v)", op, ok)
	}
	// 库里存的是哈希，不是明文
	rows, _ := store.List()
	if len(rows) != 1 || !rows[0].Enabled {
		t.Fatalf("应有一条启用中的令牌，得到 %+v", rows)
	}

	// 列表接口不能把哈希或明文漏出去
	listW := do(t, mux, http.MethodGet, "/api/tokens", "")
	var list struct {
		Tokens []map[string]any `json:"tokens"`
	}
	if err := json.Unmarshal(listW.Body.Bytes(), &list); err != nil {
		t.Fatalf("列表响应不是 JSON：%s", listW.Body.String())
	}
	if len(list.Tokens) != 1 {
		t.Fatalf("列表应有 1 行，得到 %d", len(list.Tokens))
	}
	for _, bad := range []string{"token", "token_hash", "TokenHash", "hash"} {
		if _, ok := list.Tokens[0][bad]; ok {
			t.Errorf("列表里不该出现字段 %q：%s", bad, listW.Body.String())
		}
	}

	// 审计：记了事、记了人、记了行，但**不记明文**
	if len(*events) != 1 {
		t.Fatalf("应回调一次审计，得到 %d 次", len(*events))
	}
	ev := (*events)[0]
	if ev.Action != "create" || ev.ID == 0 || ev.Table != TableKey {
		t.Errorf("审计事件不对：%+v", ev)
	}
	if ev.Values["operator"] != "张三" {
		t.Errorf("审计里应留下操作人：%+v", ev.Values)
	}
	if b, _ := json.Marshal(ev.Values); strings.Contains(string(b), token) {
		t.Errorf("审计的提交值里出现了明文令牌：%s", b)
	}
}

// 操作人为空：400 + 服务端的话原样回给前端，且没有落任何行。
func TestAPICreateRejectsEmptyOperator(t *testing.T) {
	store, mux, events := newTestAPI()

	w := do(t, mux, http.MethodPost, "/api/tokens", `{"operator":"   ","note":"x"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("应返回 400，得到 %d：%s", w.Code, w.Body.String())
	}
	if msg, _ := decode(t, w)["error"].(string); !strings.Contains(msg, "操作人") {
		t.Errorf("应说明原因（操作人不能为空），得到 %q", msg)
	}
	if rows, _ := store.List(); len(rows) != 0 {
		t.Errorf("不该落行：%+v", rows)
	}
	// 失败的写操作也要记审计（和 oao 表格那条约定一致）
	if len(*events) != 1 || (*events)[0].Err == nil {
		t.Errorf("失败的创建也要回调审计且带 error：%+v", *events)
	}
}

// 停用后令牌立刻失效；重复点同一次拿到 409（条件更新）。
func TestSetEnabledAndRepeatRejected(t *testing.T) {
	store, mux, _ := newTestAPI()
	store.Seed("tok-1", "张三", "")

	if op, ok := store.Verify("tok-1"); !ok || op != "张三" {
		t.Fatalf("种子令牌应当可用，得到 (%q, %v)", op, ok)
	}

	w := do(t, mux, http.MethodPost, "/api/tokens/enabled", `{"id":1,"enabled":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("停用应成功，得到 %d：%s", w.Code, w.Body.String())
	}
	if _, ok := store.Verify("tok-1"); ok {
		t.Error("停用后令牌应立刻失效")
	}

	// 再点一次「停用」：状态已经是停用，条件更新影响 0 行 → 409
	w = do(t, mux, http.MethodPost, "/api/tokens/enabled", `{"id":1,"enabled":false}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("重复停用应返回 409，得到 %d：%s", w.Code, w.Body.String())
	}

	// 启用回来
	if w = do(t, mux, http.MethodPost, "/api/tokens/enabled", `{"id":1,"enabled":true}`); w.Code != http.StatusOK {
		t.Fatalf("启用应成功，得到 %d：%s", w.Code, w.Body.String())
	}
	if _, ok := store.Verify("tok-1"); !ok {
		t.Error("启用后令牌应恢复可用")
	}
}

// 删除：不存在（含已删过一次）返回 404。
func TestRemove(t *testing.T) {
	store, mux, _ := newTestAPI()
	store.Seed("tok-1", "张三", "")

	if w := do(t, mux, http.MethodPost, "/api/tokens/remove", `{"id":1}`); w.Code != http.StatusOK {
		t.Fatalf("删除应成功，得到 %d：%s", w.Code, w.Body.String())
	}
	if rows, _ := store.List(); len(rows) != 0 {
		t.Errorf("删完应没有行：%+v", rows)
	}
	if w := do(t, mux, http.MethodPost, "/api/tokens/remove", `{"id":1}`); w.Code != http.StatusNotFound {
		t.Fatalf("删不存在的令牌应返回 404，得到 %d", w.Code)
	}
	if w := do(t, mux, http.MethodPost, "/api/tokens/enabled", `{"id":0,"enabled":true}`); w.Code != http.StatusBadRequest {
		t.Errorf("缺 ID 应返回 400，得到 %d：%s", w.Code, w.Body.String())
	}
}

// 方法不对、body 不是 JSON：400/405，且不能 panic。
func TestBadRequests(t *testing.T) {
	_, mux, _ := newTestAPI()

	if w := do(t, mux, http.MethodGet, "/api/tokens/enabled", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/tokens/enabled 应 405，得到 %d", w.Code)
	}
	if w := do(t, mux, http.MethodPut, "/api/tokens", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT /api/tokens 应 405，得到 %d", w.Code)
	}
	if w := do(t, mux, http.MethodPost, "/api/tokens", "{不是 json"); w.Code != http.StatusBadRequest {
		t.Errorf("坏 body 应 400，得到 %d", w.Code)
	}
}
