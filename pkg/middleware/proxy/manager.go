package proxy

import (
	"encoding/json"
	"fmt"
	"github.com/ydtg1993/papa/v3/internal/msgqueue"
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
	trackQueue *msgqueue.MsgQueue[any] //系统消息队列
}

// NewManager 创建代理管理器
func NewManager(apiURL string, refreshInterval time.Duration) *Manager {
	m := &Manager{
		apiURL:     apiURL,
		refresh:    refreshInterval,
		client:     &http.Client{Timeout: 10 * time.Second},
		trackQueue: msgqueue.NewMsgQueue[any](10),
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
	proxyCount := len(m.proxies)
	defer m.mu.RUnlock()
	if proxyCount == 0 {
		return ""
	}
	idx := atomic.AddUint64(&m.index, 1) - 1
	proxyAddress := m.proxies[idx%uint64(proxyCount)]
	return proxyAddress
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
	// 直接建好再一次性换上去：原来这里先 make 一次 m.proxies，紧接着又被 list 覆盖，
	// 那次分配是死的（写下来免得下次有人以为它是"先清空再填"）。
	list := make([]string, 0, len(proxies))
	for _, p := range proxies {
		list = append(list, "http://"+p["host"]+":"+p["port"])
	}
	m.proxies = list
	empty := len(list) == 0
	m.mu.Unlock()

	// 只报「拉到了但一条可用都没有」：出口会全部变成直连，目标站可能因此封 IP，
	// 而这件事在别处看不出来（Next 只是返回空串）—— 这是需要人看的情况。
	// 非空不报：每轮刷新（默认 8 分钟）写一条 "N available" 是把正常当异常，
	// 而错误通道那端按 Error 级别落盘（App.mdMsgListener），正常运行的日志里会一直有 ERROR。
	if empty {
		m.trackQueue.SendError(fmt.Errorf("proxy API 返回空列表：本进程将直连，不走代理"))
	}
}

// startRefresh 定时刷新
func (m *Manager) startRefresh() {
	ticker := time.NewTicker(m.refresh)
	defer ticker.Stop()
	for range ticker.C {
		m.refreshProxies()
	}
}
