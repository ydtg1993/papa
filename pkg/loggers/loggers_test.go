package loggers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"gopkg.in/natefinch/lumberjack.v2"
)

// closeLoggers 关掉各 logger 底下的 lumberjack 文件句柄。
//
// Windows 上开着的文件删不掉（t.TempDir 的清理会报 "being used by another process"），
// 所以测试收尾时必须显式关。生产不需要：进程退出时由 OS 回收。
func closeLoggers(t *testing.T, set LoggerSet) {
	t.Helper()
	for _, l := range []*logrus.Logger{
		set.Sys, set.Engine, set.Monitor, set.Browser, set.Fetcher,
		set.DB, set.Scheduler, set.Proxy, set.Filedown, set.M3U8,
	} {
		if l == nil {
			continue
		}
		if lj, ok := l.Out.(*lumberjack.Logger); ok {
			_ = lj.Close()
		}
	}
}

// 零值配置必须补上默认值：lumberjack 收 0 时行为不明（"不轮转"还是"立刻轮转"），
// 而日志目录是唯一能事后追查的东西，不能让它由"用户没写"决定。
func TestNewLoggerSetFillsDefaults(t *testing.T) {
	dir := t.TempDir()
	set := NewLoggerSet(LoggerConfig{Dir: dir})
	t.Cleanup(func() { closeLoggers(t, set) })

	if set.cfg.MaxSize != 100 || set.cfg.MaxAge != 7 || set.cfg.MaxBackups != 3 {
		t.Fatalf("默认值未补齐：MaxSize=%d MaxAge=%d MaxBackups=%d",
			set.cfg.MaxSize, set.cfg.MaxAge, set.cfg.MaxBackups)
	}

	loggers := map[string]*logrus.Logger{
		"Sys": set.Sys, "Engine": set.Engine, "Monitor": set.Monitor, "Browser": set.Browser,
		"Fetcher": set.Fetcher, "DB": set.DB, "Scheduler": set.Scheduler, "Proxy": set.Proxy,
		"Filedown": set.Filedown, "M3U8": set.M3U8,
	}
	for name, l := range loggers {
		if l == nil {
			t.Fatalf("%s logger 为 nil", name)
		}
	}

	// 显式配置不能被默认值覆盖
	custom := NewLoggerSet(LoggerConfig{Dir: dir, MaxSize: 5, MaxAge: 1, MaxBackups: 2})
	t.Cleanup(func() { closeLoggers(t, custom) })
	if custom.cfg.MaxSize != 5 || custom.cfg.MaxAge != 1 || custom.cfg.MaxBackups != 2 {
		t.Fatalf("显式配置被覆盖：%+v", custom.cfg)
	}
}

// 每个 logger 各写各的文件 —— 共用一份会让"引擎日志"里混进浏览器日志。
func TestEachLoggerWritesOwnFile(t *testing.T) {
	dir := t.TempDir()
	set := NewLoggerSet(LoggerConfig{Dir: dir})

	set.Engine.Info("engine-marker")
	set.Browser.Info("browser-marker")
	closeLoggers(t, set)

	engineLog := readFile(t, filepath.Join(dir, "engine.log"))
	browserLog := readFile(t, filepath.Join(dir, "browser.log"))

	if !strings.Contains(engineLog, "engine-marker") {
		t.Fatalf("engine.log 里没有写入的内容：\n%s", engineLog)
	}
	if strings.Contains(engineLog, "browser-marker") {
		t.Fatalf("engine.log 混进了 browser 的日志：\n%s", engineLog)
	}
	if !strings.Contains(browserLog, "browser-marker") {
		t.Fatalf("browser.log 里没有写入的内容：\n%s", browserLog)
	}
}

// 日志同时进文件和控制台：文件用于事后追查，控制台用于当场看。
// consoleHook 走的是 os.Stdout（不看 logger.Out），这里只钉"它不报错"这一条。
func TestConsoleHookCoversAllLevelsAndWrites(t *testing.T) {
	h := &consoleHook{}
	if got := h.Levels(); len(got) != len(logrus.AllLevels) {
		t.Fatalf("hook 应挂在所有级别上，实得 %v", got)
	}

	entry := logrus.NewEntry(logrus.New())
	entry.Message = "console-hook-probe"
	entry.Level = logrus.ErrorLevel
	if err := h.Fire(entry); err != nil {
		t.Fatalf("Fire = %v, want nil（hook 不该把日志失败变成业务错误）", err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	return string(b)
}
