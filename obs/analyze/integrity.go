package analyze

import (
	"fmt"
	"time"

	"github.com/ydtg1993/papa/v3/obs"
)

// addEventCoverage 检查事件时间覆盖：整段丢失不产生 seq 缺口，只能靠时间断层发现。
//
// 但「工程停机」也会留下大断层：进程不在运行的那段时间本来就没有事件。dts 是
// obs.ComputeDowntimes 还原出来的停机区间；最大断层若与某段停机起止秒级吻合，就判通过、
// 且**不写 degraded**（这不是数据缺失）。只有对不上停机边界的真断层才保留原 warn + degraded。
func addEventCoverage(in *obs.Input, res *obs.SectionResult, maxGap time.Duration, dts []obs.Downtime) {
	cov := obs.ComputeEventCoverage(in.Events)
	detail := fmt.Sprintf("首事件 %s，末事件 %s", fmtTime(cov.FirstTS), fmtTime(cov.LastTS))
	check := obs.NewCheck("event_coverage", obs.SeverityWarn, true, "事件时间覆盖正常", detail)
	switch {
	case engineInactive(in):
		check.Skipped, check.SkipReason = true, "引擎暂停/空闲，事件稀疏不代表丢失"
	case cov.MaxGap > maxGap:
		if dt, ok := downtimeForGap(cov.MaxGapFrom, cov.MaxGapTo, dts); ok {
			check.Message = fmt.Sprintf("引擎停机 %s（%s ~ %s），不是事件丢失",
				dt.To.Sub(dt.From).Round(time.Second), fmtTime(dt.From), fmtTime(dt.To))
			check.Detail = detail + "；停机区间由 run_stopped↔run_started 还原，期间无事件属正常"
			break
		}
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

// downtimeForGap 判断一段事件断层是否正好等于一次停机：起止都要与停机边界秒级吻合。
// 用「配对吻合」而不是「大断层都算停机」：真丢事件时起止一般对不上 run 边界，
// 那种断层仍必须报可疑。
func downtimeForGap(from, to time.Time, dts []obs.Downtime) (obs.Downtime, bool) {
	for _, dt := range dts {
		if absDuration(from.Sub(dt.From)) <= time.Second && absDuration(to.Sub(dt.To)) <= time.Second {
			return dt, true
		}
	}
	return obs.Downtime{}, false
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
