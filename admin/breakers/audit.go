package breakers

import (
	"github.com/ydtg1993/papa/v4/admin/oplog"
	"time"
)

// ManualPauseReason is the reason attached to a pause requested by the admin.
const ManualPauseReason = "后台手动暂停"

func ResumeEvent(operator, site, stage string, at time.Time) oplog.Event {
	values := map[string]any{"note": "请求重试整站恢复或解除普通人工暂停；不跳过自动验证", "scope": site}
	if stage != "" {
		values["note"] = "手动放行被暂停的抓取（只放行这个阶段）"
		values["stage"] = stage
	}
	return oplog.Event{Table: "breaker", Action: "resume", Values: values, Operator: operator, At: at}
}

func PauseEvent(operator, site, stage string, at time.Time) oplog.Event {
	values := map[string]any{"note": "后台手动暂停了这一站的抓取", "scope": site}
	if stage != "" {
		values["note"] = "后台手动暂停了这一站的某个阶段"
		values["stage"] = stage
	}
	return oplog.Event{Table: "breaker", Action: "pause", Values: values, Operator: operator, At: at}
}

func ForceResumeEvent(operator, site string, at time.Time) oplog.Event {
	return oplog.Event{Table: "breaker", Action: "force_resume", Values: map[string]any{"scope": site, "note": "人工强制解除保护；未验证健康，保留失败计数"}, Operator: operator, At: at}
}
