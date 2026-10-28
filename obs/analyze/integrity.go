package analyze

import (
	"fmt"
	"time"

	"github.com/ydtg1993/papa/v3/obs"
)

// addEventCoverage 检查事件时间覆盖：整段丢失不产生 seq 缺口，只能靠时间断层发现。
func addEventCoverage(in *obs.Input, res *obs.SectionResult, maxGap time.Duration) {
	cov := obs.ComputeEventCoverage(in.Events)
	detail := fmt.Sprintf("首事件 %s，末事件 %s", fmtTime(cov.FirstTS), fmtTime(cov.LastTS))
	check := obs.NewCheck("event_coverage", obs.SeverityWarn, true, "事件时间覆盖正常", detail)
	switch {
	case engineInactive(in):
		check.Skipped, check.SkipReason = true, "引擎暂停/空闲，事件稀疏不代表丢失"
	case cov.MaxGap > maxGap:
		check.Passed = false
		check.Message = fmt.Sprintf("事件最大断层 %s（%s ~ %s）", cov.MaxGap, fmtTime(cov.MaxGapFrom), fmtTime(cov.MaxGapTo))
		in.Degraded = append(in.Degraded, "事件存在时间断层，事件口径可能不完整")
	case !in.Run.From.IsZero() && cov.FirstTS.Sub(in.Run.From) > maxGap:
		check.Passed = false
		check.Message = fmt.Sprintf("窗口起点后 %s 才有首事件（%s）", cov.FirstTS.Sub(in.Run.From), fmtTime(cov.FirstTS))
		in.Degraded = append(in.Degraded, "窗口前段缺事件，事件口径可能不完整")
	}
	res.Checks = append(res.Checks, check)
}
