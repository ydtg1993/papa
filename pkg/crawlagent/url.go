package crawlagent

import (
	"net/url"

	"github.com/ydtg1993/papa/v3/pkg/rulestore"
	"github.com/ydtg1993/papa/v3/pkg/webaccess"
)

func urlIdentity(raw string) (string, error) {
	canonical, err := webaccess.CanonicalURL(raw)
	if err != nil {
		return "", ErrEvidence
	}
	u, err := url.Parse(canonical)
	if err != nil {
		return "", ErrEvidence
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", ErrEvidence
	}
	u.RawQuery = query.Encode()
	u.ForceQuery = false
	return u.String(), nil
}

func (e *execution) isHiddenURL(raw string) (bool, error) {
	key, err := urlIdentity(raw)
	if err != nil {
		return false, err
	}
	if e.hiddenURLs[key] {
		return true, nil
	}
	policy := e.agent.config.SamplePolicy
	policy.RedactBody = func(_ bool, b []byte) ([]byte, error) { return b, nil }
	clean, err := rulestore.SanitizeSample(rulestore.SampleInput{URL: raw, StatusCode: 200}, policy)
	if err != nil {
		return false, ErrEvidence
	}
	key, err = urlIdentity(clean.URL)
	if err != nil {
		return false, err
	}
	return e.hiddenURLs[key], nil
}

func (e *execution) hideURL(raw string) error {
	key, err := urlIdentity(raw)
	if err != nil {
		return err
	}
	e.hiddenURLs[key] = true
	return nil
}
