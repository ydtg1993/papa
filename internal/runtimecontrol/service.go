package runtimecontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/governance"
	"github.com/ydtg1993/papa/v4/pkg/middleware/proxy"
	"reflect"
	"sync"
	"time"
)

// Runtime is the execution capability needed for policy validation/publication.
type Runtime interface {
	ResolveRuntime(config.RuntimeConfig) (governance.Config, map[string]time.Duration, error)
	Governance() *governance.Controller
	InstallGovernance(*governance.Controller, map[string]time.Duration)
	GetProxy() *proxy.Manager
	Site(string) (core.Site, bool)
}

// Store owns durable CAS. Adapter translates persistence-specific errors.
type Store interface {
	Load(context.Context) (json.RawMessage, uint64, error)
	Save(context.Context, Document, uint64, string) (uint64, error)
	History(context.Context) ([]models.RuntimePolicyHistory, error)
	HistoryRevision(context.Context, uint64) (models.RuntimePolicyHistory, error)
}

var ErrConflict = errors.New("配置已被其他操作更新，请核对最新版本")
var ErrNotFound = errors.New("历史版本不存在")

type Kind uint8

const (
	Success Kind = iota
	Invalid
	Conflict
	Unavailable
	NotFound
)

type Event struct {
	Action, ID string
	Err        error
}

// Result describes a domain outcome; transport adapters choose status codes.
type Result struct {
	Kind   Kind
	Value  map[string]any
	Events []Event
}

func result(k Kind, v map[string]any) Result { return Result{Kind: k, Value: v} }
func failure(k Kind, msg string) Result      { return result(k, map[string]any{"error": msg}) }

type Service struct {
	mu        sync.Mutex
	revision  uint64
	uncertain bool
	document  Document
	effective governance.Config
	pools     *account.Pools
	runtime   Runtime
	store     Store
	base      config.RuntimeConfig
}

func New(runtime Runtime, store Store, base config.RuntimeConfig) *Service {
	base = cloneRuntime(base)
	cfg, _, _ := runtime.ResolveRuntime(config.RuntimeConfig{})
	return &Service{runtime: runtime, store: store, base: base, document: Document{SchemaVersion: 1}, effective: cfg}
}

// AttachPools runs during assembly, before requests are admitted.
func (s *Service) AttachPools(p *account.Pools) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pools = p
	return s.validatePools(s.effective)
}
func (s *Service) Revision() uint64 { s.mu.Lock(); defer s.mu.Unlock(); return s.revision }
func (s *Service) Uncertain() bool  { s.mu.Lock(); defer s.mu.Unlock(); return s.uncertain }

