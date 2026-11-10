package governance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/mem"
)

// NewSampler returns a cross-platform OS sampler. It keeps its own CPU baseline
// (the first sample is invalid), so unrelated gopsutil callers cannot alter the
// measured interval. On Linux, finite cgroup memory and CPU quotas take priority.
func NewSampler() Sampler { return &osSampler{} }

type osSampler struct {
	mu                          sync.Mutex
	previousTotal, previousIdle float64
	previousUsage               uint64
	previousAt                  time.Time
	previousScope               string
}

func (s *osSampler) Sample(ctx context.Context) (Metrics, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	out := Metrics{SampledAt: now, Source: "host"}
	times, err := cpu.TimesWithContext(ctx, false)
	if err != nil {
		return out, err
	}
	if len(times) != 1 {
		return out, errors.New("CPU sample unavailable")
	}
	memory, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		return out, err
	}
	v := times[0]
	total := v.User + v.System + v.Idle + v.Nice + v.Iowait + v.Irq + v.Softirq + v.Steal
	idle := v.Idle + v.Iowait
	out.MemoryPercent = memory.UsedPercent
	out.AvailableMemoryBytes = memory.Available
	scope := "host"
	quota := float64(0)
	usage := uint64(0)
	if runtime.GOOS == "linux" {
		limits := readCgroup()
		if limits.memoryLimit > 0 && limits.memoryLimit < memory.Total && limits.memoryValid {
			out.MemoryPercent = 100 * float64(limits.memoryUsed) / float64(limits.memoryLimit)
			if limits.memoryUsed < limits.memoryLimit {
				out.AvailableMemoryBytes = min(memory.Available, limits.memoryLimit-limits.memoryUsed)
			} else {
				out.AvailableMemoryBytes = 0
			}
			scope = limits.source
		}
		if limits.cpuQuota > 0 && limits.cpuValid {
			quota, usage = limits.cpuQuota, limits.cpuUsage
			scope = limits.source
		}
	}
	out.Source = scope
	if !s.previousAt.IsZero() && s.previousScope == scope {
		if quota > 0 && usage >= s.previousUsage && now.After(s.previousAt) {
			out.CPUPercent = min(100, 100*float64(usage-s.previousUsage)/float64(time.Second)/now.Sub(s.previousAt).Seconds()/quota)
			out.Valid = true
		} else if quota == 0 && total > s.previousTotal && idle >= s.previousIdle {
			out.CPUPercent = max(0, min(100, 100*(1-(idle-s.previousIdle)/(total-s.previousTotal))))
			out.Valid = true
		}
	}
	out.MemoryPercent = min(100, out.MemoryPercent)
	s.previousTotal, s.previousIdle, s.previousUsage, s.previousAt, s.previousScope = total, idle, usage, now, scope
	return out, nil
}

type cgroupLimits struct {
	memoryLimit, memoryUsed, cpuUsage uint64
	cpuQuota                          float64
	memoryValid, cpuValid             bool
	source                            string
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
func readUint(path string) (uint64, bool) {
	n, err := strconv.ParseUint(readTrim(path), 10, 64)
	return n, err == nil
}

func readCgroup() cgroupLimits {
	// Namespaced containers commonly expose their own cgroup at the mount root;
	// non-namespaced processes require the path listed in /proc/self/cgroup.
	v2 := []string{"/sys/fs/cgroup"}
	v1Memory := []string{"/sys/fs/cgroup/memory"}
	v1CPU := []string{"/sys/fs/cgroup/cpu", "/sys/fs/cgroup/cpu,cpuacct"}
	v1Usage := []string{"/sys/fs/cgroup/cpuacct", "/sys/fs/cgroup/cpu,cpuacct"}
	for _, line := range strings.Split(readTrim("/proc/self/cgroup"), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		sub := strings.TrimPrefix(filepath.Clean("/"+parts[2]), "/")
		if parts[1] == "" {
			v2 = append([]string{filepath.Join("/sys/fs/cgroup", sub)}, v2...)
			continue
		}
		for _, controller := range strings.Split(parts[1], ",") {
			switch controller {
			case "memory":
				v1Memory = append([]string{filepath.Join("/sys/fs/cgroup/memory", sub)}, v1Memory...)
			case "cpu":
				v1CPU = append([]string{filepath.Join("/sys/fs/cgroup/cpu", sub), filepath.Join("/sys/fs/cgroup/cpu,cpuacct", sub)}, v1CPU...)
			case "cpuacct":
				v1Usage = append([]string{filepath.Join("/sys/fs/cgroup/cpuacct", sub), filepath.Join("/sys/fs/cgroup/cpu,cpuacct", sub)}, v1Usage...)
			}
		}
	}
	out := cgroupLimits{source: "cgroup-v2"}
	for _, base := range v2 {
		if limit, ok := readUint(filepath.Join(base, "memory.max")); ok && limit > 0 {
			if used, valid := readUint(filepath.Join(base, "memory.current")); valid {
				out.memoryLimit, out.memoryUsed, out.memoryValid = limit, used, true
				break
			}
		}
	}
	for _, base := range v2 {
		fields := strings.Fields(readTrim(filepath.Join(base, "cpu.max")))
		if len(fields) != 2 {
			continue
		}
		quota, e1 := strconv.ParseUint(fields[0], 10, 64)
		period, e2 := strconv.ParseUint(fields[1], 10, 64)
		if e1 != nil || e2 != nil || quota == 0 || period == 0 {
			continue
		}
		for _, line := range strings.Split(readTrim(filepath.Join(base, "cpu.stat")), "\n") {
			f := strings.Fields(line)
			if len(f) == 2 && f[0] == "usage_usec" {
				if usage, err := strconv.ParseUint(f[1], 10, 64); err == nil && usage <= ^uint64(0)/1000 {
					out.cpuQuota, out.cpuUsage, out.cpuValid = float64(quota)/float64(period), usage*1000, true
				}
			}
		}
		if out.cpuValid {
			break
		}
	}
	if out.memoryValid || out.cpuValid {
		return out
	}
	out.source = "cgroup-v1"
	for _, base := range v1Memory {
		limit, ok := readUint(filepath.Join(base, "memory.limit_in_bytes"))
		used, valid := readUint(filepath.Join(base, "memory.usage_in_bytes"))
		if ok && valid && limit > 0 {
			out.memoryLimit, out.memoryUsed, out.memoryValid = limit, used, true
			break
		}
	}
	for _, base := range v1CPU {
		quota, ok := readUint(filepath.Join(base, "cpu.cfs_quota_us"))
		period, valid := readUint(filepath.Join(base, "cpu.cfs_period_us"))
		if ok && valid && quota > 0 && period > 0 {
			out.cpuQuota = float64(quota) / float64(period)
			break
		}
	}
	if out.cpuQuota > 0 {
		for _, base := range v1Usage {
			usage, ok := readUint(filepath.Join(base, "cpuacct.usage"))
			if ok {
				out.cpuUsage, out.cpuValid = usage, true
				break
			}
		}
	}
	return out
}
