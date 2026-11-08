package engine

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/pkg/discovery"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
	"github.com/ydtg1993/papa/v4/pkg/rulequality"
)

func NewRuleFetcher(cfg RuleFetcherConfig) (*RuleFetcher, error) {
	if strings.TrimSpace(cfg.Site) == "" || strings.TrimSpace(cfg.Stage) == "" || (cfg.Registry == nil && cfg.Catalog == nil) || cfg.Save == nil {
		return nil, fmt.Errorf("rule fetcher: site, stage, registry/catalog and idempotent Save required")
	}
	if cfg.Catalog != nil && (cfg.Registry != nil || len(cfg.Bindings) != 0) {
		return nil, fmt.Errorf("rule fetcher: Catalog is mutually exclusive with Registry and static Bindings")
	}
	if utf8.RuneCountInString(cfg.Site) > 128 || utf8.RuneCountInString(cfg.Stage) > 128 {
		return nil, fmt.Errorf("rule fetcher: site and stage exceed database column limits")
	}
	if cfg.Transport == "" {
		cfg.Transport = "http"
	}
	if cfg.Transport != "http" && cfg.Transport != "browser" {
		return nil, fmt.Errorf("rule fetcher: unsupported transport")
	}
	if cfg.Transport == "browser" && cfg.BuildRequest != nil {
		return nil, fmt.Errorf("rule fetcher: browser does not accept BuildRequest")
	}
	if cfg.HTTPSource != nil && (cfg.Transport != "http" || cfg.AccountID != "") {
		return nil, fmt.Errorf("rule fetcher: HTTPSource requires anonymous HTTP transport")
	}
	if len(cfg.Origins) == 0 || len(cfg.Origins) > 32 || (cfg.Catalog == nil && len(cfg.Bindings) == 0) || len(cfg.Bindings) > 32 {
		return nil, fmt.Errorf("rule fetcher: bounded origins and bindings required")
	}
	cfg.Origins = slices.Clone(cfg.Origins)
	cfg.Bindings = maps.Clone(cfg.Bindings)
	cfg.Links = slices.Clone(cfg.Links)
	if cfg.SnapshotPolicy != nil {
		policy := *cfg.SnapshotPolicy
		policy.Headers = nil // response/request headers are not part of snapshots
		policy.QueryKeys = slices.Clone(policy.QueryKeys)
		cfg.SnapshotPolicy = &policy
	}
	for i, origin := range cfg.Origins {
		u, err := discovery.CanonicalURL(origin, "")
		if err != nil {
			return nil, fmt.Errorf("rule fetcher: invalid origin")
		}
		cfg.Origins[i] = u
	}
	if cfg.MaxPages == 0 {
		cfg.MaxPages = 100
	}
	if cfg.MaxItems == 0 {
		cfg.MaxItems = 10000
	}
	if cfg.MaxPages < 1 || cfg.MaxPages > 1000 || cfg.MaxItems < 1 || cfg.MaxItems > 10000 {
		return nil, fmt.Errorf("rule fetcher: invalid pagination budgets")
	}
	if cfg.MaxChildren == 0 {
		cfg.MaxChildren = 1000
	}
	if cfg.MaxChildren < 1 || cfg.MaxChildren > 10000 {
		return nil, fmt.Errorf("rule fetcher: invalid child task budget")
	}
	if _, ok := cfg.Bindings[cfg.Stage]; !ok && cfg.Catalog == nil {
		return nil, fmt.Errorf("rule fetcher: stage binding required")
	}
	for stage, ref := range cfg.Bindings {
		if stage == "" || len(stage) > 128 || len(ref.ID) > 256 || len(ref.Version) > 256 {
			return nil, fmt.Errorf("rule fetcher: invalid binding")
		}
		c, err := cfg.Registry.Verified(ref.ID, ref.Version)
		if err != nil {
			return nil, err
		}
		if stage == cfg.Stage && cfg.Transport == "browser" && c.Rule().Format != "html" {
			return nil, fmt.Errorf("rule fetcher: browser requires HTML rule")
		}
	}
	if len(cfg.Links) > 32 {
		return nil, fmt.Errorf("rule fetcher: too many link mappings")
	}
	for i, link := range cfg.Links {
		if link.Field == "" || link.Stage == "" || link.Stage == cfg.Stage {
			return nil, fmt.Errorf("rule fetcher: link field and different target stage required")
		}
		if _, ok := cfg.Bindings[link.Stage]; !ok && cfg.Catalog == nil {
			return nil, fmt.Errorf("rule fetcher: link stage has no binding")
		}
		for key, field := range link.Meta {
			if key == "" || field == "" || ruleReserved(key) {
				return nil, fmt.Errorf("rule fetcher: invalid link metadata mapping")
			}
		}
		cfg.Links[i].Meta = maps.Clone(link.Meta)
	}
	return &RuleFetcher{config: cfg}, nil
}

