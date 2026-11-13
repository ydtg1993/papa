package accounts

import (
	"strings"
	"testing"
)

func TestFrontendKeepsConfirmationAndControlledInput(t *testing.T) {
	for _, required := range []string{"confirm_identity", "身份正确，保存并启用", "sessionStorage", "candidate_publish_failed"} {
		if !strings.Contains(Script, required) {
			t.Fatalf("missing behavior %s", required)
		}
	}
	for _, unsafe := range []string{"innerHTML", "localStorage", "document.cookie", "webSocketDebuggerUrl", "eval("} {
		if strings.Contains(Script, unsafe) {
			t.Fatalf("unsafe UI path %s", unsafe)
		}
	}
}
