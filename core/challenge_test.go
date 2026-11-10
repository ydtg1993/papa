package core

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestSiteChallengeContractAndRedaction(t *testing.T) {
	if Retryable(ErrSiteChallenge) || ErrorKind(ErrSiteChallenge) != "site_challenge" {
		t.Fatal("sentinel lost challenge classification")
	}
	err := fmt.Errorf("request: %w", &SiteChallengeError{StatusCode: 403, Evidence: "cf-mitigated: challenge"})
	if !errors.Is(err, ErrSiteChallenge) || Retryable(err) || ErrorKind(err) != "site_challenge" {
		t.Fatalf("wrong challenge contract: %v", err)
	}
	unsafe := (&SiteChallengeError{StatusCode: 200, Evidence: "https://user:secret@example.com/?token=secret"}).Error()
	if strings.Contains(unsafe, "secret") || strings.Contains(unsafe, "example.com") {
		t.Fatal("arbitrary evidence leaked", unsafe)
	}
}
