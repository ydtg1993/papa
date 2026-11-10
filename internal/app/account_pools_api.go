package app

import (
	"context"
	"errors"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"net/http"
	"time"
)

func (a *App) accountPoolsAPI(w http.ResponseWriter, r *http.Request) {
	p := a.runtimeState.pools
	view := func() map[string]any {
		v := map[string]any{"configured": p != nil, "pools": []account.PoolSnapshot{}, "accounts": a.Engine.AccountStatus()}
		if p != nil {
			v["pools"] = p.Snapshot()
			v["events"] = p.Events(account.Key{}, 0)
			v["default_policy"] = account.DefaultPolicy()
			v["event_retention"] = 100
		}
		if a.BrowserIdentities() != nil {
			if identityView, err := a.identityView(r.Context(), ""); err == nil {
				v["identity_bindings"] = identityView["bindings"]
				v["identity_reports"] = identityView["reports"]
			} else {
				v["identity_error"] = "身份状态读取失败，请到浏览器诊断页核对"
			}
		}
		return v
	}
	if r.Method == http.MethodGet {
		writeRuntimeJSON(w, 200, view())
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	if p == nil {
		writeRuntimeJSON(w, 503, map[string]any{"error": "业务尚未注册账号管理器"})
		return
	}
	var input struct {
		Action   string             `json:"action"`
		Revision uint64             `json:"revision"`
		Config   account.PoolConfig `json:"config"`
		Key      account.PoolKey    `json:"key"`
		Account  account.Key        `json:"account"`
		Disabled bool               `json:"disabled"`
		Policy   account.Policy     `json:"policy"`
	}
	if err := strictJSON(http.MaxBytesReader(w, r.Body, 64<<10), &input); err != nil {
		writeRuntimeJSON(w, 400, map[string]any{"error": "账号池请求格式无效"})
		return
	}
	if input.Action != "save" && input.Action != "delete" && input.Action != "disable" && input.Action != "verify" && input.Action != "check" && input.Action != "policy" {
		writeRuntimeJSON(w, 400, map[string]any{"error": "未知账号池操作"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	var err error
	switch input.Action {
	case "save":
		if _, ok := a.Engine.Site(input.Config.Key.Site); !ok {
			err = errors.New("未知站点")
		} else {
			_, err = p.Upsert(ctx, input.Config, input.Revision)
		}
	case "delete":
		// A policy reference is a reference too, even before the first task exists.
		a.runtimeState.mu.Lock()
		for site := range a.runtimeState.effective.Sites {
			v := a.runtimeState.effective.Effective(site)
			if site == input.Key.Site {
				if v.Pool == input.Key.Name {
					err = account.ErrPoolReferenced
				}
				for _, stage := range v.Stages {
					if stage.Pool == input.Key.Name {
						err = account.ErrPoolReferenced
					}
				}
			}
		}
		if err == nil {
			err = p.Delete(ctx, input.Key, input.Revision)
		}
		a.runtimeState.mu.Unlock()
	case "disable":
		err = p.SetDisabled(ctx, input.Account, input.Disabled, input.Revision)
	case "verify":
		err = p.Verify(a.Engine.RuntimeContext(ctx, input.Account.Site), input.Account, input.Revision)
	case "check":
		err = p.CheckExisting(a.Engine.RuntimeContext(ctx, input.Account.Site), input.Account, input.Revision)
	case "policy":
		err = p.SetPolicy(ctx, input.Account, input.Policy, input.Revision)
	default:
		err = errors.New("未知账号池操作")
	}
	safeErr := err
	if err != nil {
		safeErr = errors.New("账号池操作失败，请检查状态与版本")
	}
	target := input.Key.Site + ":" + input.Key.Name
	if input.Action == "save" {
		target = input.Config.Key.Site + ":" + input.Config.Key.Name
	} else if input.Action == "disable" || input.Action == "verify" || input.Action == "check" || input.Action == "policy" {
		target = input.Account.Site + ":" + input.Account.ID
	}
	a.runtimeAudit(r, "account_pools", input.Action, target, safeErr)
	if err != nil {
		code := 400
		message := "账号池操作失败，请核对账号注册、状态与版本"
		if errors.Is(err, account.ErrPoolConflict) {
			code = 409
			message = "版本冲突，输入已保留，请核对最新状态"
		}
		if errors.Is(err, account.ErrPoolReferenced) {
			code = 409
			message = "账号池仍被策略或任务引用，请先停用并处理引用"
		}
		response := map[string]any{"error": message, "latest": view()}
		if input.Action == "save" && errors.Is(err, account.ErrPoolConflict) {
			if saved, loadErr := p.GetIncludingDeleted(input.Config.Key); loadErr == nil && saved.Deleted {
				response["deleted"] = saved
				response["error"] = "同名账号池有删除记录，请核对版本后明确重建"
			}
		}
		writeRuntimeJSON(w, code, response)
		return
	}
	writeRuntimeJSON(w, 200, view())
}
