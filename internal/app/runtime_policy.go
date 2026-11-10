package app

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sync"
	"time"

	"github.com/ydtg1993/papa/v4/admin/auth"
	"github.com/ydtg1993/papa/v4/admin/configstore"
	"github.com/ydtg1993/papa/v4/admin/oplog"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/governance"
	"github.com/ydtg1993/papa/v4/pkg/middleware/proxy"
)

//go:embed runtime_control.js
var runtimeControlScript string

//go:embed account_pools.js
var accountPoolsScript string

type runtimeDocument struct {
	SchemaVersion int                  `json:"schema_version"`
	Overrides     config.RuntimeConfig `json:"overrides"`
}
type runtimeState struct {
	mu        sync.Mutex
	revision  uint64
	uncertain bool
	document  runtimeDocument
	effective governance.Config
	pools     *account.Pools
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

func decodeRuntime(raw []byte) (runtimeDocument, error) {
	doc := runtimeDocument{SchemaVersion: 1}
	if len(raw) == 0 {
		return doc, nil
	}
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
		return doc, errors.New("运行策略必须是 JSON 对象")
	}
	if err := strictJSON(bytes.NewReader(raw), &doc); err != nil {
		return doc, err
	}
	if doc.SchemaVersion != 1 {
		return doc, fmt.Errorf("不支持的运行策略 schema_version %d", doc.SchemaVersion)
	}
	return doc, nil
}
func strictJSON(r io.Reader, v any) error {
	d := json.NewDecoder(r)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("请求必须包含一个 JSON 对象")
	}
	return nil
}
func (a *App) initRuntime(ctx context.Context) error {
	if err := a.Engine.CheckRuntimeSchema(ctx); err != nil {
		return err
	}
	raw, rev, err := a.configStore.LoadVersioned(ctx, configstore.KeyRuntimePolicy)
	if err != nil {
		return err
	}
	doc, err := decodeRuntime(raw)
	if err != nil {
		return fmt.Errorf("持久策略无效: %w", err)
	}
	cfg, delays, err := a.Engine.ResolveRuntime(doc.Overrides)
	if err != nil {
		return err
	}
	health := doc.Overrides.ProxyHealth.Apply(a.Config.Runtime.ProxyHealth.Apply(proxy.DefaultHealthConfig()))
	if err := health.Validate(); err != nil {
		return err
	}
	c, err := governance.New(cfg)
	if err != nil {
		return err
	}
	if m := a.Engine.GetProxy(); m != nil {
		if err := m.ConfigureHealth(health); err != nil {
			return err
		}
	}
	a.Engine.InstallGovernance(c, delays)
	s := &a.runtimeState
	s.document = doc
	s.revision = rev
	s.effective = cfg
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	if err := a.initBrowserIdentities(); err != nil {
		cancel()
		return err
	}
	if a.accountOnboarding != nil {
		if err := a.accountOnboarding.Load(runCtx); err != nil {
			cancel()
			return fmt.Errorf("账号资料加载失败（请确认 migrate 与站点适配器）: %w", err)
		}
	}
	if m := a.Engine.GetAccountManager(); m != nil {
		if existing := m.Pools(); existing != nil {
			s.pools = existing
		} else {
			s.pools, err = account.NewPools(runCtx, m, account.NewGORMPoolStore(a.DB), account.PoolOptions{References: a.Engine.AccountPoolReferences, Context: func(ctx context.Context, key account.Key) context.Context {
				return a.Engine.RuntimeContext(ctx, key.Site)
			}})
			if err != nil {
				cancel()
				return fmt.Errorf("账号池状态加载失败（请确认 migrate）: %w", err)
			}
		}
	}
	if err := a.validateRuntimePools(cfg); err != nil {
		a.stopRuntime()
		return err
	}
	a.Engine.BindAccountHealthEvents(s.pools)
	s.wg.Add(1)
	go func() { defer s.wg.Done(); _ = c.RunSampler(runCtx, governance.NewSampler()) }()
	return nil
}
func (a *App) stopRuntime() {
	_ = a.drainRuntime(5 * time.Second)
}
func (a *App) drainRuntime(timeout time.Duration) bool {
	s := &a.runtimeState
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if a.accountOnboarding != nil {
		if err := a.accountOnboarding.Shutdown(ctx); err != nil {
			return false
		}
	}
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	if s.pools != nil {
		if err := s.pools.Shutdown(ctx); err != nil {
			return false
		}
	}
	return a.stopBrowserIdentities(ctx)
}
func (a *App) validateRuntimePools(cfg governance.Config) error {
	for site := range cfg.Sites {
		p := cfg.Effective(site)
		names := []string{p.Pool}
		for _, stage := range p.Stages {
			names = append(names, stage.Pool)
		}
		for _, name := range names {
			if name != "" {
				if a.runtimeState.pools == nil {
					return fmt.Errorf("站点 %s 配置了账号池，但未注册账号管理器", site)
				}
				if _, err := a.runtimeState.pools.Get(account.PoolKey{Site: site, Name: name}); err != nil {
					return fmt.Errorf("未知账号池 %s:%s", site, name)
				}
			}
		}
	}
	return nil
}
func writeRuntimeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func (a *App) runtimeAudit(r *http.Request, table, action, id string, err error) {
	if a.oplog != nil {
		a.oplog.RecordEvent(oplog.Event{Table: table, Action: action, RowID: id, Operator: auth.OperatorFrom(r.Context()), IP: r.RemoteAddr, At: time.Now(), Err: err})
	}
}
func (a *App) runtimeViewLocked() map[string]any {
	s := &a.runtimeState
	base, _, _ := a.Engine.ResolveRuntime(config.RuntimeConfig{})
	result := map[string]any{"revision": s.revision, "document": s.document, "base": base, "effective": s.effective, "sites": a.Engine.GetSiteStats(), "stages": a.Engine.GetStageStats(), "coverage": []string{"框架 HTTP、账号 HTTP、文件和 M3U8 请求（须传递任务 context；下载认证参数由业务提供）", "受管浏览器页面：首个文档同源及显式 GovernanceOrigins 的导航、XHR、子资源和重定向", "弹窗、独立 Worker、原始 Rod 页面和覆盖 CDP Fetch 处理器不在受控范围", "业务自行创建且不使用框架出口的客户端不受控"}}
	if c := a.Engine.Governance(); c != nil {
		result["snapshot"] = c.Snapshot()
	}
	result["proxy_policy"] = s.document.Overrides.ProxyHealth.Apply(a.Config.Runtime.ProxyHealth.Apply(proxy.DefaultHealthConfig()))
	result["proxy_base"] = a.Config.Runtime.ProxyHealth.Apply(proxy.DefaultHealthConfig())
	result["declarations"] = a.Engine.RuntimeDeclarations()
	result["file_overrides"] = a.Config.Runtime
	if pool := a.Engine.GetBrowserPool(); pool != nil {
		result["public_browser_pool"] = pool.Stats()
	}
	if m := a.Engine.GetProxy(); m != nil {
		result["proxies"] = m.Snapshot()
	}
	if s.pools != nil {
		result["pools"] = s.pools.Snapshot()
	}
	return result
}

