package sysinfo

import (
	"context"
	"io/fs"
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
}

// Collector 后台周期采样系统指标，供监控接口零阻塞读取
type Collector struct {
	interval    time.Duration
	dirInterval time.Duration
	dirs        map[string]string // name -> path
	mu          sync.RWMutex
	snapshot    Snapshot
}

// NewCollector 创建采集器；interval 为系统指标采样周期，dirs 为要统计占用的业务目录（name->path）
func NewCollector(interval time.Duration, dirs map[string]string) *Collector {
	return &Collector{
		interval:    interval,
		dirInterval: 30 * time.Second, // 目录遍历较慢，放慢采样
		dirs:        dirs,
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

// collectSystem 采样 CPU/内存/磁盘；CPU 需要 1s 窗口，在后台 goroutine 内阻塞不影响请求
func (c *Collector) collectSystem() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if percents, err := cpu.Percent(time.Second, false); err == nil && len(percents) > 0 {
		c.snapshot.CPUPercent = percents[0]
	}
	if vm, err := mem.VirtualMemory(); err == nil {
		c.snapshot.MemUsed = vm.Used
		c.snapshot.MemTotal = vm.Total
		c.snapshot.MemPercent = vm.UsedPercent
	}
	if du, err := disk.Usage("/"); err == nil {
		c.snapshot.DiskUsed = du.Used
		c.snapshot.DiskTotal = du.Total
		c.snapshot.DiskPercent = du.UsedPercent
	}
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
	return c.snapshot
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
