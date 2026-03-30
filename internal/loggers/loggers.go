package loggers

import (
	"github.com/sirupsen/logrus"
	"gopkg.in/natefinch/lumberjack.v2"
	_ "gopkg.in/natefinch/lumberjack.v2"
	"os"
)

var (
	SysLogger     *logrus.Logger
	EngineLogger  *logrus.Logger
	WorkerLogger  *logrus.Logger
	MonitorLogger *logrus.Logger
	BrowserLogger *logrus.Logger
	ProxyLogger   *logrus.Logger
	DBLogger      *logrus.Logger
)

func InitLogger() {
	// 通用配置
	formatter := &logrus.TextFormatter{
		FullTimestamp:   true,
		TimestampFormat: "2006-01-02 15:04:05",
	}
	//主程日志
	SysLogger = newLogger("./logs/sys.log", formatter)
	// 引擎日志
	EngineLogger = newLogger("./logs/engine.log", formatter)
	// 工作池日志
	WorkerLogger = newLogger("./logs/worker.log", formatter)
	// 监控日志
	MonitorLogger = newLogger("./logs/monitor.log", formatter)
	// 浏览器日志
	BrowserLogger = newLogger("./logs/browser.log", formatter)
	// 代理日志
	ProxyLogger = newLogger("./logs/proxy.log", formatter)
	// 数据库日志
	DBLogger = newLogger("./logs/db.log", formatter)
}

func newLogger(filename string, formatter logrus.Formatter) *logrus.Logger {
	logger := logrus.New()
	logger.SetFormatter(formatter)
	logger.SetOutput(&lumberjack.Logger{
		Filename:   filename,
		MaxSize:    10, // MB
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
