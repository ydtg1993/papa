package app

import (
	"github.com/ydtg1993/papa/v4/internal/reportrunner"
	"github.com/ydtg1993/papa/v4/pkg/events"
	"sync/atomic"
)

// observationPipeline forwards to two independent bounded consumers. It is
// itself isolated before injection into Engine; neither consumer owns App/DB.
type observationPipeline struct {
	archive events.Closer
	report  atomic.Pointer[reportrunner.Runner]
}

func (p *observationPipeline) Emit(ev events.Event) {
	if r := p.report.Load(); r != nil {
		r.Emit(ev)
	}
	p.archive.Emit(ev)
}
