package metrics

import (
	"maps"
	"sync"
)

// Registry 业务自定义监控数据的线程安全内存注册表。
// 供 fetcher 通过 engine.RecordMetric 写入，监控页读取展示；进程重启即清空。
type Registry struct {
	mu   sync.RWMutex
	data map[string]any
}

// New 创建注册表
func New() *Registry {
	return &Registry{data: make(map[string]any)}
}

// Set 写入或覆盖一个 key 对应的值
func (r *Registry) Set(key string, v any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.data[key] = v
}

// GetAll 返回全部数据的浅拷贝（value 本身若为引用类型仍共享，写入方应避免原地修改）
func (r *Registry) GetAll() map[string]any {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cp := make(map[string]any, len(r.data))
	maps.Copy(cp, r.data)
	return cp
}
