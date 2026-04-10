package proxy

import (
	"encoding/json"
	"fmt"
	pkg2 "github.com/ydtg1993/papa/pkg"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Manager 代理管理器，从 API 获取代理并缓存
type Manager struct {
	apiURL     string        // 代理API地址
	refresh    time.Duration // 刷新间隔
	proxies    []string      // 当前代理列表
	client     *http.Client
	mu         sync.RWMutex
	index      uint64
	trackQueue *pkg2.MsgQueue[any] //系统消息队列
}

// NewManager 创建代理管理器
func NewManager(apiURL string, refreshInterval time.Duration) *Manager {
	m := &Manager{
		apiURL:     apiURL,
		refresh:    refreshInterval,
		client:     &http.Client{Timeout: 10 * time.Second},
		trackQueue: pkg2.NewMsgQueue[any](10),
	}
	if apiURL != "" {
		m.refreshProxies()
		go m.startRefresh()
	}
	return m
}

// Next 位移获取一条proxy
func (m *Manager) Next() string {
	m.mu.RLock()
	proxies := m.proxies
	m.mu.RUnlock()
	if len(proxies) == 0 {
		return ""
	}
	if m.index >= uint64(len(proxies)) {
		m.index = 0
	}
	idx := atomic.AddUint64(&m.index, 1) - 1
	return proxies[idx%uint64(len(proxies))]
}

// GetErrors 获取错误消息队列
func (m *Manager) GetErrors() <-chan error {
	return m.trackQueue.Errors()
}

// refreshProxies 从 API 获取代理列表
func (m *Manager) refreshProxies() {
	req, err := http.NewRequest("GET", m.apiURL, nil)
	if err != nil {
		m.trackQueue.SendError(fmt.Errorf("create proxy request failed: %s", err.Error()))
		return
	}
	resp, err := m.client.Do(req)
	if err != nil {
		m.trackQueue.SendError(fmt.Errorf("fetch proxies failed: %w", err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		m.trackQueue.SendError(fmt.Errorf("proxy API returned status %d", resp.StatusCode))
		return
	}
	//===============接受数据结构按照三方返回做调整===============/
	var proxies []map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&proxies); err != nil {
		m.trackQueue.SendError(fmt.Errorf("decode proxy list failed: %w", err))
		return
	}
	m.mu.Lock()
	m.proxies = make([]string, 0, len(proxies))
	var list []string
	for _, p := range proxies {
		list = append(list, "http://"+p["host"]+":"+p["port"])
	}
	m.proxies = list
	m.mu.Unlock()
	m.trackQueue.SendError(fmt.Errorf("updated proxies: %d available", len(m.proxies)))
}

// startRefresh 定时刷新
func (m *Manager) startRefresh() {
	ticker := time.NewTicker(m.refresh)
	defer ticker.Stop()
	for range ticker.C {
		m.refreshProxies()
	}
}
