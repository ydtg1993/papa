package config

import (
	"os"
	"time"

	"github.com/go-viper/mapstructure/v2"
	yaml "go.yaml.in/yaml/v3"
)

type Config struct {
	App     AppConfig     `mapstructure:"app"`
	Log     LogConfig     `mapstructure:"log"`
	Crawler CrawlerConfig `mapstructure:"crawler"`
	Browser BrowserConfig `mapstructure:"browser"`
	HTML    HTMLConfig    `mapstructure:"html"`
	Proxy   ProxyConfig   `mapstructure:"proxy"`
	DB      DBConfig      `mapstructure:"db"`
	Server  ServerConfig  `mapstructure:"server"`
	// Obs 运行观测：事件流（JSONL）与报告产物。**默认全关**，不写这段时行为与没有观测体系时一致。
	// 这里只放**通用**的运行数据；业务指标与业务阈值属于业务仓库，不进框架配置。
	Obs ObsConfig `mapstructure:"obs"`
	// 三个治理队列都是**全局**一段：error_queue（失败重投，定期轮询）、recover_queue（启动恢复，
	// 只在启动跑一次）、repeat_queue（周期轮询，定期轮询）。缺省（没写这段）= 那条队列不跑。
	ErrorQueue   ErrorQueueConfig   `mapstructure:"error_queue"`
	RecoverQueue RecoverQueueConfig `mapstructure:"recover_queue"`
	RepeatQueue  RepeatQueueConfig  `mapstructure:"repeat_queue"`
	// Business 业务自己的配置段：框架**不解析、不校验**，原样透出给 App（`app.Config.Business`）。
	//
	// 存在的理由是「未知键一律拒绝启动」那条规则（见 Validate）—— 它是对的（拼错的键名不该静默失效），
	// 但副作用是业务没法在同一个 config.yaml 里放自己的配置：`covers.dir`、`archive.mode` 这类
	// 都写不进去，只能另开一个文件自己解析、塞环境变量或写死成代码常量。
	// 这一段就是给它们的位子：框架不认它的语义，写什么键都不管。
	//
	// 要按 struct 取（推荐）用 BusinessSection —— 那份解码会**校验业务段内部的未知键**，
	// 于是"拼错键名静默失效"这件事在业务段里也不会发生。要完全自由的 JSON 结构就直接读这个 map。
	//
	// 只作用于 config.yaml：业务段写在这个文件里，改完重启生效。
	Business map[string]any `mapstructure:"business"`
}

// ErrorQueueConfig 错误队列（失败任务重投）：config.yaml 的 `error_queue` 段。
//
// **全局一条队列**，扫所有站点的 failed 任务重投回各自的阶段。Enabled=false（或没写这段）= 不跑。
type ErrorQueueConfig struct {
	Enabled     bool          `mapstructure:"enabled"`      // 有没有这条队列（false 或没写 = 不跑）
	WorkerCount int           `mapstructure:"worker_count"` // 并发重新投递失败任务的数量（开启时必填 > 0）
	Interval    time.Duration `mapstructure:"interval"`     // 自动轮询间隔；0 = 不自动轮询，仅手动触发
	MaxRetry    int           `mapstructure:"max_retry"`    // 单个失败任务最多再处理代数；0 = 不限
	BatchSize   int           `mapstructure:"batch_size"`   // 每批查询处理的任务数；0 = 默认 1000（分页流式，避免一次性全量加载）
}

