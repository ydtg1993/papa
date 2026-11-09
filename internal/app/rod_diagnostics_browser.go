package app

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/launcher/flags"
	"github.com/go-rod/rod/lib/proto"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/ydtg1993/papa/v4/pkg/browser"
)

type rodLogBuffer struct {
	mu   sync.Mutex
	text []byte
}

func (b *rodLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.text = append(b.text, p...)
	if len(b.text) > 32768 {
		b.text = append([]byte(nil), b.text[len(b.text)-32768:]...)
	}
	return len(p), nil
}
func (b *rodLogBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return string(b.text) }

func runRodDiagnostic(ctx context.Context, app *App) (report rodReport) {
	report = newRodReport("about:blank")
	report.Environment["os"] = runtime.GOOS
	report.Environment["arch"] = runtime.GOARCH
	if u, e := user.Current(); e == nil {
		report.Environment["user"] = u.Username
		report.Environment["uid"] = u.Uid
	}
	if m, e := mem.VirtualMemoryWithContext(ctx); e == nil {
		report.Environment["memory_available_bytes"] = m.Available
	} else {
		report.Environment["memory"] = "不可读取"
	}
	if runtime.GOOS == "linux" {
		if d, e := disk.UsageWithContext(ctx, "/dev/shm"); e == nil {
			report.Environment["shm_free_bytes"] = d.Free
			report.Environment["shm_total_bytes"] = d.Total
		} else {
			report.Environment["shm"] = "不可读取"
		}
	}
	var l *launcher.Launcher
	var b *rod.Browser
	var control, dir string
	var logs rodLogBuffer
	var secrets []string
	defer func() {
		start := time.Now()
		if b != nil {
			_ = b.Context(context.Background()).Timeout(3 * time.Second).Close()
		}
		if l != nil && l.PID() != 0 {
			l.Kill()
		}
		// Only remove the directory created by this diagnostic, never a configured profile.
		if dir != "" {
			err := os.RemoveAll(dir)
			step := rodStep{Name: "清理临时浏览器与目录", Status: "ok", Milliseconds: time.Since(start).Milliseconds()}
			if err != nil {
				step.Status = "failed"
				step.Error = redactRod(err.Error(), secrets)
				report.OK = false
			}
			report.Steps = append(report.Steps, step)
		}
		report.BrowserLog = redactRod(logs.String(), secrets)
	}()
	report.step("配置与环境", secrets, func() error {
		if app == nil || app.Config == nil {
			return fmt.Errorf("浏览器配置不可用")
		}
		c := app.Config.Browser
		for _, v := range c.Headers {
			secrets = append(secrets, v)
		}
		for _, v := range c.Flags {
			if len(v) > 3 {
				secrets = append(secrets, v)
			}
		}
		flagNames := make([]string, 0, len(c.Flags))
		for k := range c.Flags {
			flagNames = append(flagNames, k)
		}
		sort.Strings(flagNames)
		report.Environment["flag_names"] = flagNames
		report.Environment["browser"] = map[string]any{"enabled": c.Enable, "binary": c.Binary, "headless": c.Headless, "no_sandbox": c.NoSandbox, "leakless": c.Leakless, "pool_size": c.PoolSize, "headers_count": len(c.Headers), "flags_count": len(c.Flags)}
		report.Environment["isolation"] = "独立临时用户目录、随机调试端口；配置中的 profile/调试端口由诊断隔离值覆盖"
		if !c.Enable {
			return fmt.Errorf("browser.enable=false，生产浏览器池未启用")
		}
		if c.PoolSize <= 0 {
			return fmt.Errorf("browser.pool_size 必须大于 0")
		}
		source, path := c.Binary, c.BrowserPath
		if source == "" {
			if path != "" {
				source = "path"
			} else {
				source = "rod"
			}
		}
		switch source {
		case "rod":
			if path != "" {
				return fmt.Errorf("binary=rod 不能配置 browser_path")
			}
		case "system":
			if path != "" {
				return fmt.Errorf("binary=system 不能配置 browser_path")
			}
			var ok bool
			path, ok = launcher.LookPath()
			if !ok {
				return fmt.Errorf("系统未找到 Chrome / Chromium / Edge")
			}
		case "path":
			if path == "" {
				return fmt.Errorf("binary=path 缺少 browser_path")
			}
		default:
			return fmt.Errorf("不支持的 browser.binary")
		}
		report.Environment["binary_source"] = source
		if path != "" {
			report.Environment["browser_path"] = path
		} else {
			report.Environment["browser_path"] = "Rod 管理（缺失时按当前配置下载）"
		}
		var err error
		root, err := browser.ResolveTempDir(c.TempDir)
		if err != nil {
			return err
		}
		if app.Engine != nil && app.Engine.GetBrowserPool() != nil {
			root = app.Engine.GetBrowserPool().TempDir()
		}
		report.Environment["configured_temp_dir"] = c.TempDir
		report.Environment["effective_temp_dir"] = root
		if _, err = browser.PrepareTempDir(root); err != nil {
			return err
		}
		if usage, e := disk.UsageWithContext(ctx, root); e == nil {
			report.Environment["temp_dir_filesystem"] = usage.Fstype
			report.Environment["temp_dir_total_bytes"] = usage.Total
			report.Environment["temp_dir_free_bytes"] = usage.Free
		}
		l = launcher.New().Context(ctx).Headless(c.Headless).NoSandbox(c.NoSandbox).Leakless(c.Leakless).Logger(&logs)
		if path != "" {
			l.Bin(path)
		}
		for k, v := range c.Flags {
			if v == "" {
				l.Set(flags.Flag(k))
			} else {
				l.Set(flags.Flag(k), v)
			}
		}
		dir, err = browser.NewTempProfile(l, root, "papa-rod-diagnostic-")
		if err != nil {
			return err
		}
		report.Environment["instance_temp_dir"] = dir
		l.RemoteDebuggingPort(0)
		l.Delete(flags.Flag("remote-debugging-pipe"))
		if app.Engine != nil && app.Engine.GetProxy() != nil {
			if p := app.Engine.GetProxy().Next(); p != "" {
				secrets = append(secrets, p)
				l.Proxy(p)
				report.Environment["proxy"] = "当前代理池选取（地址隐藏）"
			}
		}
		return nil
	})
	report.step("启动 Chromium", secrets, func() error { var err error; control, err = l.Launch(); return err })
	report.step("连接浏览器控制端", secrets, func() error { b = rod.New().Context(ctx).ControlURL(control); return b.Connect() })
	var page *rod.Page
	report.step("创建空白页面", secrets, func() error {
		var err error
		page, err = b.Page(proto.TargetCreateTarget{URL: "about:blank"})
		return err
	})
	report.OK = report.step("验证页面控制", secrets, func() error { _, err := page.Info(); return err })
	return report
}
