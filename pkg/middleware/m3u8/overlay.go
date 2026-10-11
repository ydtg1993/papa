package m3u8

import (
	"strconv"
	"time"
)

// OutputMode 一次下载的**产物形态**。
//
// 为什么要有它：以前只有一对布尔开关（AutoMerge / KeepSegmentsAfterMerge），表达不出
// "只要切片、不要单文件"这种诉求 —— AutoMerge=false 那条路其实仍然会拼出一个单文件。
type OutputMode int

const (
	// OutputMerge 合并成一个文件（默认）：ffmpeg `-c copy` remux 到 MergeExt（默认 .mp4），
	// 片源里的轨道原样保留、不重新编码。分片按 KeepSegments 决定留不留。
	OutputMerge OutputMode = iota
	// OutputSegments 不产出单文件，只留**切片组**：下载好的分片会被挪到
	// `<OutputDir>/<outputFile>/` 这个目录下（`DownloadResult.OutputFile` 就是它，仍然相对 OutputDir）。
	OutputSegments
	// OutputPlaylist 只把播放列表（.m3u8 原文）取回来存成 outputFile，**一个分片都不下**。
	// 用于"只要地址/只要清单"的场景（例如先存下来，回头再决定下不下）。
	OutputPlaylist
)

func (m OutputMode) String() string {
	switch m {
	case OutputSegments:
		return "segments"
	case OutputPlaylist:
		return "playlist"
	default:
		return "merge"
	}
}

// Overlay 在"已有配置"之上叠加的一层改动。**字段全是指针：nil = 不覆盖** ——
// 与 `QueueSpec.Enabled *bool` 同一套语义（区分"没写"和"写了零值"）。
//
// 三个地方用它，一个概念：
//   - 站点声明 `SiteSpec.M3U8` / 阶段声明 `StageSpec.M3U8`（启动时叠到全局那份上）；
//   - fetcher 里 `engine.M3U8ForTask(task).With(overlay)`（临时一份，只给这次调用用）；
//   - 想完全自己拼配置时：`m3u8.NewDownloader(&cfg)`，不受这里任何约束。
//
// **没有"不许改"的字段**：`ResumeStateDir` 也能改，代价写在它自己的注释里（换了 = 断点续传从头来）。
type Overlay struct {
	OutputDir      *string     // 输出目录（DownloadResult.OutputFile 相对它）
	ResumeStateDir *string     // 断点续传状态根目录。**换它 = 已经下了一半的片子从头再来**（状态按 源|目录|文件名 哈希存在它下面）
	Output         *OutputMode // 产物形态（见 OutputMode）
	MergeExt       *string     // 合并输出的扩展名（.mp4 / .mkv / .ts…）—— remux 到哪个容器由它决定
	KeepSegments   *bool       // 合并后是否保留分片（OutputSegments 恒为保留）
	MaxConcurrent  *int        // 并发下载分片数
	SegmentTimeout *int        // 单分片超时（秒）
	MaxRetries     *int        // 单分片重试次数
	RetryInterval  *int        // 重试初始间隔（秒，指数退避）
	EnableResume   *bool       // 断点续传
	RateKB         *int        // 限速（KB/s，0 = 不限）
	SaveBatchSize  *int        // 每下多少个分片存一次状态
	FfmpegPath     *string     // ffmpeg 可执行文件路径（空 = 走 PATH）
	QueueSize      *int        // 这份实例自己的消息队列容量（满了丢弃，不阻塞下载）
	// 不给覆盖的只有 OnProgress（是个回调，配置层不该带函数值 —— 逐次下载要回调就写在
	// DownloadOptions 里，见那边的说明）。
}

// Apply 把这一层叠到 base 上，返回**新的 Config**（不改 base，纯函数、可表驱动测）。
func (o Overlay) Apply(base Config) Config {
	out := base
	if o.OutputDir != nil {
		out.OutputDir = *o.OutputDir
	}
	if o.ResumeStateDir != nil {
		out.ResumeStateDir = *o.ResumeStateDir
	}
	if o.Output != nil {
		out.OutputMode = *o.Output
	}
	if o.MergeExt != nil {
		out.MergeOutputExt = *o.MergeExt
	}
	if o.KeepSegments != nil {
		out.KeepSegmentsAfterMerge = *o.KeepSegments
	}
	if o.MaxConcurrent != nil {
		out.MaxConcurrent = *o.MaxConcurrent
	}
	if o.SegmentTimeout != nil {
		out.SegmentTimeout = time.Duration(*o.SegmentTimeout) * time.Second
	}
	if o.MaxRetries != nil {
		out.MaxRetries = *o.MaxRetries
	}
	if o.RetryInterval != nil {
		out.RetryInterval = *o.RetryInterval
	}
	if o.EnableResume != nil {
		out.EnableResume = *o.EnableResume
	}
	if o.RateKB != nil {
		out.RateKB = *o.RateKB
	}
	if o.SaveBatchSize != nil {
		out.SaveBatchSize = *o.SaveBatchSize
	}
	if o.FfmpegPath != nil {
		out.FfmpegPath = *o.FfmpegPath
	}
	if o.QueueSize != nil {
		out.QueueSize = *o.QueueSize
	}
	return out
}