// RecoverQueueConfig 启动恢复：config.yaml 的 `recover_queue` 段。
//
// 它只做一件事：**进程启动时**把「未到终态」的任务（pending/processing）全部重新入队，
// **只在启动跑一次**（没有周期、没有手动入口，见 docs/RECOVER_QUEUE.md）。
type RecoverQueueConfig struct {
	Enabled     bool `mapstructure:"enabled"`      // 有没有启动恢复（false 或没写 = 不跑）
	WorkerCount int  `mapstructure:"worker_count"` // 并发重新入队的数量（开启时必填 > 0）
	BatchSize   int  `mapstructure:"batch_size"`   // 每批查询处理的任务数；0 = 默认 1000（分页流式，避免一次性全量加载）
	// BatchPause 批间停顿：每处理完一批「可能还有下一批」的任务后停多久再继续。
	//
	// 默认 30m（见 applyDefaults）：启动恢复面对的是海量积压，一次性把几千条任务灌进各阶段队列
	// 会把队列/溢出表/事件流同时打满；批间停顿把恢复摊到多个时间片里，让 worker 先消化一批。
	// 停机信号（Engine.Stop 的 cancel）能立刻打断停顿，不会把优雅关停卡满 30 分钟。
	// 写 0 或不写 = 默认 30m；要连续跑完请写很小的值（如 "1ms"）——默认值只在 0 时补，不提供"关掉"的开关。
	BatchPause time.Duration `mapstructure:"batch_pause"`
}

// RepeatQueueConfig 周期轮询队列：config.yaml 的 `repeat_queue` 段。
//
// **全局一条队列**，扫所有站点「到点的」repeatable 任务重投回各自的阶段。
// 周期是**每条任务自己的**（`Task.RepeatInterval`，0 = 用这里的 Interval）；这里的 Interval 是
// "最粗兜底"的扫描间隔。
type RepeatQueueConfig struct {
	Enabled     bool          `mapstructure:"enabled"`      // 有没有这条队列（false 或没写 = 不跑）
	WorkerCount int           `mapstructure:"worker_count"` // 并发重新投递 repeatable 任务的数量（开启时必填 > 0）
	Interval    time.Duration `mapstructure:"interval"`     // 最粗兜底的扫描间隔（开启时必填 > 0）
	BatchSize   int           `mapstructure:"batch_size"`   // 每批查询处理的任务数；0 = 默认 1000（分页流式）
}

// AppConfig 环境基础配置
type AppConfig struct {
	// Env 只有 dev / 非 dev 两档，管的是 **SQL 日志**：dev → 级别 info 且不隐参数值；
	// 其它值（含没写）→ 级别 warn 且隐掉参数值。**与建表无关**：迁移只有 `papa migrate` 一条路，
	// 任何环境都不会因为 env 而自动迁移。
	Env string `mapstructure:"env"`
}

type LogConfig struct {
	Dir        string `mapstructure:"dir"`      //日志目录
	MaxSize    int    `mapstructure:"max_size"` //文件大小限制
	MaxDays    int    `mapstructure:"max_days"`
	MaxBackups int    `mapstructure:"max_backups"`
	Compress   bool   `mapstructure:"compress"`
	LocalTime  bool   `mapstructure:"local_time"`
	// Level 十套 logger 的统一日志级别：debug / info / warn / warning / error。
	// 留空（没写）= info；**未知值启动即报错**，不会悄悄按 info 跑（见 validate.go 的规则）。
	Level string `mapstructure:"level"`
}

// ObsConfig 运行观测：config.yaml 的 `obs` 段。
//
// 与业务无关，框架只产出通用的运行数据：
//   - events：事件流（JSONL，一行一个事件），供离线报告与故障复盘消费；
//   - report：报告产物的开关与目录（P1 只占位，P2/P3 补全）。
//
// 默认全关：没写这段 = 不产任何观测文件，行为与改造前一致。人类日志与事件流是两条线 ——
// 日志负责可读的摘要，事件流负责机器可消费的时间序列，互不替代。
type ObsConfig struct {
	Events ObsEventsConfig `mapstructure:"events"`
	Report ObsReportConfig `mapstructure:"report"`
}

