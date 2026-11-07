package auth

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
)

// TokenCounter caches counts for administrative displays only. Authentication
// never authorizes from counts: every presented token is verified against the DB.
// CLI token creation and revocation therefore take effect on the next request.
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

// Verifier validates each request. Missing credentials/storage always deny.
func (c *TokenCounter) Verifier(log Logger) func(r *http.Request) (string, bool) {
	return func(r *http.Request) (string, bool) {
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

// Required 始终要求登录，不读取令牌数量缓存。
func (c *TokenCounter) Required() bool { return true }

// Confirmer 逐次校验令牌，用于高危操作前的二次确认。
func (c *TokenCounter) Confirmer(log Logger) func(token string) (string, bool) {
	return func(token string) (string, bool) {
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
