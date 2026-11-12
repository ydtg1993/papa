package shell

import (
	"html/template"
	"io/fs"
	"net/http"
	"sync"
)

// RenderOptions supplies only page presentation dependencies, never authorization.
type RenderOptions struct {
	AuthRequired   func() bool
	OptionalAssets func() (charts, motion bool)
	Logger         interface{ Errorf(string, ...any) }
}

// Renderer owns parsed page templates for one monitor lifecycle.
type Renderer struct {
	assets   fs.FS
	options  RenderOptions
	once     sync.Once
	template *template.Template
	err      error
}

func NewRenderer(assets fs.FS, options RenderOptions) *Renderer {
	return &Renderer{assets: assets, options: options}
}

type templateData struct {
	AuthRequired bool
	OaoCharts    bool
	OaoMotion    bool
}

func (s *Renderer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.once.Do(func() { s.template, s.err = template.ParseFS(s.assets, "template.html") })
	if s.err != nil {
		s.options.Logger.Errorf("load monitor template: %s", s.err.Error())
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	charts, motion := s.options.OptionalAssets()
	if err := s.template.Execute(w, templateData{AuthRequired: s.options.AuthRequired(), OaoCharts: charts, OaoMotion: motion}); err != nil {
		s.options.Logger.Errorf("execute monitor template: %s", err.Error())
	}
}
