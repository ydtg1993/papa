package proxy

import (
	"encoding/json"
	"github.com/ydtg1993/papa/internal/loggers"
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
}

// NewManager 创建代理管理器
func NewManager(apiURL string, refreshInterval time.Duration) *Manager {
	m := &Manager{
		apiURL:  apiURL,
		refresh: refreshInterval,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
	if apiURL != "" {
		m.refreshProxies()
		go m.startRefresh()
	}
	return m
}

// refreshProxies 从 API 获取代理列表
func (m *Manager) refreshProxies() {
	req, err := http.NewRequest("GET", m.apiURL, nil)
	if err != nil {
		loggers.ProxyLogger.Printf("create proxy request failed: %v", err)
		return
	}
	resp, err := m.client.Do(req)
	if err != nil {
		loggers.ProxyLogger.Printf("fetch proxies failed: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		loggers.ProxyLogger.Printf("proxy API returned status %d", resp.StatusCode)
		return
	}

	var list []string
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		loggers.ProxyLogger.Printf("decode proxy list failed: %v", err)
		return
	}
	m.mu.Lock()
	m.proxies = make([]string, len(list))
	for _, url := range list {
		m.proxies = append(m.proxies, url)
	}
	m.mu.Unlock()
	loggers.ProxyLogger.Printf("updated proxies: %d available", len(m.proxies))
}

// startRefresh 定时刷新
func (m *Manager) startRefresh() {
	ticker := time.NewTicker(m.refresh)
	defer ticker.Stop()
	for range ticker.C {
		m.refreshProxies()
	}
}

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