// Initialize loads and publishes persisted configuration before the sampler starts.
func (s *Service) Initialize(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, rev, err := s.store.Load(ctx)
	if err != nil {
		return err
	}
	doc, err := Decode(raw)
	if err != nil {
		return fmt.Errorf("持久策略无效: %w", err)
	}
	cfg, delays, err := s.runtime.ResolveRuntime(doc.Overrides)
	if err != nil {
		return err
	}
	health := doc.Overrides.ProxyHealth.Apply(s.base.ProxyHealth.Apply(proxy.DefaultHealthConfig()))
	if err = health.Validate(); err != nil {
		return err
	}
	c, err := governance.New(cfg)
	if err != nil {
		return err
	}
	if m := s.runtime.GetProxy(); m != nil {
		if err = m.ConfigureHealth(health); err != nil {
			return err
		}
	}
	s.runtime.InstallGovernance(c, delays)
	s.document = doc
	s.revision = rev
	s.effective = cfg
	s.uncertain = false
	return nil
}
func (s *Service) viewLocked() map[string]any {
	base, _, _ := s.runtime.ResolveRuntime(config.RuntimeConfig{})
	// Documents contain pointers/maps; never return service-owned mutable values.
	raw, _ := json.Marshal(s.document)
	doc, _ := Decode(raw)
	cfgRaw, _ := json.Marshal(s.effective)
	var effective governance.Config
	_ = json.Unmarshal(cfgRaw, &effective)
	view := map[string]any{"revision": s.revision, "document": doc, "base": base, "effective": effective, "proxy_policy": doc.Overrides.ProxyHealth.Apply(s.base.ProxyHealth.Apply(proxy.DefaultHealthConfig())), "proxy_base": s.base.ProxyHealth.Apply(proxy.DefaultHealthConfig()), "file_overrides": cloneRuntime(s.base)}
	// Capture policy-dependent snapshots before releasing the publication lock.
	// The HTTP adapter may add independent metrics, but must not reread policy.
	if c := s.runtime.Governance(); c != nil {
		view["snapshot"] = c.Snapshot()
	}
	if m := s.runtime.GetProxy(); m != nil {
		view["proxies"] = m.Snapshot()
	}
	if s.pools != nil {
		view["pools"] = s.pools.Snapshot()
	}
	return view
}
func (s *Service) Read(ctx context.Context) Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.uncertain {
		if err := s.reconcileLocked(ctx); err != nil {
			return failure(Unavailable, "持久策略结果仍待核对，当前生效策略未放宽")
		}
	}
	return result(Success, s.viewLocked())
}
func (s *Service) Acknowledge(site string, revision uint64) Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	// The cached revision is not authoritative after an unconfirmed write.
	// Require Read to reconcile before accepting a revision-pinned release.
	if s.uncertain {
		return failure(Unavailable, "持久策略结果仍待核对，请刷新后确认远程等待")
	}
	if revision != s.revision {
		return failure(Conflict, "策略版本已变化，请刷新后核对")
	}
	if _, ok := s.runtime.Site(site); !ok {
		return failure(Invalid, "未知站点")
	}
	s.runtime.Governance().AcknowledgeRemote(site)
	r := result(Success, s.viewLocked())
	r.Events = []Event{{Action: "acknowledge_remote", ID: site}}
	return r
}
func (s *Service) validatePools(cfg governance.Config) error {
	for site := range cfg.Sites {
		p := cfg.Effective(site)
		names := []string{p.Pool}
		for _, stage := range p.Stages {
			names = append(names, stage.Pool)
		}
		for _, name := range names {
			if name != "" {
				if s.pools == nil {
					return fmt.Errorf("站点 %s 配置了账号池，但未注册账号管理器", site)
				}
				if _, err := s.pools.Get(account.PoolKey{Site: site, Name: name}); err != nil {
					return fmt.Errorf("未知账号池 %s:%s", site, name)
				}
			}
		}
	}
	return nil
}
func (s *Service) reconcileLocked(ctx context.Context) error {
	raw, revision, err := s.store.Load(ctx)
	if err != nil {
		return err
	}
	if revision < s.revision {
		return errors.New("持久策略版本倒退，拒绝恢复默认")
	}
	doc, err := Decode(raw)
	if err != nil {
		return err
	}
	if revision == s.revision && !reflect.DeepEqual(doc, s.document) {
		return errors.New("同版本持久策略内容发生变化")
	}
	cfg, delays, err := s.runtime.ResolveRuntime(doc.Overrides)
	if err != nil {
		return err
	}
	if err = s.validatePools(cfg); err != nil {
		return err
	}
	health := doc.Overrides.ProxyHealth.Apply(s.base.ProxyHealth.Apply(proxy.DefaultHealthConfig()))
	if err = health.Validate(); err != nil {
		return err
	}
	if m := s.runtime.GetProxy(); m != nil {
		if err = m.ConfigureHealth(health); err != nil {
			return err
		}
	}
	if err = s.runtime.Governance().Update(cfg); err != nil {
		return err
	}
	s.runtime.InstallGovernance(s.runtime.Governance(), delays)
	s.document = doc
	s.revision = revision
	s.effective = cfg
	s.uncertain = false
	return nil
}

type Update struct {
	Revision uint64   `json:"revision"`
	Document Document `json:"document"`
}

