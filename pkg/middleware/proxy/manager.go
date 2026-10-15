package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ydtg1993/papa/v3/internal/msgqueue"
)

// defaultRefreshInterval 调用方传非正刷新间隔时兜底的默认值（与历史默认一致的 8 分钟）。
// 非正间隔会让 time.NewTicker panic，而且是在后台 goroutine 里、进程直接崩。
const defaultRefreshInterval = 8 * time.Minute

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
		// 非正刷新间隔会让 time.NewTicker panic（而且是在 goroutine 里、进程直接崩），
		// 这里用默认值兜底，别把"启动 goroutine 前才触发 panic"留给调用方。
		if m.refresh <= 0 {
			m.refresh = defaultRefreshInterval
		}
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
	// 代理 API 返回 JSON 数组，每条形如 {"host":"1.2.3.4","port":"8080"} —— 按第三方返回调整这里的字段名。
	var proxies []map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&proxies); err != nil {
		m.trackQueue.SendError(fmt.Errorf("decode proxy list failed: %w", err))
		return
	}
	m.mu.Lock()
	// 整份建好再一次性换上去：换的那一刻 m.proxies 要么是旧的、要么是新的，
	// 不会出现半满的中间态（读侧只持 m.mu，不做拷贝）。
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
