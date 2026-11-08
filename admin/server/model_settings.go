package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/ydtg1993/papa/v4/admin/auth"
	"github.com/ydtg1993/papa/v4/admin/configstore"
)

type ModelSettingsStore interface {
	LoadModelSettings(context.Context) (configstore.ModelSettings, error)
	SaveModelSettings(context.Context, configstore.ModelSettings, string) (configstore.ModelSettings, error)
}

func decodeModelSettingsBody(r io.Reader) (configstore.ModelSettings, error) {
	var out configstore.ModelSettings
	b, err := io.ReadAll(io.LimitReader(r, 8193))
	if err != nil || len(b) > 8192 {
		return out, configstore.ErrModelSettings
	}
	d := json.NewDecoder(bytes.NewReader(b))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return out, configstore.ErrModelSettings
	}
	seen := map[string]bool{}
	for d.More() {
		key, e := d.Token()
		name, ok := key.(string)
		if e != nil || !ok || seen[name] {
			return out, configstore.ErrModelSettings
		}
		seen[name] = true
		var v json.RawMessage
		if d.Decode(&v) != nil || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return out, configstore.ErrModelSettings
		}
	}
	if _, err = d.Token(); err != nil || d.Decode(new(any)) != io.EOF {
		return out, configstore.ErrModelSettings
	}
	d = json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	var input struct {
		configstore.ModelSettings
		APIKey string `json:"api_key"`
	}
	if d.Decode(&input) != nil {
		return out, configstore.ErrModelSettings
	}
	out = input.ModelSettings.WithAPIKey(input.APIKey)
	if out.Validate() != nil {
		return out, configstore.ErrModelSettings
	}
	return out, nil
}

func (s *Monitor) modelSettingsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", 405)
		return
	}
	if s.cfg.ModelSettings == nil {
		http.Error(w, "模型配置存储未接入", 404)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var result configstore.ModelSettings
	var err error
	if r.Method == http.MethodGet {
		result, err = s.cfg.ModelSettings.LoadModelSettings(ctx)
	} else {
		operator := auth.OperatorFrom(r.Context())
		if !ruleText(operator, 128, false) {
			http.Error(w, "需要具名操作者", 403)
			return
		}
		result, err = decodeModelSettingsBody(r.Body)
		if err == nil {
			result, err = s.cfg.ModelSettings.SaveModelSettings(ctx, result, operator)
		}
	}
	if errors.Is(err, configstore.ErrModelConflict) {
		http.Error(w, "配置已变化，请重新加载后审阅保存", 409)
		return
	}
	if errors.Is(err, configstore.ErrModelKeyMissing) {
		http.Error(w, "请填写 API Key", 400)
		return
	}
	if errors.Is(err, configstore.ErrModelSettings) {
		http.Error(w, "模型配置无效，请检查字段与预算上限", 400)
		return
	}
	if err != nil {
		http.Error(w, "模型配置存储不可用", 503)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	response := struct {
		configstore.ModelSettings
		DailyUsage *configstore.ModelDailyUsage `json:"daily_usage,omitempty"`
	}{ModelSettings: result}
	if store, ok := s.cfg.ModelSettings.(interface {
		LoadModelDailyUsage(context.Context) (configstore.ModelDailyUsage, error)
	}); ok {
		usage, e := store.LoadModelDailyUsage(ctx)
		if e != nil {
			http.Error(w, "用量读取失败；若刚保存，请重新加载确认配置", 503)
			return
		}
		response.DailyUsage = &usage
	}
	writeJSON(w, response)
}