// ObsEventsConfig 运行事件流（JSONL）。开启后每次运行写一个事件文件（默认落在 log.dir 下）。
type ObsEventsConfig struct {
	Enabled bool `mapstructure:"enabled"` // 是否写事件流（默认 false）
	// File 事件文件名：空（或全空白）= events.jsonl。相对路径，落在 log.dir 下。
	File string `mapstructure:"file"`
	// Buffer 异步写队列长度：<=0 = 默认 8192。满了丢弃并计数，**绝不阻塞 worker**。
	Buffer int `mapstructure:"buffer"`
	// SnapshotInterval 周期快照（stage_snapshot）间隔：<=0 = 不发（默认）。
	// 快照带的是运行时的时间序列，与监控页是否开启无关。
	SnapshotInterval time.Duration `mapstructure:"snapshot_interval"`
}

// ObsReportConfig 报告产物配置。
type ObsReportConfig struct {
	OnExit bool `mapstructure:"on_exit"` // 进程退出时是否生成报告（默认 false）
	// Interval 定时生成报告：Run 期间每隔 Interval 刷新一次「当天」日报（按本地日期分目录）。
	//
	// 0/未写 = 不自动生成（默认关）。与 OnExit 互相独立：可以只开定时、也可只开退出、也可都开
	//（两者都写同一天目录，后写覆盖前写，即"当天最新快照"）。生成失败只记日志，不影响业务。
	Interval time.Duration `mapstructure:"interval"`
	// Dir 报告根目录：空（或全空白）= ./reports（相对当前工作目录）。
	//
	// 产物按**本地日期**分目录：<dir>/<2006-01-02>/report.md 等（一份 = 一天）；`papa report --run` 的
	// 单 run 报告仍是 <dir>/<run_id>/（不拆，留给排查单次运行）。
	Dir string `mapstructure:"dir"`
	// Retention 报告产物保留期：每次生成报告后，把超过保留期的旧报告目录删掉（整目录）。
	// 0/未写 = 默认 7 天（168h）；负数 = 永久保留、不自动清理（与 crawler.trace.retention 同一口径）。
	// 只清 "<dir>" 下、且目录里有 report.md 的子目录 —— 手工放在同一目录里的其它东西不碰。
	Retention time.Duration `mapstructure:"retention"`
	// Timeout 进程内生成报告的超时；<=0 用 120s。生成失败只记日志，不阻塞退出。
	Timeout time.Duration `mapstructure:"timeout"`
	// Thresholds 通用判定阈值（业务阈值由业务 Section 自己持有，框架不认识）。
	Thresholds ObsThresholdsConfig `mapstructure:"thresholds"`
}

// ObsThresholdsConfig 是框架通用判定阈值。
type ObsThresholdsConfig struct {
	// StuckAfter 判"卡住"的阈值（processing 超过它仍未更新）；<=0 用 10m。
	// 这是**报告口径**，不是引擎启动恢复判据（后者是 updated_at < startedAt）。
	StuckAfter time.Duration `mapstructure:"stuck_after"`
	// CrawlFailRateMax 抓取终态失败率上限；<=0 用 5%。
	CrawlFailRateMax float64 `mapstructure:"crawl_fail_rate_max"`
}

type CrawlerConfig struct {
	DedupCacheSize   int           `mapstructure:"dedup_cache_size"`   //内存去重表最大条目数；0=不限，>0 用 LRU 限界，淘汰条目由 DB 唯一索引兜底
	QueueWatermark   float64       `mapstructure:"queue_watermark"`    //队列高水位比例(0-1)，达到后溢出到 DB；<=0 或 >1 用默认 0.75
	DrainInterval    time.Duration `mapstructure:"drain_interval"`     //溢出任务回灌间隔；<=0 用默认 2s
	StopTimeout      time.Duration `mapstructure:"stop_timeout"`       //优雅退出时等各阶段 worker 排空队列的上限；<=0 用默认 5s
	StageIdleTimeout time.Duration `mapstructure:"stage_idle_timeout"` //阶段工作池连续空闲多久就回收；<=0 = 不回收（常驻）
	Trace            TraceConfig   `mapstructure:"trace"`              //单任务步骤追踪
	Breaker          BreakerConfig `mapstructure:"breaker"`            //熔断（计数 + 阈值）
	Archive          ArchiveConfig `mapstructure:"archive"`            //页面归档：失败时把那一页的原样 HTML 落到本地文件
}

