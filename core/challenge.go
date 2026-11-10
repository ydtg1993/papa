package core

import (
	"fmt"
)

// ErrSiteChallenge identifies explicit target-site challenge responses. It is
// independent of expired credentials and account-level human verification.
var ErrSiteChallenge error = siteChallengeSentinel{}

type siteChallengeSentinel struct{}

func (siteChallengeSentinel) Error() string   { return "site challenge" }
func (siteChallengeSentinel) Retryable() bool { return false }
func (siteChallengeSentinel) Kind() string    { return "site_challenge" }

// SiteChallengeError contains bounded protocol evidence, never a response body,
// URL, cookie or token. Evidence is a fixed marker supplied by the detector.
// It stops immediate retries; a configured error queue may retry the task later.
type SiteChallengeError struct {
	StatusCode int
	Evidence   string
}

func (e *SiteChallengeError) Error() string {
	evidence := "explicit response marker"
	if e.Evidence == "cf-mitigated: challenge" {
		evidence = e.Evidence
	}
	return fmt.Sprintf("site challenge response (status=%d, evidence=%s)", e.StatusCode, evidence)
}

func (e *SiteChallengeError) Unwrap() error   { return ErrSiteChallenge }
func (e *SiteChallengeError) Retryable() bool { return false }
func (e *SiteChallengeError) Kind() string    { return "site_challenge" }
