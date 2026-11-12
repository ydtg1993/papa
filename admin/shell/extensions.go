package shell

import (
	"encoding/json"
	"net/http"
	"strings"
)

// ExtensionPage is the navigation metadata supplied by a host. Script bodies are supplied separately.
type ExtensionPage struct {
	Key, Label, Group, Section string
	Owns                       []string
}

// RegisterExtensions serves host navigation metadata and trusted custom resources.
// Only metadata is guarded: resources must never contain credentials.
func RegisterExtensions(mux *http.ServeMux, wrap func(http.Handler) http.Handler, pages []ExtensionPage, scripts, styles []string) {
	pageList := make([]map[string]any, 0, len(pages))
	for _, p := range pages {
		label, group := p.Label, p.Group
		if label == "" {
			label = p.Key
		}
		if group == "" {
			group = "General"
		}
		section := p.Section
		switch section {
		case "flow", "monitor", "data", "system":
		default:
			section = "data"
		}
		item := map[string]any{"key": p.Key, "label": label, "group": group, "section": section}
		if len(p.Owns) > 0 {
			item["owns"] = append([]string(nil), p.Owns...)
		}
		pageList = append(pageList, item)
	}
	jsBody := strings.Join(scripts, "\n;\n")
	cssBody := strings.Join(styles, "\n")

	mux.Handle("/static/custom.js", extensionNoCache(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		_, _ = w.Write([]byte(jsBody))
	})))
	mux.Handle("/static/custom.css", extensionNoCache(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		_, _ = w.Write([]byte(cssBody))
	})))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"pages": pageList})
	})
	if wrap != nil {
		mux.Handle("/api/pages", wrap(handler))
	} else {
		mux.Handle("/api/pages", handler)
	}
}

func extensionNoCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		next.ServeHTTP(w, r)
	})
}