// BreakerConfig 熔断：窗口内**终态失败**数达到阈值就**自动暂停那一站的抓取**，
// 等人处理完再从后台手动放行（`POST /api/breaker/resume`）。
//
// 只数终态失败（重试耗尽 / 不可重试），**不数每次尝试失败** —— 否则一个任务重试 3 次会记 3 次，
// 几个烂 URL 就能把窗口灌满、熔断误触发，把整个爬虫停掉。
//
// 这一节配的是**判据**（窗口与阈值）；"停"这个动作与暂停状态在引擎的暂停信号里
// （人工暂停写的是同一张表，只是来源标成 manual）。**暂停状态不跨重启**：进程重启即恢复运行
// （起进程本身就是一次人工介入）。
type BreakerConfig struct {
	Enabled   bool          `mapstructure:"enabled"`   //是否启用熔断（计数与阈值；默认 false）
	Window    time.Duration `mapstructure:"window"`    //统计窗口；<=0 用默认 5m
	Threshold int           `mapstructure:"threshold"` //窗口内终态失败数达到它即暂停；enabled=true 时必填且 > 0，否则启动 panic
}

// defaultBreakerWindow 未配置窗口时的默认值。
const defaultBreakerWindow = 5 * time.Minute

// WindowOrDefault 返回生效的统计窗口。
func (b BreakerConfig) WindowOrDefault() time.Duration {
	if b.Window <= 0 {
		return defaultBreakerWindow
	}
	return b.Window
}

// 阈值**故意没有** OrDefault：它是"启用时必填"的项，enabled=true 却没给正阈值由
// breaker.New 直接 panic（调用点在 engine.NewEngine，即启动时）。
// 兜一个默认值只会让人以为开着、数的却是另一回事。

// defaultStopTimeout 引擎关停时等各阶段 worker 排空队列的默认上限。
const defaultStopTimeout = 5 * time.Second

// StopTimeoutOrDefault 返回生效的引擎关停排空上限（配置里的 0/负值在这里补成默认 5s）。
//
// 它和 server.shutdown_timeout 是两件事：那个等的是**在途 HTTP 请求**（比如日志打包下载），
// 这个等的是 **worker 把队列里的存量任务跑完**。两者互不相干，各有各的调用点。
func (c CrawlerConfig) StopTimeoutOrDefault() time.Duration {
	if c.StopTimeout <= 0 {
		return defaultStopTimeout
	}
	return c.StopTimeout
}

// TraceConfig 单任务步骤追踪配置。
// 开启后 handler 可用 task.Trace.Step/Fail 上报步骤，写入 crawler_task_trace 表；
// 关闭时不建表、不写库，task.Trace 为 nil（调用是安全的 no-op）。
type TraceConfig struct {
	Enabled   bool          `mapstructure:"enabled"`   // 是否开启步骤追踪（默认 false）
	Retention time.Duration `mapstructure:"retention"` // 步骤记录保留期；0/未写 = 默认 7 天，负数 = 不自动清理
}

