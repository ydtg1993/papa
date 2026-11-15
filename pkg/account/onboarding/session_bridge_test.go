package onboarding

import (
	"testing"

	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/browser"
)

func TestBrowserCandidateStagingDoesNotRequireProviderCookieCopy(t *testing.T) {
	candidate := &account.Credentials{BrowserState: &browser.PageState{Version: 1, Origin: "https://fixture.example", LocalStorage: map[string]string{"token": "synthetic"}}}
	state, err := credentialState(candidate)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := restoreCredentials(state)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Session != nil || restored.Session == nil || restored.BrowserState.LocalStorage["token"] != "synthetic" {
		t.Fatal("browser candidate staging changed its protocol or lost state")
	}
}
