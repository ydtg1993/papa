package access

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/ydtg1993/papa/v4/admin/configstore"
)

// LoadWhitelistContext 保留普通读错时的放宽及日志语义，但取消不是配置值：
// 取消或超时返回 error，调用方不得据此发布空白名单。
func LoadWhitelistContext(ctx context.Context, reader configstore.ContextStringListReader, log Logger) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := reader.LoadStringsContext(ctx, configstore.KeyIPWhitelist)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil, err
	}
	if err != nil {
		log.Errorf("来源白名单读不出来，按「不限制来源」继续：%s", err.Error())
		return nil, nil
	}
	return entries, nil
}

// WhitelistReloader 拥有周期读取的 goroutine、取消和排空状态。
// 宿主单独完成初次读取，随后启动重载；publish 只负责替换生效配置。
// Stop 返回 false 时读取或发布仍未退出，宿主必须保留它使用的数据库等资源。
type WhitelistReloader struct {
	reader   configstore.ContextStringListReader
	log      Logger
	publish  func([]string)
	interval time.Duration

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	stopped bool
}

// NewWhitelistReloader 创建尚未启动的重载器。interval <= 0 使用 30 秒。
func NewWhitelistReloader(reader configstore.ContextStringListReader, log Logger, publish func([]string), interval time.Duration) *WhitelistReloader {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &WhitelistReloader{reader: reader, log: log, publish: publish, interval: interval}
}

// Start 最多启动一次；重复启动、取消后启动返回 false。
func (r *WhitelistReloader) Start(ctx context.Context) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done != nil || r.stopped || ctx.Err() != nil {
		return false
	}
	ctx, r.cancel = context.WithCancel(ctx)
	r.done = make(chan struct{})
	go r.run(ctx, r.done)
	return true
}

func (r *WhitelistReloader) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			entries, err := LoadWhitelistContext(ctx, r.reader, r.log)
			if ctx.Err() != nil {
				return
			}
			if err == nil {
				r.publish(entries)
			}
		}
	}
}

// Cancel 只发出取消信号，不等待当前读取或发布返回。之后用 Stop 确认排空。
func (r *WhitelistReloader) Cancel() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = true
	if r.cancel != nil {
		r.cancel()
	}
}

// Stop 使用宿主提供的统一关停 context，不为自身另外增加等待预算。
// 多次调用安全；读端不理会取消时返回 false，绝不把未退出当成排空。
func (r *WhitelistReloader) Stop(ctx context.Context) bool {
	if r == nil {
		return true
	}
	r.Cancel()
	r.mu.Lock()
	done := r.done
	r.mu.Unlock()
	if done == nil {
		return true
	}
	select {
	case <-done:
		return true
	default:
	}
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// WarnIfOpenToAll 生效白名单为空时喊一声。
//
// 空白名单是**合法**配置（持久配置行缺失或空数组），但必须说出来：
// 它和"配了却没生效"从行为上分不出来 —— 而后者正是"后台怎么谁都能打开"这类排查里
// 最难想到的方向。按 Error 级打，与 auth.WarnIfNoToken 一个路数（一个是来源那道门，
// 一个是凭据那道门；令牌缺失时管理接口仍拒绝访问）。
func WarnIfOpenToAll(log Logger, entries []string) {
	if len(entries) > 0 {
		return
	}
	log.Errorf("警告：生效的来源 IP 白名单为空 —— /monitor 与 /api/* 不限制**任何来源**" +
		"（管理接口仍强制校验访问令牌）；要限制来源，去后台设置页写 IP/CIDR（存在 crawler_config 表的 ip_whitelist 行）")
}
