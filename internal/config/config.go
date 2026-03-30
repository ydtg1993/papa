package config

import "time"
import "github.com/spf13/viper"

var Cfg *Config

type Config struct {
	Crawler CrawlerConfig `mapstructure:"crawler"`
	Browser BrowserConfig `mapstructure:"browser"`
	Proxy   ProxyConfig   `mapstructure:"proxy"`
	DB      DBConfig      `mapstructure:"db"`
}

type CrawlerConfig struct {
	Target string                 `mapstructure:"target"`
	Stages map[string]StageConfig `mapstructure:"stages"`
	Retry  RetryConfig            `mapstructure:"retry"`
}

type StageConfig struct {
	WorkerCount int `mapstructure:"worker_count"`
	QueueSize   int `mapstructure:"queue_size"`
}

type RetryConfig struct {
	MaxAttempts int           `mapstructure:"max_attempts"`
	Backoff     time.Duration `mapstructure:"backoff"`
}

type BrowserConfig struct {
	PoolSize    int           `mapstructure:"pool_size"`
	Headless    bool          `mapstructure:"headless"`
	MaxIdleTime time.Duration `mapstructure:"max_idle_time"`
}

type ProxyConfig struct {
	APIURL          string        `mapstructure:"api_url"`
	RefreshInterval time.Duration `mapstructure:"refresh_interval"`
}

// 添加 DBConfig
type DBConfig struct {
	Driver          string        `mapstructure:"driver"`             // mysql, postgres etc
	DSN             string        `mapstructure:"dsn"`                // 数据源名称
	MaxIdleConns    int           `mapstructure:"max_idle_conns"`     // 最大空闲连接数
	MaxOpenConns    int           `mapstructure:"max_open_conns"`     // 最大打开连接数
	ConnMaxLifetime time.Duration `mapstructure:"conn_max_lifetime"`  // 连接最大生命周期
	ConnMaxIdleTime time.Duration `mapstructure:"conn_max_idle_time"` // 空闲连接最大存活时间
}

type OutputConfig struct {
	File string `mapstructure:"file"`
}

func Load(path string) error {
	viper.SetConfigFile(path)
	if err := viper.ReadInConfig(); err != nil {
		return err
	}
	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return err
	}
	Cfg = &cfg
	return nil
}
