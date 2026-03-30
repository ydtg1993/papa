package crawler

type Task struct {
	URL   string
	Retry int
	Stage string //阶段标识，如 "catalog", "detail"
}

func (t *Task) GetUrl() string {
	return t.URL
}

func (t *Task) GetRetry() int {
	return t.Retry
}

func (t *Task) IncRetry() {
	t.Retry++
}