// ArchiveConfig 页面归档：把「失败那一刻抓到的那一页」原样落到本地文件。
//
// 为什么是文件而不是 DB 的 content 列：**原始字节**才是证据 —— goquery 把文档重新序列化一遍
// 会丢掉 `<noscript>` 里的回退标签（延迟渲染的封面正是藏在里面），而那恰恰是选择器坏掉时
// 最需要看的东西。文件还能直接用浏览器打开、看它到底长什么样。
//
// 归档由 `FetchHTML` 自动完成：handler 不用写任何代码，抓到的页面先登记在本次尝试的缓冲里，
// 尝试结束（含 panic）时按结局决定落不落盘，并在 trace 里留一条指向文件的步骤。
type ArchiveConfig struct {
	Enabled bool   `mapstructure:"enabled"` // 是否开启（默认 false）
	Dir     string `mapstructure:"dir"`     // 归档根目录，如 logs/fetcher-html（**必填**，见校验层）
	// Mode 落盘时机：failure = 只在失败的尝试落（默认，写入量按失败率走，与 trace「只在失败尝试写 data」同构）；
	// always = 每次尝试都落（排查期开，一天几万个文件很正常）。
	Mode      string        `mapstructure:"mode"`
	Retention time.Duration `mapstructure:"retention"`   // 保留期；0/未写 = 默认 7 天（与 trace 对齐），负数 = 不自动清理
	MaxFileMB int           `mapstructure:"max_file_mb"` // 单页上限（MB）；0/未写 = 默认 8；超了不归档并 WARN
}

// crawler.archive.mode 的两个取值（校验层认这两个，别处一律按 failure 处理）。
const (
	ArchiveModeFailure = "failure" // 只在失败的尝试落盘（默认）
	ArchiveModeAlways  = "always"  // 每次尝试都落盘（排查期用）
)

// StageConfig / RetryConfig 已随"阶段参数搬进 Go 声明"一起移除（见 internal/app/spec.go 的
// StageSpec / RetrySpec）—— 配置里不再有 crawler.stages 段，重试参数也在阶段声明里。

// BrowserConfig 浏览器池配置（go-rod 驱动）
type BrowserConfig struct {
	Enable      bool          `mapstructure:"enable"`           //是否启用浏览器池，false 则不创建
	PoolSize    int           `mapstructure:"pool_size"`        //浏览器并发上限（按需创建，非常驻数量；改需重启）
	DirectSize  int           `mapstructure:"direct_pool_size"` //强制直连浏览器并发上限（不经过代理）
	MaxIdleTime time.Duration `mapstructure:"max_idle_time"`    //池内实例的闲置上限：超时回收；0 = 不回收
	Headless    bool          `mapstructure:"headless"`         //无头模式
	NoSandbox   bool          `mapstructure:"no_sandbox"`
	Leakless    bool          `mapstructure:"leakless"` //是否启用 leakless 进程守护（Windows 上其 exe 易被杀软误报）
	// Binary 浏览器二进制从哪来：rod / system / path（见下面三个常量）。
	// 留空 = 看 browser_path：给了就按 path，没给就按 rod。
	Binary string `mapstructure:"binary"`
	// BrowserPath 只在 Binary 为 path（或 Binary 留空）时生效。
	BrowserPath string `mapstructure:"browser_path"`
	// Flags 额外 Chromium 启动参数（launcher flags）：键是开关名（可带也可不带前导 --），
	// 值为空 = 布尔开关（只出现 `--name`），否则拼成 `--name=value`。
	//
	// **这里只放 Chromium 自己的开关**（disable-gpu / renderer-process-limit / blink-settings …）；
	// 框架已经单列的 headless / no_sandbox / leakless / 代理走各自的配置键，
	// 再在这里写一遍会覆盖掉那几项（flags 最后应用）。
	Flags map[string]string `mapstructure:"flags"`
	// SelfCheck 启动时自检：建池时真实启动一次浏览器（rod 模式下这会触发 Chromium 下载），
	// 起不来就启动失败；false = 第一次抓取时才启动。
	SelfCheck bool              `mapstructure:"self_check"`
	Headers   map[string]string `mapstructure:"headers"` //默认请求头
}

