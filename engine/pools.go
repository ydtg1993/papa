package engine

import "github.com/ydtg1993/papa/v4/engine/internal/scheduler"

type poolRegistry = scheduler.Registry[*Task]

func newPoolRegistry() *poolRegistry { return scheduler.NewRegistry[*Task]() }
