package app

import (
	"github.com/ydtg1993/papa/v4/internal/reportrunner"
	"github.com/ydtg1993/papa/v4/obs/inputbudget"
	"testing"
)

func TestReportInputLimitsComposition(t *testing.T) {
	a := &App{}
	if err := a.UseReportInputLimits(inputbudget.Limits{MaxEvents: 17}); err != nil {
		t.Fatal(err)
	}
	if a.reportInputLimits.MaxEvents != 17 || a.reportInputLimits.MaxLineBytes <= 0 {
		t.Fatal("input limits not normalized")
	}
	a.reportRunner = reportrunner.NewWithLimits(reportrunner.Options{}, a.reportInputLimits)
	if err := a.UseReportInputLimits(inputbudget.Limits{MaxEvents: 18}); err == nil {
		t.Fatal("late budget change accepted")
	}
	if a.reportInputLimits.MaxEvents != 17 {
		t.Fatal("rejected update changed budget")
	}
}