// browser.binary 的取值。
const (
	// BrowserBinaryRod 用 Rod 管理（缺了自动下载）的 Chromium —— 服务器上不用手动装浏览器。
	BrowserBinaryRod = "rod"
	// BrowserBinarySystem 用系统已安装的 Chrome / Chromium / Edge（启动时探测，找不到就报错，不下载）。
	BrowserBinarySystem = "system"
	// BrowserBinaryPath 用 browser_path 指定的可执行文件。
	BrowserBinaryPath = "path"
)

// HTMLConfig 静态 HTML 抓取客户端配置
type HTMLConfig struct {
	Enable      bool              `mapstructure:"enable"`        //是否启用静态 HTML 客户端
	Timeout     time.Duration     `mapstructure:"timeout"`       //请求超时
	MaxBodySize int64             `mapstructure:"max_body_size"` //响应体大小上限（字节）
	Headers     map[string]string `mapstructure:"headers"`       //额外请求头
}

// ProxyConfig 爬虫代理管理器
type ProxyConfig struct {
	APIURL          string        `mapstructure:"api_url"`          //代理服务api url
	RefreshInterval time.Duration `mapstructure:"refresh_interval"` //代理刷新时间
}

// DBConfig 数据库配置
type DBConfig struct {
	Driver          string        `mapstructure:"driver"`             // 数据库驱动：目前只支持 "mysql"（别的值启动即报错）
	DSN             string        `mapstructure:"dsn"`                // 数据源名称
	MaxIdleConns    int           `mapstructure:"max_idle_conns"`     // 最大空闲连接数
	MaxOpenConns    int           `mapstructure:"max_open_conns"`     // 最大打开连接数
	ConnMaxLifetime time.Duration `mapstructure:"conn_max_lifetime"`  // 连接最大生命周期
	ConnMaxIdleTime time.Duration `mapstructure:"conn_max_idle_time"` // 空闲连接最大存活时间
	// LogLevel 是 SQL 日志级别：silent / error / warn / info。
	// **留空按环境推**：dev 打 info（本地要调 SQL），其它一律 warn ——
	// 每条 SQL 连同它的参数值进日志就等于把业务数据带出去，而日志能从 OA 后台导出。
	LogLevel string `mapstructure:"log_level"`
}

// SQL 日志级别名。
const (
	SQLLogSilent = "silent"
	SQLLogError  = "error"
	SQLLogWarn   = "warn"
	SQLLogInfo   = "info"
)

// validSQLLogLevel 报告级别名是否合法（空串合法，表示"按环境推"）。
func validSQLLogLevel(name string) bool {
	switch name {
	case "", SQLLogSilent, SQLLogError, SQLLogWarn, SQLLogInfo:
		return true
	}
	return false
}

// SQLLogLevel 返回生效的 SQL 日志级别名：显式配了 db.log_level 就用它，没配按环境推。
func (c *Config) SQLLogLevel() string {
	if c.DB.LogLevel != "" {
		return c.DB.LogLevel
	}
	if c.App.Env == "dev" {
		return SQLLogInfo
	}
	return SQLLogWarn
}

// SQLHideParams 报告 SQL 日志里要不要隐掉参数值。
// 非 dev 一律隐：即便只打慢查询，那行 SQL 也带着参数值。
// 调试要看具体值就把 env 设成 dev。
func (c *Config) SQLHideParams() bool {
	return c.App.Env != "dev"
}

