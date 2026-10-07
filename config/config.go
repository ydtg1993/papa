package config

import (
	"fmt"
	"os"
	"time"

	"github.com/go-viper/mapstructure/v2"
	yaml "go.yaml.in/yaml/v3"
)

type Config struct {
	App          AppConfig          `mapstructure:"app"`
	Log          LogConfig          `mapstructure:"log"`
	Crawler      CrawlerConfig      `mapstructure:"crawler"`
	Browser      BrowserConfig      `mapstructure:"browser"`
	HTML         HTMLConfig         `mapstructure:"html"`
	Proxy        ProxyConfig        `mapstructure:"proxy"`
	DB           DBConfig           `mapstructure:"db"`
	Server       ServerConfig       `mapstructure:"server"`
	Scheduler    SchedulerConfig    `mapstructure:"scheduler"`
	ErrorQueue   ErrorQueueConfig   `mapstructure:"error_queue"`
	RecoverQueue RecoverQueueConfig `mapstructure:"recover_queue"`
	RepeatQueue  RepeatQueueConfig  `mapstructure:"repeat_queue"`
}

// ErrorQueueConfig 失败任务错误队列处理配置
type ErrorQueueConfig struct {
	Enabled     bool          `mapstructure:"enabled"`      // 是否启用错误队列处理
	WorkerCount int           `mapstructure:"worker_count"` // 并发重新投递失败任务的数量
	Interval    time.Duration `mapstructure:"interval"`     // 自动轮询间隔；0 = 不自动轮询，仅手动触发
	MaxRetry    int           `mapstructure:"max_retry"`    // 单个失败任务最多再处理代数；0 = 不限
	BatchSize   int           `mapstructure:"batch_size"`   // 每批查询处理的任务数；0 = 默认 1000（分页流式，避免一次性全量加载）
}

// RecoverQueueConfig 启动恢复配置。
//
// 它现在只做一件事：**进程启动时把「未到终态」的任务（pending/processing）全部重新入队**。
//
// 原来的「运行中按 updated_at 超时判卡死」（`timeout` + `interval` 两个配置项）整个删掉了：
//   - 那个启发式会**误伤长任务** —— 下整部剧跑过 timeout 就被当成卡死重投，与仍在跑的 worker 撞车写同一行；
//   - 而它本来要解的「意外中断」，在启动这一刻是**确定**的：进程刚起，processing 全是孤儿，
//     不需要靠时间推测。反过来说，按超时筛还会漏掉崩溃前刚认领的那批（见 crawler/recoverqueue.go）。
//
// 运行期真正的卡死应该靠给外部调用设超时解决（htmlfetch 与 rod 都有 timeout），不是靠事后扫库。
type RecoverQueueConfig struct {
	Enabled     bool `mapstructure:"enabled"`      // 是否启用启动恢复（关掉则启动时不捞）
	WorkerCount int  `mapstructure:"worker_count"` // 并发重新入队的数量
	BatchSize   int  `mapstructure:"batch_size"`   // 每批查询处理的任务数；0 = 默认 1000（分页流式，避免一次性全量加载）
}

// RepeatQueueConfig 周期轮询队列处理配置（定时重新投递「已完成」的 repeatable 任务，实现周期轮询）
type RepeatQueueConfig struct {
	Enabled     bool          `mapstructure:"enabled"`      // 是否启用周期轮询 repeatable 任务
	WorkerCount int           `mapstructure:"worker_count"` // 并发重新投递 repeatable 任务的数量
	Interval    time.Duration `mapstructure:"interval"`     // 轮询间隔；0 = 不自动轮询，仅手动触发
	BatchSize   int           `mapstructure:"batch_size"`   // 每批查询处理的任务数；0 = 默认 1000（分页流式）
}

// AppConfig 环境基础配置
type AppConfig struct {
	Env string `mapstructure:"env"` //dev开发环境(数据迁移) ol线上
}

type LogConfig struct {
	Dir        string `mapstructure:"dir"`      //日志目录
	MaxSize    int    `mapstructure:"max_size"` //文件大小限制
	MaxDays    int    `mapstructure:"max_days"`
	MaxBackups int    `mapstructure:"max_backups"`
	Compress   bool   `mapstructure:"compress"`
	LocalTime  bool   `mapstructure:"local_time"`
}

