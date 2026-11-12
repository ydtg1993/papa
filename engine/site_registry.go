package engine

import (
	"maps"
	"slices"
	"sort"

	"github.com/ydtg1993/papa/v4/core"
)

// SetSite 登记一份站点声明快照（App.RegisterSites 调）。key 为空时忽略。
func (e *Engine) SetSite(s core.Site) {
	if s.Key == "" {
		return
	}
	e.breakers.breakerMu.Lock()
	defer e.breakers.breakerMu.Unlock()
	e.sites.registerLocked(s)
}

// siteHeaders 取某个站点的请求头（没有返回 nil）。
func (e *Engine) siteHeaders(site string) map[string]string {
	if site == "" {
		return nil
	}
	e.breakers.breakerMu.Lock()
	defer e.breakers.breakerMu.Unlock()
	s, _ := e.sites.snapshotLocked(site)
	return s.Headers
}

// Site 取某个站点的声明快照。handler 里典型用法：
//
//	site, _ := engine.Site(task.Site)   // task.Site 随行落库，重投之后照样认得出站点
//	abs := site.BaseURL + rel
func (e *Engine) Site(key string) (core.Site, bool) {
	e.breakers.breakerMu.Lock()
	defer e.breakers.breakerMu.Unlock()
	return e.sites.snapshotLocked(key)
}

// siteCatalog owns declarations. Its Locked methods require the existing
// breaker coordinator lock: registration and protection keep their old ordering.
// No second mutex is introduced, and no mutable declaration escapes the catalog.
type siteCatalog struct{ entries map[string]core.Site }

func cloneSite(s core.Site) core.Site {
	s.Headers = maps.Clone(s.Headers)
	s.RestrictedKeywords = slices.Clone(s.RestrictedKeywords)
	return s
}
func (c *siteCatalog) registerLocked(s core.Site) {
	if c.entries == nil {
		c.entries = make(map[string]core.Site)
	}
	c.entries[s.Key] = cloneSite(s)
}
func (c *siteCatalog) snapshotLocked(key string) (core.Site, bool) {
	s, ok := c.entries[key]
	return cloneSite(s), ok
}
func (c *siteCatalog) keysLocked(startPausedOnly bool) []string {
	keys := make([]string, 0, len(c.entries))
	for key, s := range c.entries {
		if !startPausedOnly || s.StartPaused {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}
