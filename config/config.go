package config

import (
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
}

// ErrorQueueConfig 失败任务错误队列处理配置
type ErrorQueueConfig struct {
	Enabled     bool          `mapstructure:"enabled"`      // 是否启用错误队列处理
	WorkerCount int           `mapstructure:"worker_count"` // 并发重新投递失败任务的数量
	Interval    time.Duration `mapstructure:"interval"`     // 自动轮询间隔；0 = 不自动轮询，仅手动触发
	MaxRetry    int           `mapstructure:"max_retry"`    // 单个失败任务最多再处理代数；0 = 不限
}

// RecoverQueueConfig 中断恢复队列处理配置（主程序重启后立即恢复卡死的 pending/processing 任务）
type RecoverQueueConfig struct {
	Enabled     bool          `mapstructure:"enabled"`      // 是否启用中断恢复队列
	WorkerCount int           `mapstructure:"worker_count"` // 并发恢复数量
	Interval    time.Duration `mapstructure:"interval"`     // 自动轮询间隔；0 = 仅启动时+手动触发
	Timeout     time.Duration `mapstructure:"timeout"`      // 任务卡住多久算卡死（updated_at 早于 now-timeout）；0 = 默认 6h
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
	Target         string                 `mapstructure:"target"`           //爬虫目标网站域
	Stages         map[string]StageConfig `mapstructure:"stages"`           //阶段配置 例如:目录页 详情页 内容页...
	DedupCacheSize int                    `mapstructure:"dedup_cache_size"` //内存去重表最大条目数；0=不限，>0 用 LRU 限界，淘汰条目由 DB 唯一索引兜底
	QueueWatermark float64                `mapstructure:"queue_watermark"`  //队列高水位比例(0-1)，达到后溢出到 DB；<=0 或 >1 用默认 0.75
	DrainInterval  time.Duration          `mapstructure:"drain_interval"`   //溢出任务回灌间隔；<=0 用默认 2s
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
	PoolSize    int               `mapstructure:"pool_size"`        //唤起浏览器数量
	DirectSize  int               `mapstructure:"direct_pool_size"` //强制直连浏览器数量（不经过代理）
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
}

// ServerConfig 统一 HTTP 服务(监控页面)
type ServerConfig struct {
	Enabled       bool              `mapstructure:"enabled"`        // 是否开启 HTTP 服务
	Port          int               `mapstructure:"port"`           // 监听端口，如 9090
	Monitor       bool              `mapstructure:"monitor"`        // 是否挂载监控页面/API(/monitor /api/monitor)
	AuthKey       string            `mapstructure:"auth_key"`       // 监控访问密钥，空=不校验
	AuthKeyFile   string            `mapstructure:"auth_key_file"`  // 密钥文件路径，优先于 auth_key
	Whitelist     []string          `mapstructure:"whitelist"`      // 来源 IP/CIDR 白名单（回退默认），空=不限制
	WhitelistFile string            `mapstructure:"whitelist_file"` // 白名单持久化文件路径，优先于 whitelist
	MonitorDirs   map[string]string `mapstructure:"monitor_dirs"`   // 监控页展示的业务目录占用，name->path
}

// SchedulerConfig 定时任务调度器配置。
// 内置 job 已移除：业务定时任务通过 app.RegisterCronJob 注册（见 docs），框架级恢复/失败重试走 recover_queue / error_queue。
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
	return &cfg, nil
}
