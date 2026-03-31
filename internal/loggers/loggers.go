package loggers

import (
	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/internal/config"
	"gopkg.in/natefinch/lumberjack.v2"
	_ "gopkg.in/natefinch/lumberjack.v2"
	"os"
	"path/filepath"
)

type LoggerSet struct {
	Sys     *logrus.Logger
	Engine  *logrus.Logger
	Worker  *logrus.Logger
	Monitor *logrus.Logger
	Browser *logrus.Logger
	Proxy   *logrus.Logger
	DB      *logrus.Logger
}

func NewLoggerSet(cfg config.LogConfig) LoggerSet {
	// 通用配置
	formatter := &logrus.TextFormatter{
		FullTimestamp:   true,
		TimestampFormat: "2006-01-02 15:04:05",
	}

	return LoggerSet{
		Sys:     newLogger(filepath.Join(cfg.Dir, "/sys.log"), formatter, cfg),
		Engine:  newLogger(filepath.Join(cfg.Dir, "/engine.log"), formatter, cfg),
		Worker:  newLogger(filepath.Join(cfg.Dir, "/worker.log"), formatter, cfg),
		Monitor: newLogger(filepath.Join(cfg.Dir, "/monitor.log"), formatter, cfg),
		Browser: newLogger(filepath.Join(cfg.Dir, "/browser.log"), formatter, cfg),
		Proxy:   newLogger(filepath.Join(cfg.Dir, "/proxy.log"), formatter, cfg),
		DB:      newLogger(filepath.Join(cfg.Dir, "/db.log"), formatter, cfg),
	}
}

func newLogger(filename string, formatter logrus.Formatter, cfg config.LogConfig) *logrus.Logger {
	logger := logrus.New()
	logger.SetFormatter(formatter)
	logger.SetOutput(&lumberjack.Logger{
		Filename:   filename,
		MaxSize:    cfg.MaxSize, // MB
		MaxBackups: 5,
		MaxAge:     30, // days
		Compress:   true,
	})
	logger.AddHook(&consoleHook{})
	return logger
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