func (s *Service) Save(ctx context.Context, input Update, operator string) Result {
	// Detach caller-owned maps and pointer fields before retaining the document.
	raw, err := json.Marshal(input.Document)
	if err != nil {
		return failure(Invalid, err.Error())
	}
	var detached Document
	err = json.Unmarshal(raw, &detached)
	input.Document = detached
	if err != nil {
		return failure(Invalid, err.Error())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.uncertain {
		if err := s.reconcileLocked(ctx); err != nil {
			return result(Unavailable, map[string]any{"error": "前次写入结果仍待核对，拒绝继续写入"})
		}
	}
	if input.Revision != s.revision {
		return result(Conflict, map[string]any{"error": ErrConflict.Error(), "latest": s.viewLocked()})
	}
	if input.Document.SchemaVersion != 1 {
		return result(Invalid, map[string]any{"error": "schema_version 必须为 1"})
	}
	cfg, delays, err := s.runtime.ResolveRuntime(input.Document.Overrides)
	health := input.Document.Overrides.ProxyHealth.Apply(s.base.ProxyHealth.Apply(proxy.DefaultHealthConfig()))
	if err == nil {
		err = health.Validate()
	}
	if err == nil {
		err = s.validatePools(cfg)
	}
	if err != nil {
		return result(Invalid, map[string]any{"error": err.Error()})
	}
	rev, err := s.store.Save(ctx, input.Document, input.Revision, operator)
	if err != nil && !errors.Is(err, ErrConflict) {
		// A lost acknowledgement is reconciled once; never replay the write.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		raw, actual, readErr := s.store.Load(ctx)
		cancel()
		saved, decodeErr := Decode(raw)
		if readErr == nil && decodeErr == nil && actual == input.Revision+1 && reflect.DeepEqual(saved, input.Document) {
			rev = actual
			err = nil
		}
	}
	if err != nil {
		s.uncertain = true

		kind := Unavailable
		if errors.Is(err, ErrConflict) {
			kind = Conflict
		}
		r := result(kind, map[string]any{"error": "写入结果需核对，请刷新服务端版本；未自动重试", "revision": s.revision})
		r.Events = []Event{{Action: "save", Err: errors.New("运行策略持久化未确认")}}
		return r
	}
	// Both update calls validate before mutation; the exact values were validated
	// above, so publication cannot fail after a confirmed database write.
	if m := s.runtime.GetProxy(); m != nil {
		if err := m.ConfigureHealth(health); err != nil {
			s.uncertain = true
			return result(Unavailable, map[string]any{"error": "策略已保存，运行状态尚待核对"})
		}
	}
	_ = s.runtime.Governance().Update(cfg)
	s.runtime.InstallGovernance(s.runtime.Governance(), delays)
	s.revision = rev
	s.document = input.Document
	s.effective = cfg
	s.uncertain = false
	r := result(Success, s.viewLocked())
	r.Events = []Event{{Action: "save", ID: fmt.Sprint(rev)}}
	return r
}

// DeletePool holds the same mutex as validation, CAS and publication. No policy
// can begin referencing the pool between this check and its durable deletion.
func (s *Service) DeletePool(ctx context.Context, key account.PoolKey, revision uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pools == nil {
		return errors.New("业务尚未注册账号管理器")
	}
	// An unconfirmed save may already reference this pool durably. Reconcile
	// under the same lock before checking references, or fail closed.
	if s.uncertain {
		if err := s.reconcileLocked(ctx); err != nil {
			return fmt.Errorf("持久策略结果仍待核对，拒绝删除账号池: %w", err)
		}
	}
	for site := range s.effective.Sites {
		if site != key.Site {
			continue
		}
		p := s.effective.Effective(site)
		if p.Pool == key.Name {
			return account.ErrPoolReferenced
		}
		for _, stage := range p.Stages {
			if stage.Pool == key.Name {
				return account.ErrPoolReferenced
			}
		}
	}
	return s.pools.Delete(ctx, key, revision)
}

const HistoryLimit = 100

func (s *Service) History(ctx context.Context) Result {
	rows, err := s.store.History(ctx)
	if err != nil {
		return failure(Unavailable, "策略历史读取失败，请检查数据库及迁移")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return result(Success, map[string]any{"history": rows, "revision": s.revision, "retention": HistoryLimit})
}

type HistoryRequest struct {
	Action   string     `json:"action"`
	Revision uint64     `json:"revision"`
	Target   uint64     `json:"target"`
	Paths    [][]string `json:"paths"`
}

func (s *Service) HistoryAction(ctx context.Context, input HistoryRequest, operator string) Result {
	row, err := s.store.HistoryRevision(ctx, input.Target)
	if err != nil {
		k := Unavailable
		if errors.Is(err, ErrNotFound) {
			k = NotFound
		}
		return failure(k, "历史版本不可用，请刷新历史列表")
	}
	target, err := Decode(row.Document)
	if err != nil {
		return failure(Invalid, "历史版本格式已不兼容")
	}
	s.mu.Lock()
	if s.uncertain {
		err = s.reconcileLocked(ctx)
	}
	if err != nil || s.revision != input.Revision {
		s.mu.Unlock()
		return failure(Conflict, "当前版本已变化或待核对，请重新预览")
	}
	changes := Changes(s.document, target)
	if input.Action == "preview" {
		effective, _, resolveErr := s.runtime.ResolveRuntime(target.Overrides)
		var poolErr error
		if resolveErr == nil {
			poolErr = s.validatePools(effective)
		}
		warning := ""
		if resolveErr != nil || poolErr != nil {
			warning = "完整历史设置与当前站点或账号池不兼容；请仅选择仍适用的字段，保存时重新校验。"
		}
		v := map[string]any{"revision": s.revision, "target": input.Target, "changes": changes, "effective": effective, "warning": warning}
		s.mu.Unlock()
		return result(Success, v)
	}
	document, err := PatchHistory(s.document, target, input.Paths)
	s.mu.Unlock()
	if err != nil {
		return failure(Invalid, err.Error())
	}
	// The pinned revision rejects any edit between patch calculation and save.
	r := s.Save(ctx, Update{Revision: input.Revision, Document: document}, operator)
	r.Events = append([]Event{{Action: "restore_requested", ID: fmt.Sprint(input.Target)}}, r.Events...)
	return r
}

func cloneRuntime(v config.RuntimeConfig) config.RuntimeConfig {
	raw, _ := json.Marshal(v)
	var copy config.RuntimeConfig
	_ = json.Unmarshal(raw, &copy)
	return copy
}
