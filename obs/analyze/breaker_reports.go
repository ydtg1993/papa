package analyze

import (
	"context"

	"github.com/ydtg1993/papa/v4/internal/breakerstore"
	"github.com/ydtg1993/papa/v4/obs"
	"github.com/ydtg1993/papa/v4/pkg/events"
)

// Daily reports reference the same durable incidents as the monitor. They never
// reconstruct a second failure window from a potentially incomplete event log.
func addBreakerReports(ctx context.Context, in *obs.Input, res *obs.SectionResult) {
	ids := []string{}
	seen := map[string]bool{}
	for i := len(in.Events) - 1; i >= 0; i-- {
		ev := in.Events[i]
		id := ev.Attrs["incident_id"]
		if (ev.Type == events.TypeBreakerTripped || ev.Type == events.TypeBreakerTransition) && id != "" && !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	if len(ids) > 100 {
		ids = ids[:100]
		res.Problems = append(res.Problems, obs.Problem{ID: "breaker-references-truncated", Severity: obs.SeverityWarn, Title: "日志中的熔断引用超过 100 条，仅保留最近更新引用"})
	}
	table := obs.Table{Title: "熔断持久报告", Columns: []string{"事件 ID", "说明"}}
	if in.DB == nil {
		if len(ids) == 0 {
			return
		}
		for _, id := range ids {
			table.Rows = append(table.Rows, []string{id, "未连接数据库；请按事件 ID 查询持久报告，不从日志重建"})
		}
		res.Tables = append(res.Tables, table)
		return
	}
	reports, limited, err := breakerstore.New(in.DB).HistoryForWindow(ctx, in.Run.From, reportWindowTo(in), 10000)
	if err != nil {
		res.Problems = append(res.Problems, obs.Problem{ID: "breaker-reports-unavailable", Severity: obs.SeverityWarn, Title: "持久熔断报告读取失败；未用日志补造报告"})
		return
	}
	if limited {
		res.Problems = append(res.Problems, obs.Problem{ID: "breaker-reports-truncated", Severity: obs.SeverityWarn, Title: "当天熔断过程超过 10000 条；本次展示不完整，无法据此确认日报截止状态；数据库历史未删除"})
	}
	found := map[string]bool{}
	last := map[string]int{}
	for _, row := range reports {
		found[row.IncidentID] = true
		detail, err := breakerstore.HistoryText(row)
		if err != nil {
			detail = "历史记录无法解析；未使用最新状态覆盖"
		}
		if !in.Run.From.IsZero() && row.At.Before(in.Run.From) {
			detail = "当日开始时仍在保护；此前最后记录：" + detail
		}
		table.Rows = append(table.Rows, []string{row.IncidentID, row.Site + "；" + detail})
	}
	for i, row := range reports {
		if previous, ok := last[row.IncidentID]; !ok || row.Sequence > reports[previous].Sequence {
			last[row.IncidentID] = i
		}
	}
	for i, row := range reports {
		if limited || last[row.IncidentID] != i || row.State == "closed" || row.State == "recovered" {
			continue
		}
		detail, _ := breakerstore.HistoryText(row)
		res.Problems = append(res.Problems, obs.Problem{ID: "breaker-active-" + row.IncidentID, Scope: row.Site, Severity: obs.SeverityWarn, Title: "本日报截止仍在保护：" + row.State, Evidence: []obs.Evidence{{Type: "db", Ref: "crawler_breaker_history/" + row.IncidentID, Excerpt: detail}}})
	}
	for _, id := range ids {
		if !found[id] {
			table.Rows = append(table.Rows, []string{id, "未包含在本次持久查询结果；可能尚未保存或超出窗口/展示上限"})
		}
	}
	res.Tables = append(res.Tables, table)
}
