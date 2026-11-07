package sysinfo

import (
	"context"
	"io/fs"
	"maps"
	"math"
	"path/filepath"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
)

// DirStat 单个业务目录的占用
type DirStat struct {
	Path string `json:"path"`
	Size int64  `json:"size"` // 字节
}

// Snapshot 一次系统指标采样结果
type Snapshot struct {
	CPUPercent  float64            `json:"cpu_percent"`  // CPU 使用率（百分比 0-100）
	MemUsed     uint64             `json:"mem_used"`     // 内存已用（字节）
	MemTotal    uint64             `json:"mem_total"`    // 内存总量（字节）
	MemPercent  float64            `json:"mem_percent"`  // 内存使用率
	DiskUsed    uint64             `json:"disk_used"`    // 根分区磁盘已用（字节）
	DiskTotal   uint64             `json:"disk_total"`   // 根分区磁盘总量（字节）
	DiskPercent float64            `json:"disk_percent"` // 根分区磁盘使用率
	Dirs        map[string]DirStat `json:"dirs"`         // 业务目录占用，key 为目录名
	// Available 表示最近一轮该指标采样是否成功；失败保留旧数值，但不能当作本轮有效值。
	// SampledAt 是最近一次成功采样完成的 UTC 时间；从未成功时为 nil（JSON null）。
	CPUAvailable     bool       `json:"cpu_available"`
	MemAvailable     bool       `json:"mem_available"`
	DiskAvailable    bool       `json:"disk_available"`
	CPUSampledAt     *time.Time `json:"cpu_sampled_at"`
	MemSampledAt     *time.Time `json:"mem_sampled_at"`
	DiskSampledAt    *time.Time `json:"disk_sampled_at"`
	SampleIntervalMS int64      `json:"sample_interval_ms"` // 系统指标有效采样间隔，毫秒。
}

// systemSources 属于单个 Collector，构造后保持不变；测试不用替换共享全局函数。
type systemSources struct {
	cpuPercent func(time.Duration, bool) ([]float64, error)
	memory     func() (*mem.VirtualMemoryStat, error)
	diskUsage  func(string) (*disk.UsageStat, error)
	now        func() time.Time
}

// Collector 后台周期采样系统指标，供监控接口零阻塞读取
type Collector struct {
	interval    time.Duration
	dirInterval time.Duration
	dirs        map[string]string // name -> path
	mu          sync.RWMutex
	snapshot    Snapshot
	sources     systemSources
}

// NewCollector 创建采集器；interval 为系统指标采样周期，dirs 为要统计占用的业务目录（name->path）
func NewCollector(interval time.Duration, dirs map[string]string) *Collector {
	return newCollector(interval, dirs, systemSources{
		cpuPercent: cpu.Percent,
		memory:     mem.VirtualMemory,
		diskUsage:  disk.Usage,
		now:        time.Now,
	})
}

func newCollector(interval time.Duration, dirs map[string]string, sources systemSources) *Collector {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	return &Collector{
		interval:    interval,
		dirInterval: 30 * time.Second, // 目录遍历较慢，放慢采样
		dirs:        maps.Clone(dirs),
		sources:     sources,
		snapshot:    Snapshot{SampleIntervalMS: interval.Milliseconds()},
	}
}

// Start 启动后台采样 goroutine，ctx 取消即退出
func (c *Collector) Start(ctx context.Context) {
	go func() {
		c.collectSystem()
		c.collectDirs()
		sysTicker := time.NewTicker(c.interval)
		dirTicker := time.NewTicker(c.dirInterval)
		defer sysTicker.Stop()
		defer dirTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-sysTicker.C:
				c.collectSystem()
			case <-dirTicker.C:
				c.collectDirs()
			}
		}
	}()
}

// collectSystem 在锁外完成慢采样，随后短锁发布；Snapshot 不会被 CPU 的 1s 窗口阻塞。
func (c *Collector) collectSystem() {
	var sample Snapshot
	if percents, err := c.sources.cpuPercent(time.Second, false); err == nil && len(percents) > 0 && validPercent(percents[0]) {
		sample.CPUPercent = percents[0]
		sample.CPUAvailable = true
		at := c.sources.now().UTC()
		sample.CPUSampledAt = &at
	}
	if vm, err := c.sources.memory(); err == nil && vm != nil && vm.Total > 0 && validPercent(vm.UsedPercent) {
		sample.MemUsed = vm.Used
		sample.MemTotal = vm.Total
		sample.MemPercent = vm.UsedPercent
		sample.MemAvailable = true
		at := c.sources.now().UTC()
		sample.MemSampledAt = &at
	}
	if du, err := c.sources.diskUsage("/"); err == nil && du != nil && du.Total > 0 && validPercent(du.UsedPercent) {
		sample.DiskUsed = du.Used
		sample.DiskTotal = du.Total
		sample.DiskPercent = du.UsedPercent
		sample.DiskAvailable = true
		at := c.sources.now().UTC()
		sample.DiskSampledAt = &at
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.snapshot.CPUAvailable = sample.CPUAvailable
	c.snapshot.MemAvailable = sample.MemAvailable
	c.snapshot.DiskAvailable = sample.DiskAvailable
	if sample.CPUAvailable {
		c.snapshot.CPUPercent = sample.CPUPercent
		c.snapshot.CPUSampledAt = sample.CPUSampledAt
	}
	if sample.MemAvailable {
		c.snapshot.MemUsed = sample.MemUsed
		c.snapshot.MemTotal = sample.MemTotal
		c.snapshot.MemPercent = sample.MemPercent
		c.snapshot.MemSampledAt = sample.MemSampledAt
	}
	if sample.DiskAvailable {
		c.snapshot.DiskUsed = sample.DiskUsed
		c.snapshot.DiskTotal = sample.DiskTotal
		c.snapshot.DiskPercent = sample.DiskPercent
		c.snapshot.DiskSampledAt = sample.DiskSampledAt
	}
}

func validPercent(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 100
}

// collectDirs 统计各业务目录占用；目录不存在或遍历失败则记 0
func (c *Collector) collectDirs() {
	if len(c.dirs) == 0 {
		return
	}
	stats := make(map[string]DirStat, len(c.dirs))
	for name, path := range c.dirs {
		size, err := DirSize(path)
		if err != nil {
			size = 0
		}
		stats[name] = DirStat{Path: path, Size: size}
	}

	c.mu.Lock()
	c.snapshot.Dirs = stats
	c.mu.Unlock()
}

// Snapshot 返回最近一次采样结果
func (c *Collector) Snapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	snapshot := c.snapshot
	// 配置与快照均独占目录 map，调用方修改不能污染采集器或其它读者。
	snapshot.Dirs = maps.Clone(snapshot.Dirs)
	// 时间指针也要按值返回，调用方修改已取得快照不会污染下一次读取。
	for _, at := range []**time.Time{&snapshot.CPUSampledAt, &snapshot.MemSampledAt, &snapshot.DiskSampledAt} {
		if *at != nil {
			value := **at
			*at = &value
		}
	}
	return snapshot
}

// DirSize 递归统计目录下所有文件总大小（字节）；跳过无法读取的条目
func DirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 跳过无权限/已删除的条目
		}
		if d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}