func (f *RuleFetcher) GetStage() string { return f.config.Stage }
func (f *RuleFetcher) FetchHandler(ctx context.Context, task *Task, e *Engine) error {
	if e == nil {
		return core.WrapNoRetry(fmt.Errorf("rule fetcher: engine required"))
	}
	if task.FlowID != "" {
		return f.Run(ctx, task, e, func(child *Task) error { return e.SubmitFlowTask(ctx, task.FlowID, uint(task.ID), child) })
	}
	return f.Run(ctx, task, e, e.SubmitTask)
}

// RunHTTP embeds the HTTP workflow in a caller-owned queue without constructing
// a database-backed Engine. It has no account manager, browser or background
// workers; production engine queues should call FetchHandler instead.
func (f *RuleFetcher) RunHTTP(ctx context.Context, task *Task, client *htmlfetch.Client, submit func(*Task) error) error {
	if client == nil || f.config.Transport != "http" || task == nil || task.Meta[ruleAccountMeta] != "" {
		return core.WrapNoRetry(fmt.Errorf("rule fetcher: RunHTTP requires anonymous HTTP task and client"))
	}
	return f.Run(ctx, task, &Engine{htmlClient: client}, submit)
}

// Run executes the same handler with an explicit enqueue function. Production
// FetchHandler uses Engine.SubmitTask; embedded/offline demos can use a bounded
// local queue. A nil error from submit must mean the task is durably accepted or
// already exists. The business Save and queue are not one atomic transaction.
func (f *RuleFetcher) Run(ctx context.Context, task *Task, e *Engine, submit func(*Task) error) (runErr error) {
	if e == nil || submit == nil {
		return core.WrapNoRetry(fmt.Errorf("rule fetcher: engine and submit required"))
	}
	req, bindings, cp, err := f.taskState(task)
	if err != nil {
		return core.WrapNoRetry(err)
	}
	requestHash := ruleDigest([]byte(ruleJSON(req)))
	ref := bindings[task.Stage]
	ctx, quality := f.beginRuleQuality(ctx, task, ref, requestHash, len(cp.Requests)+1)
	if quality != nil {
		defer func() {
			p := recover()
			quality.finish(ctx, runErr, p != nil)
			if p != nil {
				panic(p)
			}
		}()
	}
	// Registry availability used to be checked while rebuilding task identity.
	// Keep its error/no-retry precedence over pagination stops, but record it
	// only after the pinned metadata and identity have independently validated.
	if f.config.Registry != nil {
		if _, err := f.config.Registry.Verified(ref.ID, ref.Version); err != nil {
			return core.WrapNoRetry(err)
		}
	}
	if slices.Contains(cp.Requests, requestHash) {
		quality.stop("request-cycle")
		task.Trace.Step("规则分页停止", map[string]any{"reason": "request-cycle"})
		return nil
	}
	compiled, err := f.resolveRule(ctx, ref)
	if err != nil {
		return err
	}
	if err := f.checkRuleContract(compiled); err != nil {
		return core.WrapNoRetry(err)
	}
	if quality != nil {
		quality.value.FieldSchemaSHA256 = rulequality.FieldSchemaDigest(compiled.Rule())
	}
	ctx = f.beginRuleSnapshot(ctx, task, req, compiled.Rule().Format)
	quality.phase("source", rulequality.FailureRule)
	page, err := f.readRulePage(ctx, cloneRuleTask(task), e, req, compiled)
	if err != nil {
		return err
	}
	page.EvidenceID = ruleSnapshotID(ctx)
	release, _ := decodeRuleRelease(task.Meta[ruleReleaseMeta]) // checked by taskState
	page.ReleaseID, page.ReleaseGeneration = release.ID, release.Generation
	page.Rule = ref
	page.Page = len(cp.Requests) + 1
	if slices.Contains(cp.Bodies, page.BodySHA256) {
		quality.stop("repeated-content")
		task.Trace.Step("规则分页停止", map[string]any{"reason": "repeated-content"})
		return nil
	}
	cp.Requests = append(cp.Requests, requestHash)
	cp.Bodies = append(cp.Bodies, page.BodySHA256)
	// Keep only novel, budgeted records; the original extraction was validated
	// before filtering. Evidence indices are remapped to the retained rows.
	// Extraction validation already passed. A missing ItemKey breaks this
	// fetcher's processing contract, not the parser's validation result.
	quality.phase("deduplicate", rulequality.FailureRule)
	keep := map[int]int{}
	records := page.Result.Records[:0]
	for i, record := range page.Result.Records {
		var identity any = record
		if f.config.ItemKey != "" {
			var ok bool
			identity, ok = record[f.config.ItemKey]
			if !ok || identity == nil || identity == "" {
				return core.WrapNoRetry(fmt.Errorf("rule fetcher: item key missing"))
			}
		}
		key := ruleDigest([]byte(ruleJSON(identity)))
		if slices.Contains(cp.Items, key) {
			continue
		}
		if len(cp.Items) >= cp.MaxItems {
			break
		}
		cp.Items = append(cp.Items, key)
		keep[i] = len(records)
		records = append(records, record)
	}
	page.Result.Records = records
	if quality != nil {
		quality.value.NewRows = len(records)
	}
	page.Result.Validation.Rows = len(records)
	evidence := page.Result.Evidence[:0]
	for _, ev := range page.Result.Evidence {
		if index, ok := keep[ev.Record]; ok {
			ev.Record = index
			evidence = append(evidence, ev)
		}
	}
	page.Result.Evidence = evidence
	if len(records) == 0 {
		quality.stop("no-new-items")
		task.Trace.Step("规则分页停止", map[string]any{"reason": "no-new-items"})
		return nil
	}
	var next *RuleRequest
	quality.phase("planning", rulequality.FailurePlanning)
	if len(cp.Items) >= cp.MaxItems {
		page.StopReason = "item-limit"
	} else if len(cp.Requests) >= cp.MaxPages {
		page.StopReason = "page-limit"
	} else {
		if f.config.Next != nil {
			next, err = f.config.Next(ctx, cloneRuleTask(task), cloneRulePage(page))
			if err != nil {
				return ruleSourceFailure("next request planning", err)
			}
		} else if page.Result.NextURL != "" {
			next = &RuleRequest{URL: page.Result.NextURL}
		} else if page.Result.NextCursor != "" {
			next = &RuleRequest{URL: req.URL, State: page.Result.NextCursor}
		}
		if next == nil {
			page.StopReason = "no-next-page"
		} else {
			copied := *next
			next = &copied
			next.URL, err = f.allowedURL(next.URL)
			if err != nil {
				return core.WrapNoRetry(err)
			}
			if slices.Contains(cp.Requests, ruleDigest([]byte(ruleJSON(next)))) {
				next = nil
				page.StopReason = "request-cycle"
			}
		}
	}
	quality.stop(page.StopReason)
	// Resolve linked stages once before Save. A database outage must not become
	// a permanent planning failure or allow partially accepted business output.
	if f.config.Catalog != nil {
		for _, link := range f.config.Links {
			if _, err := f.resolveRule(ctx, bindings[link.Stage]); err != nil {
				quality.phase("planning", rulequality.FailureRule)
				return err
			}
		}
	}
	children, err := f.ruleChildren(ctx, task, bindings, cp, page, next)
	if err != nil {
		return core.WrapNoRetry(err)
	}
	if quality != nil {
		quality.value.PlannedChildren = len(children)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	quality.phase("save", rulequality.FailureSave)
	if err := f.config.Save(ctx, cloneRuleTask(task), page); err != nil {
		return fmt.Errorf("rule fetcher: save: %w", err)
	}
	for _, child := range children {
		quality.phase("submit", rulequality.FailureSubmit)
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := submit(child); err != nil {
			return fmt.Errorf("rule fetcher: submit child: %w", err)
		}
		if quality != nil {
			quality.value.SubmittedChildren++
		}
	}
	task.Trace.Step("规则采集完成", map[string]any{"rule_id": ref.ID, "rule_version": ref.Version, "records": len(records), "page": page.Page, "children": len(children), "stop_reason": page.StopReason})
	return nil
}

// Next is a planning hook, not a transformation hook. Its snapshot cannot alter
// validated records, evidence or the item identities already checkpointed.
func cloneRulePage(page RulePage) RulePage {
	var cloneValue func(any) any
	cloneValue = func(value any) any {
		switch v := value.(type) {
		case map[string]any:
			out := make(map[string]any, len(v))
			for k, item := range v {
				out[k] = cloneValue(item)
			}
			return out
		case []any:
			out := make([]any, len(v))
			for i, item := range v {
				out[i] = cloneValue(item)
			}
			return out
		default:
			return v
		}
	}
	page.Result.Records = slices.Clone(page.Result.Records)
	for i, record := range page.Result.Records {
		page.Result.Records[i] = cloneValue(record).(map[string]any)
	}
	page.Result.Evidence = slices.Clone(page.Result.Evidence)
	for i := range page.Result.Evidence {
		page.Result.Evidence[i].Steps = slices.Clone(page.Result.Evidence[i].Steps)
	}
	page.Result.Validation.Issues = slices.Clone(page.Result.Validation.Issues)
	return page
}

func (f *RuleFetcher) ruleChildren(ctx context.Context, task *Task, bindings map[string]RuleRef, cp ruleCheckpoint, page RulePage, next *RuleRequest) ([]*Task, error) {
	children := []*Task{}
	seen := map[string]bool{}
	plannedBytes := 0
	add := func(t *Task) error {
		if !seen[t.URL+"\x00"+t.Stage] {
			plannedBytes += len(t.URL) + len(ruleJSON(t.Meta))
			if len(children) >= f.config.MaxChildren || plannedBytes > 8<<20 {
				ruleQualityFrom(ctx).phase("planning", rulequality.FailureBudget)
				return fmt.Errorf("rule fetcher: child plan exceeds task or metadata budget")
			}
			seen[t.URL+"\x00"+t.Stage] = true
			t.PID = task.ID
			children = append(children, t)
		}
		return nil
	}
	for _, record := range page.Result.Records {
		for _, link := range f.config.Links {
			raw, ok := record[link.Field].(string)
			if !ok || raw == "" {
				return nil, fmt.Errorf("rule fetcher: detail link missing or not a string")
			}
			resolved, err := discovery.CanonicalURL(page.SourceURL, raw)
			if err != nil {
				return nil, fmt.Errorf("rule fetcher: invalid detail link")
			}
			meta := map[string]string{}
			if batch := task.Meta[ruleBatchMeta]; batch != "" {
				meta[ruleBatchMeta] = batch
			}
			if release := task.Meta[ruleReleaseMeta]; release != "" {
				meta[ruleReleaseMeta] = release
			}
			for key, field := range link.Meta {
				value, ok := record[field]
				if !ok || value == nil {
					return nil, fmt.Errorf("rule fetcher: mapped detail metadata missing")
				}
				switch v := value.(type) {
				case string:
					meta[key] = v
				default:
					meta[key] = ruleJSON(v)
				}
			}
			child, err := f.makeTask(link.Stage, RuleRequest{URL: resolved}, meta, bindings, task.Meta[ruleAccountMeta], ruleCheckpoint{MaxPages: cp.MaxPages, MaxItems: cp.MaxItems})
			if err != nil {
				return nil, err
			}
			if err := add(child); err != nil {
				return nil, err
			}
		}
	}
	if next != nil {
		child, err := f.makeTask(task.Stage, *next, task.Meta, bindings, task.Meta[ruleAccountMeta], cp)
		if err != nil {
			return nil, err
		}
		if err := add(child); err != nil {
			return nil, err
		}
	}
	return children, nil
}
