package loggers

import (
	"github.com/sirupsen/logrus"
	"gopkg.in/natefinch/lumberjack.v2"
	"os"
	"path/filepath"
)

type LoggerSet struct {
	Sys       *logrus.Logger //系统日志
	Engine    *logrus.Logger //引擎管理器
	Monitor   *logrus.Logger //监控器
	Browser   *logrus.Logger //rod浏览器驱动
	Fetcher   *logrus.Logger //爬虫业务层
	DB        *logrus.Logger //数据库
	Scheduler *logrus.Logger //任务计划
	Proxy     *logrus.Logger
	Filedown  *logrus.Logger
	M3U8      *logrus.Logger
	cfg       LoggerConfig
}

type LoggerConfig struct {
	Dir string

	// MaxSize 是日志文件在轮转前的最大大小，单位为兆字节（MB）。默认值为 100 MB。
	MaxSize int

	// MaxAge 保留旧日志文件的最大天数（一天按 24 小时计）。
	// **0（或没配）= 用默认的 7 天** —— 注意这不是 lumberjack 那套"0 = 永不删除"：
	// NewLoggerSet 会把 0 归一成 7，想留久一点就给个大数（比如 3650）。
	MaxAge int

	// MaxBackups 保留的旧日志文件个数。**0（或没配）= 用默认的 3 份**（同上，不是"全留"）。
	MaxBackups int

	// LocalTime 决定备份文件中时间戳的格式化是否使用计算机的本地时间。默认使用 UTC 时间。
	LocalTime bool

	// Compress 决定轮转的日志文件是否应使用 gzip 压缩。默认不进行压缩。
	Compress bool

	// Level 十套 logger 的统一日志级别：debug / info / warn / warning / error。
	// **空值或未知值一律按 info**：非法值由 config 层在启动时报错（log.level 规则），
	// 这里再兜一次是为了让 pkg/loggers 单独使用时也不 panic —— 这个包不校验用户配置。
	Level string
}

func NewLoggerSet(cfg LoggerConfig) LoggerSet {
	// 通用配置
	formatter := &logrus.TextFormatter{
		FullTimestamp:   true,
		TimestampFormat: "2006-01-02 15:04:05",
	}
	if cfg.MaxSize == 0 {
		cfg.MaxSize = 100
	}
	if cfg.MaxAge == 0 {
		cfg.MaxAge = 7
	}
	if cfg.MaxBackups == 0 {
		cfg.MaxBackups = 3
	}

	return LoggerSet{
		Sys:       newLogger(cfg, filepath.Join(cfg.Dir, "/sys.log"), formatter),
		Engine:    newLogger(cfg, filepath.Join(cfg.Dir, "/engine.log"), formatter),
		Monitor:   newLogger(cfg, filepath.Join(cfg.Dir, "/monitor.log"), formatter),
		Browser:   newLogger(cfg, filepath.Join(cfg.Dir, "/browser.log"), formatter),
		Fetcher:   newLogger(cfg, filepath.Join(cfg.Dir, "/fetcher.log"), formatter),
		DB:        newLogger(cfg, filepath.Join(cfg.Dir, "/db.log"), formatter),
		Scheduler: newLogger(cfg, filepath.Join(cfg.Dir, "/scheduler.log"), formatter),
		Proxy:     newLogger(cfg, filepath.Join(cfg.Dir, "/proxy.log"), formatter),
		Filedown:  newLogger(cfg, filepath.Join(cfg.Dir, "/filedown.log"), formatter),
		M3U8:      newLogger(cfg, filepath.Join(cfg.Dir, "/m3u8.log"), formatter),
		cfg:       cfg,
	}
}

func newLogger(cfg LoggerConfig, filename string, formatter logrus.Formatter) *logrus.Logger {
	logger := logrus.New()
	logger.SetFormatter(formatter)
	logger.SetLevel(cfg.logrusLevel())
	logger.SetOutput(&lumberjack.Logger{
		Filename:   filename,
		MaxSize:    cfg.MaxSize, // MB
		MaxBackups: cfg.MaxBackups,
		MaxAge:     cfg.MaxAge, // days
		Compress:   cfg.Compress,
		LocalTime:  cfg.LocalTime,
	})
	logger.AddHook(&consoleHook{})
	return logger
}

// logrusLevel 把配置里的级别名映射成 logrus 级别。空值与未知值都按 info ——
// 十套 logger 共用同一个级别，不然"调低日志"这件事就变成逐套猜。
func (cfg LoggerConfig) logrusLevel() logrus.Level {
	switch cfg.Level {
	case "debug":
		return logrus.DebugLevel
	case "warn", "warning": // warning 是 warn 的别名
		return logrus.WarnLevel
	case "error":
		return logrus.ErrorLevel
	default:
		return logrus.InfoLevel
	}
}

type consoleHook struct{}

func (h *consoleHook) Levels() []logrus.Level {
	return logrus.AllLevels
}

func (h *consoleHook) Fire(entry *logrus.Entry) error {
	line, err := entry.String()
	if err == nil {
		os.Stdout.Write([]byte(line))
	}
	return nil
}
