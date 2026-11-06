package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
)

type ruleReleaseIdentity struct {
	ID         string `json:"id"`
	Generation uint64 `json:"generation"`
}

func decodeRuleRelease(text string) (ruleReleaseIdentity, error) {
	var ref ruleReleaseIdentity
	if text == "" {
		return ref, nil
	} // Original Registry tasks have no release.
	if len(text) > 512 || ruleDecode(text, &ref) != nil || strings.TrimSpace(ref.ID) == "" || len(ref.ID) > 128 || ref.Generation == 0 {
		return ref, fmt.Errorf("rule task: invalid pinned publication identity")
	}
	return ref, nil
}

func (f *RuleFetcher) resolveRule(ctx context.Context, ref RuleRef) (*crawlrule.Compiled, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var c *crawlrule.Compiled
	var err error
	if f.config.Catalog != nil {
		c, err = f.config.Catalog.Resolve(ctx, ref.ID, ref.Version)
	} else {
		c, err = f.config.Registry.Verified(ref.ID, ref.Version)
	}
	if err != nil {
		if errors.Is(err, crawlrule.ErrVersionNotFound) || errors.Is(err, crawlrule.ErrUnverifiedRule) || errors.Is(err, crawlrule.ErrInvalidVersion) {
			return nil, core.WrapNoRetry(err)
		}
		return nil, err // Database/network/context errors retain retry semantics.
	}
	if c == nil || c.Rule().ID != ref.ID || c.Rule().Version != ref.Version {
		return nil, core.WrapNoRetry(fmt.Errorf("rule task: catalog returned a different or missing version"))
	}
	return c, nil
}

func (f *RuleFetcher) checkCatalogBindings(ctx context.Context, bindings map[string]RuleRef) error {
	if len(bindings) == 0 || len(bindings) > 32 {
		return fmt.Errorf("rule task: catalog binding count outside budget")
	}
	if _, ok := bindings[f.config.Stage]; !ok {
		return fmt.Errorf("rule task: catalog is missing current stage")
	}
	for _, link := range f.config.Links {
		if _, ok := bindings[link.Stage]; !ok {
			return fmt.Errorf("rule task: catalog is missing linked stage")
		}
	}
	for stage, ref := range bindings {
		if strings.TrimSpace(stage) == "" || len(stage) > 128 || ref.ID == "" || len(ref.ID) > 128 || ref.Version == "" || len(ref.Version) > 128 {
			return fmt.Errorf("rule task: malformed catalog binding")
		}
		c, err := f.resolveRule(ctx, ref)
		if err != nil {
			return err
		}
		if stage == f.config.Stage {
			if err := f.checkRuleContract(c); err != nil {
				return core.WrapNoRetry(err)
			}
		}
	}
	return ctx.Err()
}

// Publications pin extraction rules, while the deployment still owns transport
// and record mappings. Check their compatibility before creating an entry task
// or performing source work for a restored task; never consult a newer release.
func (f *RuleFetcher) checkRuleContract(c *crawlrule.Compiled) error {
	rule := c.Rule()
	if f.config.Transport == "browser" && rule.Format != "html" {
		return fmt.Errorf("rule fetcher: browser requires HTML rule")
	}
	fields := make(map[string]crawlrule.Field, len(rule.Fields))
	for _, field := range rule.Fields {
		fields[field.Name] = field
	}
	if f.config.ItemKey != "" {
		if _, ok := fields[f.config.ItemKey]; !ok {
			return fmt.Errorf("rule fetcher: pinned rule is missing item key field")
		}
	}
	for _, link := range f.config.Links {
		field, ok := fields[link.Field]
		if !ok {
			return fmt.Errorf("rule fetcher: pinned rule is missing detail link field")
		}
		switch field.Validation.Kind {
		case "any", "string", "http-url":
		default:
			return fmt.Errorf("rule fetcher: pinned detail link field cannot produce a string")
		}
		if n := len(field.Transforms); n > 0 {
			switch field.Transforms[n-1].Op {
			case "number", "bool":
				return fmt.Errorf("rule fetcher: pinned detail link transform cannot produce a string")
			}
		}
		for _, field := range link.Meta {
			if _, ok := fields[field]; !ok {
				return fmt.Errorf("rule fetcher: pinned rule is missing detail metadata field")
			}
		}
	}
	return nil
}
