package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Manager 代理管理器，从 API 获取代理并缓存
type Manager struct {
	apiURL  string        // 代理API地址
	refresh time.Duration // 刷新间隔
	proxies []string      // 当前代理列表
	client  *http.Client
	mu      sync.RWMutex
	index   uint64
	errors  chan error //错误消息队列
}

// NewManager 创建代理管理器
func NewManager(apiURL string, refreshInterval time.Duration) *Manager {
	m := &Manager{
		apiURL:  apiURL,
		refresh: refreshInterval,
		client:  &http.Client{Timeout: 10 * time.Second},
		errors:  make(chan error, 10),
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
	return m.errors
}

// refreshProxies 从 API 获取代理列表
func (m *Manager) refreshProxies() {
	req, err := http.NewRequest("GET", m.apiURL, nil)
	if err != nil {
		m.sendError(fmt.Errorf("create proxy request failed: %v", err))
		return
	}
	resp, err := m.client.Do(req)
	if err != nil {
		m.sendError(fmt.Errorf("fetch proxies failed: %v", err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		m.sendError(fmt.Errorf("proxy API returned status %d", resp.StatusCode))
		return
	}

	var list []string
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		m.sendError(fmt.Errorf("decode proxy list failed: %v", err))
		return
	}
	m.mu.Lock()
	m.proxies = make([]string, 0, len(list))
	for _, url := range list {
		m.proxies = append(m.proxies, url)
	}
	m.mu.Unlock()
	m.sendError(fmt.Errorf("updated proxies: %d available", len(m.proxies)))
}

// startRefresh 定时刷新
func (m *Manager) startRefresh() {
	ticker := time.NewTicker(m.refresh)
	defer ticker.Stop()
	for range ticker.C {
		m.refreshProxies()
	}
}

// sendError非阻塞发送错误
func (m *Manager) sendError(err error) {
	select {
	case m.errors <- err:
	default:
		break
	}
}