func (a *App) runtimeRemoteAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var input struct {
		Site     string `json:"site"`
		Revision uint64 `json:"revision"`
	}
	if err := strictJSON(http.MaxBytesReader(w, r.Body, 4096), &input); err != nil {
		writeRuntimeJSON(w, 400, map[string]any{"error": "无效请求"})
		return
	}
	a.runtimeState.mu.Lock()
	defer a.runtimeState.mu.Unlock()
	if input.Revision != a.runtimeState.revision {
		writeRuntimeJSON(w, 409, map[string]any{"error": "策略版本已变化，请刷新后核对"})
		return
	}
	if _, ok := a.Engine.Site(input.Site); !ok {
		writeRuntimeJSON(w, 400, map[string]any{"error": "未知站点"})
		return
	}
	a.Engine.Governance().AcknowledgeRemote(input.Site)
	a.runtimeAudit(r, "runtime_policy", "acknowledge_remote", input.Site, nil)
	writeRuntimeJSON(w, 200, a.runtimeViewLocked())
}
func (a *App) runtimePolicyAPI(w http.ResponseWriter, r *http.Request) {
	s := &a.runtimeState
	if r.Method == http.MethodGet {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.uncertain {
			if err := a.reconcileRuntimeLocked(r.Context()); err != nil {
				writeRuntimeJSON(w, 503, map[string]any{"error": "持久策略结果仍待核对，当前生效策略未放宽"})
				return
			}
		}
		writeRuntimeJSON(w, 200, a.runtimeViewLocked())
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var input struct {
		Revision uint64          `json:"revision"`
		Document runtimeDocument `json:"document"`
	}
	if err := strictJSON(http.MaxBytesReader(w, r.Body, 128<<10), &input); err != nil {
		writeRuntimeJSON(w, 400, map[string]any{"error": "运行策略 JSON 无效: " + err.Error()})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.uncertain {
		if err := a.reconcileRuntimeLocked(r.Context()); err != nil {
			writeRuntimeJSON(w, 503, map[string]any{"error": "前次写入结果仍待核对，拒绝继续写入"})
			return
		}
	}
	if input.Revision != s.revision {
		writeRuntimeJSON(w, 409, map[string]any{"error": configstore.ErrConflict.Error(), "latest": a.runtimeViewLocked()})
		return
	}
	if input.Document.SchemaVersion != 1 {
		writeRuntimeJSON(w, 400, map[string]any{"error": "schema_version 必须为 1"})
		return
	}
	cfg, delays, err := a.Engine.ResolveRuntime(input.Document.Overrides)
	health := input.Document.Overrides.ProxyHealth.Apply(a.Config.Runtime.ProxyHealth.Apply(proxy.DefaultHealthConfig()))
	if err == nil {
		err = health.Validate()
	}
	if err == nil {
		err = a.validateRuntimePools(cfg)
	}
	if err != nil {
		writeRuntimeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	rev, err := a.configStore.SaveCAS(r.Context(), configstore.KeyRuntimePolicy, input.Document, input.Revision, auth.OperatorFrom(r.Context()))
	if err != nil && !errors.Is(err, configstore.ErrConflict) {
		// A lost acknowledgement is reconciled once; never replay the write.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		raw, actual, readErr := a.configStore.LoadVersioned(ctx, configstore.KeyRuntimePolicy)
		cancel()
		saved, decodeErr := decodeRuntime(raw)
		if readErr == nil && decodeErr == nil && actual == input.Revision+1 && reflect.DeepEqual(saved, input.Document) {
			rev = actual
			err = nil
		}
	}
	if err != nil {
		s.uncertain = true
		a.runtimeAudit(r, "runtime_policy", "save", "", errors.New("运行策略持久化未确认"))
		code := 503
		if errors.Is(err, configstore.ErrConflict) {
			code = 409
		}
		writeRuntimeJSON(w, code, map[string]any{"error": "写入结果需核对，请刷新服务端版本；未自动重试", "revision": s.revision})
		return
	}
	// Both update calls validate before mutation; the exact values were validated
	// above, so publication cannot fail after a confirmed database write.
	if m := a.Engine.GetProxy(); m != nil {
		if err := m.ConfigureHealth(health); err != nil {
			s.uncertain = true
			writeRuntimeJSON(w, 503, map[string]any{"error": "策略已保存，运行状态尚待核对"})
			return
		}
	}
	_ = a.Engine.Governance().Update(cfg)
	a.Engine.InstallGovernance(a.Engine.Governance(), delays)
	s.revision = rev
	s.document = input.Document
	s.effective = cfg
	s.uncertain = false
	a.runtimeAudit(r, "runtime_policy", "save", fmt.Sprint(rev), nil)
	writeRuntimeJSON(w, 200, a.runtimeViewLocked())
}

// Resolve an uncertain acknowledgement or a CAS winner from durable storage.
// A GET after a timed-out save must not indefinitely report an old cached value.
func (a *App) reconcileRuntimeLocked(ctx context.Context) error {
	s := &a.runtimeState
	raw, revision, err := a.configStore.LoadVersioned(ctx, configstore.KeyRuntimePolicy)
	if err != nil {
		return err
	}
	if revision < s.revision {
		return errors.New("持久策略版本倒退，拒绝恢复默认")
	}
	doc, err := decodeRuntime(raw)
	if err != nil {
		return err
	}
	if revision == s.revision && !reflect.DeepEqual(doc, s.document) {
		return errors.New("同版本持久策略内容发生变化")
	}
	cfg, delays, err := a.Engine.ResolveRuntime(doc.Overrides)
	if err != nil {
		return err
	}
	if err = a.validateRuntimePools(cfg); err != nil {
		return err
	}
	health := doc.Overrides.ProxyHealth.Apply(a.Config.Runtime.ProxyHealth.Apply(proxy.DefaultHealthConfig()))
	if err = health.Validate(); err != nil {
		return err
	}
	if m := a.Engine.GetProxy(); m != nil {
		if err = m.ConfigureHealth(health); err != nil {
			return err
		}
	}
	if err = a.Engine.Governance().Update(cfg); err != nil {
		return err
	}
	a.Engine.InstallGovernance(a.Engine.Governance(), delays)
	s.document = doc
	s.revision = revision
	s.effective = cfg
	s.uncertain = false
	return nil
}