type CrawlerConfig struct {
	Stages         map[string]StageConfig `mapstructure:"stages"`           //阶段配置 例如:目录页 详情页 内容页...
	DedupCacheSize int                    `mapstructure:"dedup_cache_size"` //内存去重表最大条目数；0=不限，>0 用 LRU 限界，淘汰条目由 DB 唯一索引兜底
	QueueWatermark float64                `mapstructure:"queue_watermark"`  //队列高水位比例(0-1)，达到后溢出到 DB；<=0 或 >1 用默认 0.75
	DrainInterval  time.Duration          `mapstructure:"drain_interval"`   //溢出任务回灌间隔；<=0 用默认 2s
	StopTimeout    time.Duration          `mapstructure:"stop_timeout"`     //优雅退出时等各阶段 worker 排空队列的上限；<=0 用默认 5s
	Trace          TraceConfig            `mapstructure:"trace"`            //单任务步骤追踪
}

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

type StageConfig struct {
	WorkerCount int           `mapstructure:"worker_count"` //worker pool池分配并发数
	QueueSize   int           `mapstructure:"queue_size"`   //任务队列长度
	Delay       DurationRange `mapstructure:"delay"`        // 任务间隔时间 防止被反爬拦截，支持 "10s" 或 "10s-30s"
	Retry       RetryConfig   `mapstructure:"retry"`        //重试
}

type RetryConfig struct {
	MaxAttempts int           `mapstructure:"max_attempts"` //最大尝试次数（含首次执行）
	Backoff     time.Duration `mapstructure:"backoff"`      //退出延迟
}

// BrowserConfig chromedp浏览器池配置
type BrowserConfig struct {
	Enable      bool              `mapstructure:"enable"`           //是否启用浏览器池，false 则不创建
	PoolSize    int               `mapstructure:"pool_size"`        //浏览器并发上限（按需创建，非常驻数量；改需重启）
	DirectSize  int               `mapstructure:"direct_pool_size"` //强制直连浏览器并发上限（不经过代理）
	MaxIdleTime time.Duration     `mapstructure:"max_idle_time"`    //浏览器生命周期
	Headless    bool              `mapstructure:"headless"`         //无头模式
	NoSandbox   bool              `mapstructure:"no_sandbox"`
	Leakless    bool              `mapstructure:"leakless"` //是否启用 leakless 进程守护（Windows 上其 exe 易被杀软误报）
	BrowserPath string            `mapstructure:"browser_path"`
	Headers     map[string]string `mapstructure:"headers"` //默认请求头
}

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
	Driver          string        `mapstructure:"driver"`             // mysql, postgres etc
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
	Enabled       bool              `mapstructure:"enabled"`
	Port          int               `mapstructure:"port"`           // 监听端口，如 9090
	Whitelist     []string          `mapstructure:"whitelist"`      // 来源 IP/CIDR 白名单（回退默认），空=不限制
	WhitelistFile string            `mapstructure:"whitelist_file"` // 白名单持久化文件路径，优先于 whitelist
	MonitorDirs   map[string]string `mapstructure:"monitor_dirs"`   // 监控页展示的业务目录占用，name->path
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

// SchedulerConfig 定时任务调度器配置。
// 内置 job 已移除：业务定时任务通过 app.RegisterCronJob 注册（见 docs），框架级恢复/失败重试走 recover_queue / error_queue。
// 注：recover_queue 现在只在**启动时**跑一次（见 RecoverQueueConfig），不再有定时轮询。
type SchedulerConfig struct {
	Timezone string `mapstructure:"timezone"` // cron 时区
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
	decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Result: &cfg,
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
	// 拼错一个级别名不该在运行时被静默当成默认值 —— 启动前就报出来
	if !validSQLLogLevel(cfg.DB.LogLevel) {
		return nil, fmt.Errorf("db.log_level 只能是 %s / %s / %s / %s（留空表示按 app.env 推），实得 %q",
			SQLLogSilent, SQLLogError, SQLLogWarn, SQLLogInfo, cfg.DB.LogLevel)
	}
	return &cfg, nil
}
