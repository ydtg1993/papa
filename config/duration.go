package config

import (
	"fmt"
	"math"
	"math/rand"
	"strings"
	"time"
)

// DurationRange 表示一个固定时长或 [Min, Max] 随机区间。
// 文本形式支持 "10s"（固定）与 "10s-30s"（区间）。
type DurationRange struct {
	Min time.Duration
	Max time.Duration
}

// UnmarshalText 解析 "10s" 或 "10s-30s"，供 viper/mapstructure 解码。
func (d *DurationRange) UnmarshalText(text []byte) error {
	r, err := ParseDurationRange(string(text))
	if err != nil {
		return err
	}
	*d = r
	return nil
}

// ParseDurationRange 解析 "10s" 或 "10s-30s"。
func ParseDurationRange(s string) (DurationRange, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return DurationRange{}, fmt.Errorf("empty duration range")
	}
	parts := strings.Split(s, "-")
	switch len(parts) {
	case 1:
		v, err := time.ParseDuration(strings.TrimSpace(parts[0]))
		if err != nil {
			return DurationRange{}, err
		}
		return DurationRange{Min: v, Max: v}, nil
	case 2:
		min, err := time.ParseDuration(strings.TrimSpace(parts[0]))
		if err != nil {
			return DurationRange{}, err
		}
		max, err := time.ParseDuration(strings.TrimSpace(parts[1]))
		if err != nil {
			return DurationRange{}, err
		}
		if max < min {
			return DurationRange{}, fmt.Errorf("invalid range %q: max < min", s)
		}
		return DurationRange{Min: min, Max: max}, nil
	default:
		return DurationRange{}, fmt.Errorf("invalid duration range %q", s)
	}
}

// Random 返回区间内随机时长；固定值或空区间时返回 Min。
func (d DurationRange) Random() time.Duration {
	if d.Max <= d.Min {
		return d.Min
	}
	width := int64(d.Max - d.Min)
	if width == math.MaxInt64 {
		// 宽度 + 1 会溢出成负数传给 rand.Int63n → panic。`0s-2562047h47m16.854775807s`
		// 这类区间能被 ParseDuration 正常解析，却不可表示"含两端点的均匀采样"。
		// 退化成一个固定值（Min），不 panic、也不产出负数。
		return d.Min
	}
	return d.Min + time.Duration(rand.Int63n(width+1))
}

// Fixed 报告是否为固定值（或空区间）。
func (d DurationRange) Fixed() bool {
	return d.Max <= d.Min
}
