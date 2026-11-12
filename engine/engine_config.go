package engine

import (
	"fmt"
	"strings"
	"time"

	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/engine/internal/fetchruntime"
	"github.com/ydtg1993/papa/v4/internal/workerpool"
	"github.com/ydtg1993/papa/v4/pkg/middleware/filedown"
	"github.com/ydtg1993/papa/v4/pkg/middleware/m3u8"
)

// stageInfo 内部阶段信息
type stageInfo struct {
	// stage 阶段名（`GetStage()` 的原值，**不带站点**）。登记表的键是复合键
	// `core.StageKey(config.Site, stage)`，这里留着裸名是给监控快照与日志用的
	//（后台要在站点分组里显示"catalog"这种短名字，而不是 "example:catalog"）。
	stage      string
	config     StageConfig
	fetcher    Fetcher
	submitFunc func(engine *Engine)
}

// StageConfig 阶段配置
type StageConfig struct {
	// Nonempty explicitly attests that a read-only probe covers the entire stage.
	// Stages with writes, downloads or dispatch must leave this empty.
	BreakerReadOnlyCoverage string
	RepeatLimit             int                  // repeat_queue 重投上限：repeat >= 此值不再重投；<=0 不限。
	MaxAttempts             int                  // Handler 最大尝试次数（含首次执行）
	Backoff                 time.Duration        // Handler 错误重试退避时间
	Delay                   config.DurationRange // 任务间隔延迟，支持随机区间
	WorkerCount             int                  // WorkerCount 该阶段专用的 worker 数量
	QueueSize               int                  // QueueSize 该阶段的任务队列缓冲大小

	// Site 本阶段所属站点（`SiteSpec.Key`）。**必填** —— 阶段键是 `site:stage`，熔断按站点
	// 分组、任务表也按它记归属（`crawler_tasks.site`），没有「未归属」那一档。
	Site string

	// Entry 本阶段**有入口任务**：启动那一趟调它的 `SubmitEntries` 投一轮
	//（`ApplyRegisterStage` 的第三趟）。零值 false = 这阶段没有入口这回事。
	//
	// 它只说"有没有入口"，**不说"现在跑不跑"** —— "启动先别跑整站"是站点级的一件事，
	// 写在 `SiteSpec.StartPaused` 上（见 core.Site.StartPaused）。这两件事原来挤在一个
	// 三态枚举里（auto/manual/off），结果是 manual 只管得住入口那一投、管不住整站：
	// 声明 manual 之后启动恢复与轮询队列照样把这一站跑起来。
	Entry bool

	// M3U8 / FileDown 本阶段的下载器**覆盖层**（nil = 不覆盖）：启动时叠在"站点那份 → 全局那份"之上，
	// 解析出这一阶段该用的实例（见 engine/downloads.go）。fetcher 用 `M3U8ForTask(task)` /
	// `FiledownForTask(task)` 取 —— 一行拿到"这一阶段该用的那个"，不会拿错站。
	M3U8     *m3u8.Overlay
	FileDown *filedown.Overlay
}

// AddStage 登记一个阶段。**键由 config.Site + stage 拼出来**（`core.StageKey`）——
// 站点归属取自声明层的 `StageConfig.Site`，所以调用方不可能"忘了带站点"。
// 阶段名只在本站内唯一：两个站登记各自的 `catalog` 是两条独立记录（各自的池子与统计）。
// WorkerCount/QueueSize 必须 > 0；非法配置返回错误且不登记阶段。
// 下载覆盖配置按登记时的值复制；修改调用方原值不会隐式修改阶段。
func (e *Engine) AddStage(stage string, config StageConfig, fetcher Fetcher, subFunc func(engine *Engine)) error {
	if config.BreakerReadOnlyCoverage != "" && (strings.TrimSpace(config.BreakerReadOnlyCoverage) == "" || len(config.BreakerReadOnlyCoverage) > 1024) {
		return fmt.Errorf("stage %q: BreakerReadOnlyCoverage 必须描述完整只读覆盖范围，且不超过 1024 字节", stage)
	}
	if err := workerpool.ValidateConfig(config.WorkerCount, config.QueueSize); err != nil {
		return fmt.Errorf("stage %q: %w", core.StageKey(config.Site, stage), err)
	}
	downloads := fetchruntime.CloneDownloads(SiteDownloads{M3U8: config.M3U8, FileDown: config.FileDown})
	config.M3U8, config.FileDown = downloads.M3U8, downloads.FileDown
	e.stages[core.StageKey(config.Site, stage)] = &stageInfo{
		stage:      stage,
		config:     config,
		fetcher:    fetcher,
		submitFunc: subFunc,
	}
	return nil
}
