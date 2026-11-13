package reportrunner

import (
	"fmt"
	"github.com/ydtg1993/papa/v4/obs/report"
	"io"
	"os"
	"path/filepath"
	"time"
)

func (r *Runner) writer() {
	defer close(r.done)
	defer r.pruning.close()
	disabled := false
	for rep := range r.jobs {
		if disabled {
			continue
		}
		panicked := false
		func() {
			defer func() {
				if recover() != nil {
					r.failures.Add(1)
					panicked = true
				}
			}()
			dir := filepath.Join(r.options.Config.ReportDir, report.DayName(rep.GeneratedAt))
			if _, err := r.write(dir, rep); err != nil {
				r.failures.Add(1)
				return
			}
			if err := r.prune(rep.GeneratedAt); err != nil {
				r.failures.Add(1)
			}
		}()
		disabled = panicked
		if panicked {
			r.pruning.close()
		}
	}
}

// Pruning inspects at most 128 entries and removes only the four known artifacts
// from expired date directories. The writer retains the cursor between passes,
// closes it at EOF/error/exit, and never removes unrelated files/directories.
func (r *Runner) prune(now time.Time) (result error) {
	defer func() {
		if result != nil {
			r.pruning.close()
		}
	}()
	retention := r.options.Config.Retention
	if retention < 0 {
		return nil
	}
	if retention == 0 {
		retention = 7 * 24 * time.Hour
	}
	if r.pruning.dir == nil {
		f, err := os.Open(r.options.Config.ReportDir)
		if err != nil {
			return err
		}
		r.pruning.dir = f
	}
	entries, err := r.pruning.dir.ReadDir(128)
	if err != nil {
		r.pruning.close()
		if err != io.EOF {
			return err
		}
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		day, err := time.ParseInLocation("2006-01-02", e.Name(), now.Location())
		if err != nil || !day.Before(now.Add(-retention)) || e.Name() == report.DayName(now) {
			continue
		}
		for _, name := range []string{"report.md", "report.json", "ai-brief.md", "ai-brief.json"} {
			p := filepath.Join(r.options.Config.ReportDir, e.Name(), name)
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("prune report artifact: %w", err)
			}
		}
		_ = os.Remove(filepath.Join(r.options.Config.ReportDir, e.Name()))
	}
	return nil
}

// retentionCursor is exclusively owned by the writer, not intake/collector.
type retentionCursor struct{ dir *os.File }

func (c *retentionCursor) close() {
	if c.dir != nil {
		_ = c.dir.Close()
		c.dir = nil
	}
}
