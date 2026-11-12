package accountpools

import (
	"errors"
	"github.com/ydtg1993/papa/v4/admin/internal/httpjson"
	"net/http"
	"strconv"

	"github.com/ydtg1993/papa/v4/engine"
)

// accountCompensationAPI is a view/action adapter only. Eligibility, version
// checks and requeue behavior remain in Engine and are shared with other APIs.
func (a *Handler) Compensation(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		query := r.URL.Query()
		limit := 100
		if raw := query.Get("limit"); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value < 1 || value > 200 {
				httpjson.Write(w, 400, map[string]any{"error": "查询数量应在 1 到 200 之间"})
				return
			}
			limit = value
		}
		preview, err := a.Runtime.PreviewCompensation(r.Context(), engine.CompensationFilter{Site: query.Get("site"), AccountPool: query.Get("pool"), AccountID: query.Get("account"), Limit: limit})
		if err != nil {
			httpjson.Write(w, 503, map[string]any{"error": "补偿预览读取失败，请检查数据库及迁移"})
			return
		}
		httpjson.Write(w, 200, preview)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var input struct {
		Action   string                       `json:"action"`
		ID       uint                         `json:"id"`
		Version  uint64                       `json:"version"`
		Requests []engine.CompensationRequest `json:"requests"`
	}
	if err := httpjson.Strict(http.MaxBytesReader(w, r.Body, 32<<10), &input); err != nil || (input.Action != "release" && input.Action != "retry") {
		httpjson.Write(w, 400, map[string]any{"error": "补偿操作请求无效"})
		return
	}
	if input.Action == "release" {
		if input.ID == 0 {
			httpjson.Write(w, 400, map[string]any{"error": "请选择任务"})
			return
		}
		err := a.Runtime.ReleaseTaskCompensation(r.Context(), input.ID, input.Version)
		var safeErr error
		if err != nil {
			safeErr = errors.New("任务条件或版本已变化")
		}
		a.runtimeAudit(r, "task_compensation", "release", strconv.FormatUint(uint64(input.ID), 10), safeErr)
		if err != nil {
			httpjson.Write(w, 409, map[string]any{"error": "解除失败：任务条件或版本已变化，请重新预览"})
			return
		}
		httpjson.Write(w, 200, map[string]any{"released": true, "message": "已解除任务人工等待，尚未重投；请重新预览"})
		return
	}
	if len(input.Requests) == 0 || len(input.Requests) > 200 {
		httpjson.Write(w, 400, map[string]any{"error": "请选择 1 到 200 个任务"})
		return
	}
	result, err := a.Runtime.BatchCompensate(r.Context(), input.Requests)
	var safeErr error
	if err != nil {
		safeErr = errors.New("批量补偿未全部完成")
	}
	for _, item := range result {
		if !item.Submitted {
			safeErr = errors.New("批量补偿未全部完成")
			break
		}
	}
	a.runtimeAudit(r, "task_compensation", "retry", strconv.Itoa(len(input.Requests)), safeErr)
	if err != nil {
		httpjson.Write(w, 503, map[string]any{"error": "批量操作结果待核对，请刷新任务状态；未自动重试", "results": result})
		return
	}
	httpjson.Write(w, 200, map[string]any{"results": result})
}
