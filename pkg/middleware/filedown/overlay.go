package filedown

import (
	"strconv"
	"time"
)

// Overlay 在"已有配置"之上叠加的一层改动。**字段全是指针：nil = 不覆盖** ——
// 与 `QueueSpec.Enabled *bool` 同一套语义（区分"没写"和"写了零值"）。
//
// 三个地方用它，一个概念：
//   - 站点声明 `SiteSpec.FileDown` / 阶段声明 `StageSpec.FileDown`（启动时叠到全局那份上）；
//   - fetcher 里 `engine.FiledownForTask(task).With(overlay)`（临时一份，只给这次调用用）；
//   - 想完全自己拼配置时：`filedown.NewDownloader(&cfg)`，不受这里任何约束。
//
// 与 m3u8 那份是**同一套约定**（那边多一个产物形态 OutputMode，文件下载没有这一维）。
// **没有"不许改"的字段**：`ResumeStateDir` 也能改，代价写在它自己的注释里（换了 = 断点续传从头来）。
type Overlay struct {
	OutputDir              *string // 输出目录（DownloadResult.OutputFile 相对它）
	ResumeStateDir         *string // 断点续传状态根目录。**换它 = 已经下了一半的文件从头再来**
	MaxRetries             *int    // 重试次数
	RetryInterval          *int    // 重试间隔（秒）
	Timeout                *int    // 单次请求超时（秒）
	MaxConcurrent          *int    // 并发分片数
	ChunkSize              *int64  // 每块大小（字节）
	EnableResume           *bool   // 断点续传
	SaveBatchSize          *int    // 每下载多少个分片保存一次状态
	KeepSegmentsAfterMerge *bool   // 合并后是否保留临时分片文件
	QueueSize              *int    // 这份实例自己的消息队列容量（满了丢弃，不阻塞下载）
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
	if o.MaxRetries != nil {
		out.MaxRetries = *o.MaxRetries
	}
	if o.RetryInterval != nil {
		out.RetryInterval = *o.RetryInterval
	}
	if o.Timeout != nil {
		out.Timeout = time.Duration(*o.Timeout) * time.Second
	}
	if o.MaxConcurrent != nil {
		out.MaxConcurrent = *o.MaxConcurrent
	}
	if o.ChunkSize != nil {
		out.ChunkSize = *o.ChunkSize
	}
	if o.EnableResume != nil {
		out.EnableResume = *o.EnableResume
	}
	if o.SaveBatchSize != nil {
		out.SaveBatchSize = *o.SaveBatchSize
	}
	if o.KeepSegmentsAfterMerge != nil {
		out.KeepSegmentsAfterMerge = *o.KeepSegmentsAfterMerge
	}
	if o.QueueSize != nil {
		out.QueueSize = *o.QueueSize
	}
	return out
}

// Then 把 other 叠在 o 之上（other 里的非 nil 字段赢），返回合并后的**新 Overlay**。
// 用途是层级解析：站点那一层 `.Then(阶段那一层)` = "阶段 > 站点"。
func (o Overlay) Then(other Overlay) Overlay {
	merged := o
	if other.OutputDir != nil {
		merged.OutputDir = other.OutputDir
	}
	if other.ResumeStateDir != nil {
		merged.ResumeStateDir = other.ResumeStateDir
	}
	if other.MaxRetries != nil {
		merged.MaxRetries = other.MaxRetries
	}
	if other.RetryInterval != nil {
		merged.RetryInterval = other.RetryInterval
	}
	if other.Timeout != nil {
		merged.Timeout = other.Timeout
	}
	if other.MaxConcurrent != nil {
		merged.MaxConcurrent = other.MaxConcurrent
	}
	if other.ChunkSize != nil {
		merged.ChunkSize = other.ChunkSize
	}
	if other.EnableResume != nil {
		merged.EnableResume = other.EnableResume
	}
	if other.SaveBatchSize != nil {
		merged.SaveBatchSize = other.SaveBatchSize
	}
	if other.KeepSegmentsAfterMerge != nil {
		merged.KeepSegmentsAfterMerge = other.KeepSegmentsAfterMerge
	}
	if other.QueueSize != nil {
		merged.QueueSize = other.QueueSize
	}
	return merged
}

// Fields 列出这一层**改动了哪些项**（`键=值`），没改的不列。
// 给"到底哪一层生效"这类自查用：`papa check` 与启动日志靠它把每站/每阶段的覆盖打出来。
func (o Overlay) Fields() []string {
	var out []string
	add := func(name, value string) { out = append(out, name+"="+value) }
	if o.OutputDir != nil {
		add("output_dir", *o.OutputDir)
	}
	if o.ResumeStateDir != nil {
		add("resume_state_dir", *o.ResumeStateDir)
	}
	if o.MaxRetries != nil {
		add("max_retries", strconv.Itoa(*o.MaxRetries))
	}
	if o.RetryInterval != nil {
		add("retry_interval", strconv.Itoa(*o.RetryInterval)+"s")
	}
	if o.Timeout != nil {
		add("timeout", strconv.Itoa(*o.Timeout)+"s")
	}
	if o.MaxConcurrent != nil {
		add("max_concurrent", strconv.Itoa(*o.MaxConcurrent))
	}
	if o.ChunkSize != nil {
		add("chunk_size", strconv.FormatInt(*o.ChunkSize, 10))
	}
	if o.EnableResume != nil {
		add("enable_resume", boolText(*o.EnableResume))
	}
	if o.SaveBatchSize != nil {
		add("save_batch_size", strconv.Itoa(*o.SaveBatchSize))
	}
	if o.KeepSegmentsAfterMerge != nil {
		add("keep_segments", boolText(*o.KeepSegmentsAfterMerge))
	}
	if o.QueueSize != nil {
		add("queue_size", strconv.Itoa(*o.QueueSize))
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
