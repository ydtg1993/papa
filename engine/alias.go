package engine

import "github.com/ydtg1993/papa/v2/core"

// 跨包流动的纯值类型统一搬到了 core（零依赖叶子包，见那里的包注释），这里做一次**别名转发**：
// 直接 import papa/v2/engine 的老代码继续可用，抽包这一步对调用方零感知。
//
// 新代码请直接用 core —— 这些类型本来就不属于引擎，放在这里只是历史原因。
// 别名是 Go 的类型别名（`=`），两边是**同一个类型**，不是两个可互转的类型：
// 所以 `map[string]engine.StageStats` 与 `map[string]core.StageStats` 可以直接赋值。
type (
	// 告警
	AlertLevel = core.AlertLevel
	TaskError  = core.TaskError
	AlertEvent = core.AlertEvent
	Notifier   = core.Notifier

	// 错误分类
	NoRetryError = core.NoRetryError

	// 熔断
	BreakerStatus = core.BreakerStatus

	// 监控快照 DTO
	StageStats  = core.StageStats
	GlobalStats = core.GlobalStats
	WorkerStat  = core.WorkerStat
	QueueStats  = core.QueueStats
	StopStats   = core.StopStats
	QueueStat   = core.QueueStat
	TraceStep   = core.TraceStep
)

// 告警级别常量。
const (
	AlertInfo     = core.AlertInfo
	AlertWarn     = core.AlertWarn
	AlertError    = core.AlertError
	AlertCritical = core.AlertCritical
)

// 错误辅助函数。
var (
	WrapNoRetry     = core.WrapNoRetry
	WrapNoRetryKind = core.WrapNoRetryKind
	Retryable       = core.Retryable
	ErrorKind       = core.ErrorKind
)
