package browserdiagnostics

import (
	"context"
	"errors"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/browser/identity"
	identityruntime "github.com/ydtg1993/papa/v4/pkg/browser/identity/runtime"
)

type Input struct {
	Action    string                  `json:"action"`
	Expected  uint64                  `json:"expected"`
	Profile   identity.Profile        `json:"profile"`
	Subject   identityruntime.Subject `json:"subject"`
	Reference identity.Reference      `json:"reference"`
}

// InvalidInput identifies validation errors without prescribing HTTP status.
type InvalidInput struct{ Message string }

func (e *InvalidInput) Error() string { return e.Message }

// ActionResult distinguishes rejected input from an attempted state operation.
type ActionResult struct {
	Value     any
	Attempted bool
}

func (i Input) Site() string {
	if i.Action == "profile" {
		return i.Profile.Site
	}
	if i.Action == "diagnose" {
		return i.Reference.Site
	}
	return i.Subject.Site
}
func (i Input) AuditKey() string { return i.Site() + ":" + i.Subject.Kind + ":" + i.Subject.Key }

// Apply validates and executes one identity management operation. Every caller
// participates in cancellation/draining independently of the transport adapter.
func (a *Service) Apply(parent context.Context, input Input) (ActionResult, error) {
	ctx, done, ok := a.Begin(parent)
	if !ok {
		return ActionResult{}, ErrClosing
	}
	defer done()
	reject := func(message string) (ActionResult, error) { return ActionResult{}, &InvalidInput{message} }
	m := a.Manager()
	if m == nil {
		return ActionResult{}, errors.New("浏览器身份管理尚未初始化")
	}
	site := input.Site()
	if input.Action == "diagnose" && ((input.Subject.Site != "" && input.Subject.Site != site) || (input.Subject.Kind != "" && input.Subject.Kind != "session" && input.Subject.Kind != "account")) {
		return reject("诊断主体与档案站点不一致或类型无效")
	}
	if a.options.HasSite == nil || !a.options.HasSite(site) {
		return reject("未知站点")
	}
	if input.Action != "profile" && input.Action != "diagnose" {
		if err := input.Subject.Validate(); err != nil {
			return reject(err.Error())
		}
	}
	if input.Subject.Kind == "account" {
		var accounts *account.Manager
		if a.options.Resources != nil {
			accounts = a.options.Resources.GetAccountManager()
		}
		if accounts == nil {
			return reject("未配置账号管理器")
		}
		found := false
		for _, status := range accounts.Status() {
			if status.Key == (account.Key{Site: input.Subject.Site, ID: input.Subject.Key}) {
				found = true
				break
			}
		}
		if !found {
			return reject("账号未配置浏览器或不存在")
		}
	}
	result := ActionResult{Attempted: true}
	var err error
	switch input.Action {
	case "profile":
		if err = input.Profile.Validate(); err != nil {
			return reject(err.Error())
		}
		err = m.Store().AppendProfile(ctx, input.Profile, input.Expected)
		result.Value = input.Profile
	case "stage":
		result.Value, err = m.Stage(ctx, input.Subject, input.Reference, input.Expected)
	case "cancel":
		result.Value, err = m.Cancel(ctx, input.Subject, input.Expected)
	case "reverify":
		result.Value, err = m.Reverify(ctx, input.Subject, input.Expected)
	case "diagnose":
		result.Value, err = a.Diagnose(ctx, input.Reference, input.Subject)
		if errors.Is(err, ErrBusy) {
			result.Attempted = false
		}
	default:
		return reject("未知操作")
	}
	return result, err
}
