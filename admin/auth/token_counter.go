package auth

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
)

// TokenCounter 是令牌数量（总数 / 启用数）的**进程内 TTL 缓存**。
//
// 为什么需要它：`Verifier` 在每个 `/api/*` 请求上都要先 COUNT 一次启用令牌数，
// `/monitor` 首帧还要 COUNT 总数（`Required`）。前端每 3 秒轮询一次接口，
// 于是 `SELECT count(*) FROM crawler_access_token` 会一直刷日志；而令牌数量
// 几乎不变，这条查询完全值得按 TTL 摊薄。
//
// 缓存的是"**是否已经配过凭据**"这个判断，不是令牌本身：具体令牌仍逐请求
// `Verify` 查库（见 `Verifier`），所以停用/删除令牌对单个请求的拒绝是即时的。
//
// 一致性：
//   - 本进程后台「访问令牌」页的写操作成功后由 app 层调用 `Invalidate`，后台操作即时生效；
//   - `papa token add`（另一个进程）/ 直接改 SQL 的变更，最迟 TTL 后可见。
//
// 方向性（为什么 TTL 不能太长）：缓存里 `enabled>0` 过期（令牌随后全被停用）只会多走一次
// `Verify` → fail closed；反过来缓存里 `enabled=0` 过期（别处刚补了令牌）时，后台在 TTL 内
// 仍会按"还没配凭据"放行 —— 这是安全相关的那一侧，所以 app 装配时 TTL 取短值（`tokenCountCacheTTL`）。
type TokenCounter struct {
	db  *gorm.DB
	ttl time.Duration
	now func() time.Time // 测试注入；生产为 time.Now

	mu        sync.Mutex
	total     int64
	totalAt   time.Time
	enabled   int64
	enabledAt time.Time
}

// NewTokenCounter 造一个带 TTL 的数量缓存；ttl <= 0 表示不缓存（直接透传，与旧行为一致）。
// db 为 nil 时两个计数都恒为 0（视同没配凭据），与 CountEnabled/CountAll 的约定相同。
func NewTokenCounter(db *gorm.DB, ttl time.Duration) *TokenCounter {
	return &TokenCounter{db: db, ttl: ttl, now: time.Now}
}

// dbOrNil 让 nil 接收者也能安全走 CountEnabled/CountAll/Verify 的 nil 语义。
func (c *TokenCounter) dbOrNil() *gorm.DB {
	if c == nil {
		return nil
	}
	return c.db
}

// CountEnabled 返回启用的令牌数；TTL 内命中缓存，不回源。
func (c *TokenCounter) CountEnabled() (int64, error) {
	if c == nil || c.ttl <= 0 || c.db == nil {
		return CountEnabled(c.dbOrNil())
	}
	c.mu.Lock()
	if !c.enabledAt.IsZero() && c.now().Sub(c.enabledAt) < c.ttl {
		n := c.enabled
		c.mu.Unlock()
		return n, nil
	}
	c.mu.Unlock()

	n, err := CountEnabled(c.db)
	if err != nil {
		return 0, err // 读失败不缓存：下一次调用重试，别把一次抖动固化成 0
	}
	c.mu.Lock()
	c.enabled, c.enabledAt = n, c.now()
	c.mu.Unlock()
	return n, nil
}

// CountAll 返回令牌总数（不论启用与否）；TTL 内命中缓存，不回源。
func (c *TokenCounter) CountAll() (int64, error) {
	if c == nil || c.ttl <= 0 || c.db == nil {
		return CountAll(c.dbOrNil())
	}
	c.mu.Lock()
	if !c.totalAt.IsZero() && c.now().Sub(c.totalAt) < c.ttl {
		n := c.total
		c.mu.Unlock()
		return n, nil
	}
	c.mu.Unlock()

	n, err := CountAll(c.db)
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	c.total, c.totalAt = n, c.now()
	c.mu.Unlock()
	return n, nil
}

// Invalidate 让两个计数都失效（本进程写完令牌表后调用，下一次读回源）。
func (c *TokenCounter) Invalidate() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.enabledAt = time.Time{}
	c.totalAt = time.Time{}
	c.mu.Unlock()
}

// Verifier 是 `Verifier(db, log)` 的缓存版：判定逻辑完全一致，只是计数走 TokenCounter。
func (c *TokenCounter) Verifier(log Logger) func(r *http.Request) (string, bool) {
	return func(r *http.Request) (string, bool) {
		n, err := c.CountEnabled()
		if err != nil {
			if log != nil {
				log.Errorf("统计访问令牌失败: %s", err.Error())
			}
			return "", false
		}
		if n == 0 {
			total, err := c.CountAll()
			if err != nil {
				if log != nil {
					log.Errorf("统计访问令牌失败: %s", err.Error())
				}
				return "", false
			}
			// 一条都没配过 → 未配置凭据；配过但都停用了 → 拒绝
			return "", total == 0
		}
		op, ok, err := Verify(c.dbOrNil(), Extract(r))
		if err != nil {
			if log != nil {
				log.Errorf("校验访问令牌失败: %s", err.Error())
			}
			return "", false
		}
		return op, ok
	}
}

// Required 是 `Required(db)` 的缓存版：后台首帧要不要弹登录框。
func (c *TokenCounter) Required() bool {
	total, err := c.CountAll()
	if err != nil {
		return true // 查库失败按 true 处理（fail closed）
	}
	return total > 0
}

// Confirmer 是 `Confirmer(db, log)` 的缓存版：高危操作前的二次确认。
func (c *TokenCounter) Confirmer(log Logger) func(token string) (string, bool) {
	return func(token string) (string, bool) {
		total, err := c.CountAll()
		if err != nil {
			if log != nil {
				log.Errorf("统计访问令牌失败: %s", err.Error())
			}
			return "", false
		}
		if total == 0 {
			return "", true
		}
		op, ok, err := Verify(c.dbOrNil(), strings.TrimSpace(token))
		if err != nil {
			if log != nil {
				log.Errorf("校验访问令牌失败: %s", err.Error())
			}
			return "", false
		}
		return op, ok
	}
}
