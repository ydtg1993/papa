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
	if r == nil || r.limiter == nil || n <= 0 {
		return nil
	}
	// 必须分批等：rate.Limiter.WaitN 在 n 大于桶大小（burst）时**直接返回错误**
	// （"exceeds limiter's burst"），而这里桶大小取的是一秒的额度 —— HLS 片段动辄几百 KB，
	// 于是限速一开、稍大的片段必失败，功能等于不可用。
	// 按 burst 分批等，速率不变，任意大小的片段都能过。
	burst := r.limiter.Burst()
	for n > 0 {
		step := min(n, burst)
		if err := r.limiter.WaitN(ctx, step); err != nil {
			return err
		}
		n -= step
	}
	return nil
}
