package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/ydtg1993/papa/v4/engine"
)

func scheduleID(r *http.Request, key string, required bool) (uint, error) {
	raw := r.URL.Query().Get(key)
	if raw == "" && !required {
		return 0, nil
	}
	n, err := strconv.ParseUint(raw, 10, strconv.IntSize)
	if err != nil || (required && n == 0) {
		return 0, engine.ErrRuleScheduleInvalid
	}
	return uint(n), nil
}

func (s *Monitor) ruleScheduleHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.RuleSchedules == nil || s.cfg.RuleScheduleDetail == nil {
		http.Error(w, "宿主未接入周期批次查询", http.StatusNotFound)
		return
	}
	after, err := scheduleID(r, "after", false)
	var result any
	if err == nil && r.URL.Path == "/api/rule-schedules/detail" {
		var id uint
		id, err = scheduleID(r, "id", true)
		failed := r.URL.Query().Get("failed")
		if failed != "" && failed != "0" && failed != "1" {
			err = engine.ErrRuleScheduleInvalid
		}
		if err == nil {
			result, err = s.cfg.RuleScheduleDetail(r.Context(), id, after, r.URL.Query().Get("batch"), failed == "1")
		}
	} else if err == nil {
		result, err = s.cfg.RuleSchedules(r.Context(), r.URL.Query().Get("site"), after)
	}
	if err != nil {
		code, message := http.StatusInternalServerError, "周期批次读取失败，请查看服务端日志"
		switch {
		case errors.Is(err, engine.ErrRuleScheduleInvalid):
			code, message = http.StatusBadRequest, "周期批次查询参数无效"
		case errors.Is(err, engine.ErrTaskNotFound):
			code, message = http.StatusNotFound, "控制任务不存在"
		case errors.Is(err, engine.ErrStageNotRegistered):
			code, message = http.StatusConflict, "该控制阶段未注册为 RuleSchedule，请检查启动配置"
		case errors.Is(err, engine.ErrRuleScheduleCorrupt):
			code, message = http.StatusConflict, "批次状态或成员完整性异常，请检查持久记录，勿删除成员"
		case errors.Is(err, engine.ErrRuleScheduleConflict):
			code, message = http.StatusConflict, "批次已切换，请刷新后从第一页查看成员"
		case errors.Is(err, context.DeadlineExceeded):
			code, message = http.StatusGatewayTimeout, "周期批次查询超时，请缩小站点范围后重试"
		}
		if code == http.StatusInternalServerError && s.logger != nil {
			s.logger.Errorf("rule schedule read: %v", err)
		}
		http.Error(w, message, code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(result)
}
