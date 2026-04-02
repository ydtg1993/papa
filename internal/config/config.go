package config

import "time"
import "github.com/spf13/viper"

type Config struct {
	App     AppConfig     `mapstructure:"app"`
	Log     LogConfig     `mapstructure:"log"`
	Crawler CrawlerConfig `mapstructure:"crawler"`
	Browser BrowserConfig `mapstructure:"browser"`
	Proxy   ProxyConfig   `mapstructure:"proxy"`
	DB      DBConfig      `mapstructure:"db"`
	Monitor MonitorConfig `mapstructure:"monitor"`
}

// AppConfig 环境基础配置
type AppConfig struct {
	Env string `mapstructure:"env"` //dev开发环境(数据迁移) ol线上
}

type LogConfig struct {
	Dir     string `mapstructure:"dir"`      //日志目录
	MaxSize int    `mapstructure:"max_size"` //文件大小限制
}

type CrawlerConfig struct {
	Target string                 `mapstructure:"target"` //爬虫目标网站域
	Stages map[string]StageConfig `mapstructure:"stages"` //阶段配置 例如:目录页 详情页 内容页...
}

type StageConfig struct {
	WorkerCount int         `mapstructure:"worker_count"` //worker pool池分配并发数
	QueueSize   int         `mapstructure:"queue_size"`   //任务队列长度
	Retry       RetryConfig `mapstructure:"retry"`        //重试
}

type RetryConfig struct {
	MaxAttempts int           `mapstructure:"max_attempts"` //重试次数
	Backoff     time.Duration `mapstructure:"backoff"`      //退出延迟
}

// BrowserConfig chromedp浏览器池配置
type BrowserConfig struct {
	PoolSize    int           `mapstructure:"pool_size"`     //唤起浏览器数量
	MaxIdleTime time.Duration `mapstructure:"max_idle_time"` //浏览器生命周期
	Headless    bool          `mapstructure:"headless"`      //无头模式
	DisableGpu  bool          `mapstructure:"disable_gpu"`
	NoSandbox   bool          `mapstructure:"no_sandbox"`
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

// MonitorConfig 网页监控端
type MonitorConfig struct {
	Enabled bool `mapstructure:"enabled"` // 是否开启监控网页
	Port    int  `mapstructure:"port"`    // 监听端口，如 9090
}

func Load(path string) (*Config, error) {
	viper.SetConfigFile(path)
	if err := viper.ReadInConfig(); err != nil {
		return nil, err
	}
	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
