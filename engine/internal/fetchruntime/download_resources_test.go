package fetchruntime

import (
	"github.com/ydtg1993/papa/v4/pkg/middleware"
	"testing"
)

func TestDownloadResourceConfigurationFreezesSharedBudget(t *testing.T) {
	var d Downloads
	budget := middleware.NewDownloadBudget(1024, 4096)
	if err := d.ConfigureResources(middleware.DownloadLimits{Chunks: 7}, budget); err != nil {
		t.Fatal(err)
	}
	first, got := d.Resources()
	second, again := d.Resources()
	if got != budget || again != budget || first != second || first.Chunks != 7 || first.ResumeBytes <= 0 {
		t.Fatal("resource snapshot or budget changed")
	}
	if err := d.ConfigureResources(middleware.DownloadLimits{Chunks: 8}, nil); err == nil {
		t.Fatal("late configuration accepted")
	}
	release, err := got.AcquireMemory(1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := again.AcquireMemory(1); err == nil {
		t.Fatal("lookups received independent budgets")
	}
	release()
	release2, err := again.AcquireMemory(1024)
	if err != nil {
		t.Fatal(err)
	}
	release2()
}
