package m3u8

import (
	"context"
	"golang.org/x/time/rate"
)

// RateLimiter 限速器
type RateLimiter struct {
	limiter *rate.Limiter
}

// NewRateLimiter 创建限速器，rateKB 为 KB/s
func NewRateLimiter(rateKB int) *RateLimiter {
	if rateKB <= 0 {
		return nil // 不限速
	}
	// 转换为 bytes/s
	limit := rate.Limit(float64(rateKB) * 1024)
	return &RateLimiter{
		limiter: rate.NewLimiter(limit, int(limit)), // 桶大小等于速率
	}
}

func (r *RateLimiter) Wait(ctx context.Context, n int) error {
	if r == nil || r.limiter == nil {
		return nil
	}
	return r.limiter.WaitN(ctx, n)
}
