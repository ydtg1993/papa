package runtimecontrol

import (
	"strings"
	"testing"
)

func TestFrontendPreservesRawHTTPError(t *testing.T) {
	if !strings.Contains(Script, "rawHTTPError") {
		t.Fatal("raw HTTP errors must remain available to the controlled UI")
	}
}
