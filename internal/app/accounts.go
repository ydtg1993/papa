package app

import (
	"context"
	_ "embed"
	"errors"
	"net/http"

	"github.com/ydtg1993/papa/v4/admin/auth"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/account/onboarding"
)

//go:embed accounts.js
var accountsScript string

// UseAccountOnboarding installs a business-constructed service before Run.
// The business must explicitly supply encryption, store and site Providers.
func (a *App) UseAccountOnboarding(s *onboarding.Service) error {
	if s == nil || a.Engine == nil {
		return onboarding.ErrNotConfigured
	}
	if a.accountOnboarding == s {
		return nil
	}
	if a.accountOnboarding != nil {
		return errors.New("account onboarding service already installed")
	}
	if manager := a.Engine.GetAccountManager(); manager != nil && manager != s.Manager() {
		return errors.New("account onboarding manager differs from engine manager")
	}
	a.Engine.SetAccountManager(s.Manager())
	s.WrapContext(func(ctx context.Context, site string) context.Context { return a.Engine.RuntimeContext(ctx, site) })
	s.SetEventListener(a.Engine.NotifyAccountHealthEvent)
	a.accountOnboarding = s
	return nil
}

func (a *App) accountsAPI(w http.ResponseWriter, r *http.Request) {
	owner := auth.OperatorFrom(r.Context())
	if owner == "" {
		writeRuntimeJSON(w, 401, map[string]any{"error": "账号接入需要已验证的操作人"})
		return
	}
	s := a.accountOnboarding
	if r.Method == http.MethodGet {
		if s == nil {
			writeRuntimeJSON(w, 200, map[string]any{"configured": false, "providers": []any{}, "profiles": []any{}, "message": "业务尚未配置账号接入、站点适配器和外部加密密钥"})
			return
		}
		if id := r.URL.Query().Get("process"); id != "" {
			p, err := s.Get(owner, id)
			if err != nil {
				accountsError(w, err)
				return
			}
			writeRuntimeJSON(w, 200, p)
			return
		}
		profiles, err := s.Profiles(r.Context())
		if err != nil {
			accountsError(w, err)
			return
		}
		writeRuntimeJSON(w, 200, map[string]any{"configured": true, "providers": s.Providers(), "profiles": profiles, "runtime": a.Engine.AccountStatus()})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	if s == nil {
		accountsError(w, onboarding.ErrNotConfigured)
		return
	}
	var input struct {
		Action   string             `json:"action"`
		Profile  onboarding.Profile `json:"profile"`
		Key      account.Key        `json:"key"`
		Revision uint64             `json:"revision"`
		Method   string             `json:"method"`
		Input    map[string]string  `json:"input"`
		Remember bool               `json:"remember"`
		ID       string             `json:"id"`
	}
	if err := strictJSON(http.MaxBytesReader(w, r.Body, 1100<<10), &input); err != nil {
		writeRuntimeJSON(w, 400, map[string]any{"error": "账号接入请求格式无效"})
		return
	}
	var result any
	var err error
	switch input.Action {
	case "draft":
		result, err = s.SaveDraft(r.Context(), input.Profile, input.Revision)
	case "start":
		result, err = s.Start(r.Context(), owner, input.Key, input.Revision, input.Method, input.Input, input.Remember)
	case "continue", "confirm", "cancel":
		result, err = s.Act(owner, input.ID, input.Revision, input.Action, input.Input)
	default:
		err = errors.New("unknown onboarding action")
	}
	// Never audit raw inputs, process IDs, provider errors or credential state.
	var auditErr error
	if err != nil {
		auditErr = errors.New("账号接入操作未完成")
	}
	a.runtimeAudit(r, "account_profiles", input.Action, input.Key.Site+":"+input.Key.ID, auditErr)
	if err != nil {
		accountsError(w, err)
		return
	}
	writeRuntimeJSON(w, 200, result)
}
func (a *App) accountsRemoteAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	owner := auth.OperatorFrom(r.Context())
	if owner == "" {
		writeRuntimeJSON(w, 401, map[string]any{"error": "远程登录需要已验证的操作人"})
		return
	}
	if a.accountOnboarding == nil {
		accountsError(w, onboarding.ErrNotConfigured)
		return
	}
	var input onboarding.RemoteRequest
	if err := strictJSON(http.MaxBytesReader(w, r.Body, 12<<10), &input); err != nil {
		writeRuntimeJSON(w, 400, map[string]any{"error": "远程登录请求格式无效"})
		return
	}
	result, err := a.accountOnboarding.Remote(r.Context(), owner, input)
	if err != nil {
		accountsError(w, err)
		return
	}
	writeRuntimeJSON(w, 200, result)
}
func accountsError(w http.ResponseWriter, err error) {
	code, message := 400, "账号操作失败，请核对最新状态；敏感输入不会自动重放"
	switch {
	case errors.Is(err, onboarding.ErrConflict), errors.Is(err, account.ErrSuperseded), errors.Is(err, account.ErrPoolConflict):
		code, message = 409, "版本已变化，请刷新核对后重新提交"
	case errors.Is(err, onboarding.ErrDenied):
		code, message = 403, "此登录过程不属于当前操作人或控制权已失效"
	case errors.Is(err, onboarding.ErrGone):
		code, message = 410, "登录过程已结束、过期或因进程重启终止，请重新发起"
	case errors.Is(err, onboarding.ErrBusy):
		code, message = 429, "登录名额或远程控制正在使用，请稍后重试"
	case errors.Is(err, onboarding.ErrNotConfigured):
		code, message = 503, "业务尚未配置站点适配器和外部加密凭据存储"
	case errors.Is(err, onboarding.ErrIdentity):
		code, message = 409, "识别到的真实账号与绑定身份不匹配，旧凭据保持不变"
	}
	writeRuntimeJSON(w, code, map[string]any{"error": message})
}
