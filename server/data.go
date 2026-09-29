package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/ydtg1993/papa/v2/pkg/dataadmin"
)

// dataModelsHandler 列出所有可浏览模型及其列元数据
func (s *Monitor) dataModelsHandler(w http.ResponseWriter, r *http.Request) {
	if s.cfg.DataAdmin == nil {
		http.Error(w, "data admin not enabled", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"models": s.cfg.DataAdmin.Models()})
}

// dataListHandler 分页查询某个模型的数据
func (s *Monitor) dataListHandler(w http.ResponseWriter, r *http.Request) {
	if s.cfg.DataAdmin == nil {
		http.Error(w, "data admin not enabled", http.StatusNotFound)
		return
	}
	key := r.PathValue("model")
	q := dataadmin.ListQuery{
		Page:   atoiDefault(r.URL.Query().Get("page"), 1),
		Size:   atoiDefault(r.URL.Query().Get("size"), 20),
		Search: r.URL.Query().Get("search"),
		Sort:   r.URL.Query().Get("sort"),
		Filter: parseFilter(r),
	}
	result, err := s.cfg.DataAdmin.List(key, q)
	if err != nil {
		if errors.Is(err, dataadmin.ErrUnknownModel) {
			http.Error(w, "unknown model", http.StatusNotFound)
			return
		}
		s.logger.Errorf("list model %s: %s", key, err.Error())
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, result)
}

// parseFilter 解析 filter[col]=val 形式的等值筛选
func parseFilter(r *http.Request) map[string]string {
	filter := map[string]string{}
	for k, vs := range r.URL.Query() {
		if len(vs) == 0 {
			continue
		}
		if strings.HasPrefix(k, "filter[") && strings.HasSuffix(k, "]") {
			col := k[len("filter[") : len(k)-1]
			if col != "" {
				filter[col] = vs[0]
			}
		}
	}
	return filter
}

// atoiDefault 解析整数，空/非法返回默认值
func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
