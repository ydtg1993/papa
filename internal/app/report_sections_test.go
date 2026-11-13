package app

import (
	"github.com/ydtg1993/papa/v4/internal/reportrunner"
	"reflect"
	"testing"
)

func TestOnlineReportHasNoExecutableOrBusinessResourceInputs(t *testing.T) {
	opts := reflect.TypeOf(reportrunner.Options{})
	for _, name := range []string{"DB", "EngineState", "Sections", "Patterns", "Logger"} {
		if _, ok := opts.FieldByName(name); ok {
			t.Fatal("online resource coupling", name)
		}
	}
	a := reflect.TypeOf(&App{})
	for _, name := range []string{"UseReportSection", "UseReportPattern"} {
		if _, ok := a.MethodByName(name); ok {
			t.Fatal("online callback API remains", name)
		}
	}
}
