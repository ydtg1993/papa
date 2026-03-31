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
}

type AppConfig struct {
	Env string `mapstructure:"env"`
}

type LogConfig struct {
	Dir     string `mapstructure:"dir"`
	MaxSize int    `mapstructure:"max_size"`
}

type CrawlerConfig struct {
	Target string                 `mapstructure:"target"`
	Stages map[string]StageConfig `mapstructure:"stages"`
}

type StageConfig struct {
	WorkerCount int         `mapstructure:"worker_count"`
	QueueSize   int         `mapstructure:"queue_size"`
	Retry       RetryConfig `mapstructure:"retry"`
}

type RetryConfig struct {
	MaxAttempts int           `mapstructure:"max_attempts"`
	Backoff     time.Duration `mapstructure:"backoff"`
}

type BrowserConfig struct {
	PoolSize    int           `mapstructure:"pool_size"`
	MaxIdleTime time.Duration `mapstructure:"max_idle_time"`
	Headless    bool          `mapstructure:"headless"`
	DisableGpu  bool          `mapstructure:"disable_gpu"`
	NoSandbox   bool          `mapstructure:"no_sandbox"`
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