// Then 把 other 叠在 o 之上（other 里的非 nil 字段赢），返回合并后的**新 Overlay**。
//
// 用途是层级解析：站点那一层 `.Then(阶段那一层)` = "阶段 > 站点"，一次算出该建实例的配置。
func (o Overlay) Then(other Overlay) Overlay {
	merged := o
	if other.OutputDir != nil {
		merged.OutputDir = other.OutputDir
	}
	if other.ResumeStateDir != nil {
		merged.ResumeStateDir = other.ResumeStateDir
	}
	if other.Output != nil {
		merged.Output = other.Output
	}
	if other.MergeExt != nil {
		merged.MergeExt = other.MergeExt
	}
	if other.KeepSegments != nil {
		merged.KeepSegments = other.KeepSegments
	}
	if other.MaxConcurrent != nil {
		merged.MaxConcurrent = other.MaxConcurrent
	}
	if other.SegmentTimeout != nil {
		merged.SegmentTimeout = other.SegmentTimeout
	}
	if other.MaxRetries != nil {
		merged.MaxRetries = other.MaxRetries
	}
	if other.RetryInterval != nil {
		merged.RetryInterval = other.RetryInterval
	}
	if other.EnableResume != nil {
		merged.EnableResume = other.EnableResume
	}
	if other.RateKB != nil {
		merged.RateKB = other.RateKB
	}
	if other.SaveBatchSize != nil {
		merged.SaveBatchSize = other.SaveBatchSize
	}
	if other.FfmpegPath != nil {
		merged.FfmpegPath = other.FfmpegPath
	}
	if other.QueueSize != nil {
		merged.QueueSize = other.QueueSize
	}
	return merged
}

// Fields 列出这一层**改动了哪些项**（`键=值`），没改的不列。
//
// 给"到底哪一层生效"这类自查用：`papa check` 与启动日志靠它把每站/每阶段的覆盖打出来 ——
// 分层的代价就是"改一处要看几份声明"，把生效项列出来是唯一便宜的补偿。
func (o Overlay) Fields() []string {
	var out []string
	add := func(name, value string) { out = append(out, name+"="+value) }
	if o.OutputDir != nil {
		add("output_dir", *o.OutputDir)
	}
	if o.ResumeStateDir != nil {
		add("resume_state_dir", *o.ResumeStateDir)
	}
	if o.Output != nil {
		add("output", o.Output.String())
	}
	if o.MergeExt != nil {
		add("merge_ext", *o.MergeExt)
	}
	if o.KeepSegments != nil {
		add("keep_segments", boolText(*o.KeepSegments))
	}
	if o.MaxConcurrent != nil {
		add("max_concurrent", itoa(*o.MaxConcurrent))
	}
	if o.SegmentTimeout != nil {
		add("segment_timeout", itoa(*o.SegmentTimeout)+"s")
	}
	if o.MaxRetries != nil {
		add("max_retries", itoa(*o.MaxRetries))
	}
	if o.RetryInterval != nil {
		add("retry_interval", itoa(*o.RetryInterval)+"s")
	}
	if o.EnableResume != nil {
		add("enable_resume", boolText(*o.EnableResume))
	}
	if o.RateKB != nil {
		add("rate_kb", itoa(*o.RateKB))
	}
	if o.SaveBatchSize != nil {
		add("save_batch_size", itoa(*o.SaveBatchSize))
	}
	if o.FfmpegPath != nil {
		add("ffmpeg_path", *o.FfmpegPath)
	}
	if o.QueueSize != nil {
		add("queue_size", itoa(*o.QueueSize))
	}
	return out
}

// IsZero 这一层什么都没改（用来跳过"空的覆盖层"）。
func (o Overlay) IsZero() bool { return len(o.Fields()) == 0 }

func boolText(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func itoa(v int) string { return strconv.Itoa(v) }