// ServerConfig 统一 HTTP 服务(监控页面)
type ServerConfig struct {
	// Enabled 是否开启统一 HTTP 服务。开则挂载监控后台（/monitor + /api/* + 表格页 + 自定义页 +
	// 业务用 UseRouter 注册的路由）；关则整个服务不监听，上面这些一律不挂载（启动会打错误日志）。
	// 原来还有一个 monitor 子开关，但它唯一的效果是「起了服务却什么都不挂」，已移除。
	Enabled     bool              `mapstructure:"enabled"`
	Port        int               `mapstructure:"port"`         // 监听端口，如 9090
	MonitorDirs map[string]string `mapstructure:"monitor_dirs"` // 监控页展示的业务目录占用，name->path
	// 治理队列（error/recover/repeat）积压数的采样间隔；<=0 用默认 1m。监控页只读内存快照，仅采样时查库。
	QueueSampleInterval time.Duration `mapstructure:"queue_sample_interval"`
	// 操作日志：开启后后台所有增删改操作写入 crawler_operation_log 表（含失败）。
	// 关闭时不建表、不写库。
	OperationLog bool `mapstructure:"operation_log"`
	// HTTP 服务的超时。<=0 用默认值（写超时除外，见下）。
	// ReadHeaderTimeout 挡慢连接（只发头不发送体的那种）；IdleTimeout 管 keep-alive 空闲连接。
	ReadHeaderTimeout time.Duration `mapstructure:"read_header_timeout"` // 默认 10s
	ReadTimeout       time.Duration `mapstructure:"read_timeout"`        // 默认 30s
	IdleTimeout       time.Duration `mapstructure:"idle_timeout"`        // 默认 60s
	// WriteTimeout **默认 0 = 不限**：日志打包下载可能传很久，给它设一个上限等于把在途下载掐断 ——
	// 那正是「优雅关停」要避免的事。确实要限制再显式配。
	WriteTimeout time.Duration `mapstructure:"write_timeout"`
	// ShutdownTimeout 优雅关停时等在途请求（比如日志下载）跑完的上限；<=0 用默认 10s。
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`
}

// HTTP 服务超时的默认值。
const (
	defaultReadHeaderTimeout = 10 * time.Second
	defaultReadTimeout       = 30 * time.Second
	defaultIdleTimeout       = 60 * time.Second
	defaultShutdownTimeout   = 10 * time.Second
)

// HTTPTimeouts 返回生效的 HTTP 超时（基础配置里的 0 在这里补成默认值）。
func (s ServerConfig) HTTPTimeouts() (readHeader, read, write, idle, shutdown time.Duration) {
	readHeader, read, idle, shutdown = s.ReadHeaderTimeout, s.ReadTimeout, s.IdleTimeout, s.ShutdownTimeout
	if readHeader <= 0 {
		readHeader = defaultReadHeaderTimeout
	}
	if read <= 0 {
		read = defaultReadTimeout
	}
	if idle <= 0 {
		idle = defaultIdleTimeout
	}
	if shutdown <= 0 {
		shutdown = defaultShutdownTimeout
	}
	// write 不补默认：0 就是"不限"，这是刻意的（见字段注释）
	return readHeader, read, s.WriteTimeout, idle, shutdown
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// 用 yaml.v3 解析为 map（保留 key 大小写），再交给 mapstructure 解码，
	// 避免 viper 将 header key 转小写导致 "User-Agent" 等请求头失效。
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	var cfg Config
	var md mapstructure.Metadata
	decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Result: &cfg,
		// 接上 Metadata 才拿得到 Unused —— 即「文件里有、结构体里没有」的键路径。
		// 它是「拼错一个键名，值悄悄不生效」的唯一线索，见 Validate。
		Metadata: &md,
		// 组合默认 hook（时长/切片）与 TextUnmarshaller hook，使 DurationRange 支持 "10s-30s"。
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			mapstructure.TextUnmarshallerHookFunc(),
			mapstructure.StringToTimeDurationHookFunc(),
			mapstructure.StringToSliceHookFunc(","),
		),
	})
	if err != nil {
		return nil, err
	}
	if err := decoder.Decode(raw); err != nil {
		return nil, err
	}
	// 键名与值域统一在这一层校验；越界**直接 panic**（判据与 App.RegisterSites、
	// internal/breaker.New 一致：非法配置该在启动时炸掉，而不是等跑到某条任务上
	// 静默变成另一种行为）。原来那条 db.log_level 检查也搬进了同一张表。
	Validate(&cfg, md.Unused)
	return &cfg, nil
}
