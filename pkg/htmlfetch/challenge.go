package htmlfetch

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/ydtg1993/papa/v4/core"
)

// A challenge response must stop before net/http applies Set-Cookie or follows
// Location. Returning an error without a response is the RoundTripper contract;
// this private carrier keeps ownership of the body/metadata until Client.do can
// close the body and return the transport-neutral error without a URL wrapper.
type siteChallengeResponse struct {
	err      error
	response *http.Response
	url      *url.URL
}

func (e *siteChallengeResponse) Error() string { return e.err.Error() }
func (e *siteChallengeResponse) Unwrap() error { return e.err }

type challengeTransport struct{ base http.RoundTripper }

func (t challengeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(req)
	if err != nil || response == nil {
		return response, err
	}
	if challenge := DetectSiteChallenge(response.StatusCode, response.Header); challenge != nil {
		u := *req.URL
		return nil, &siteChallengeResponse{err: challenge, response: response, url: &u}
	}
	return response, nil
}

// DetectSiteChallenge recognizes explicit protocol evidence regardless of HTTP
// status. Ordinary status codes, HTML and heuristic text are not evidence. The
// marker describes this response, not the vendor protecting an entire site.
func DetectSiteChallenge(status int, header http.Header) error {
	// Iteration also handles non-canonical headers from CDP or custom transports.
	for name, values := range header {
		if !strings.EqualFold(name, "cf-mitigated") {
			continue
		}
		for _, value := range values {
			if strings.EqualFold(strings.TrimSpace(value), "challenge") {
				return &core.SiteChallengeError{StatusCode: status, Evidence: "cf-mitigated: challenge"}
			}
		}
	}
	return nil
}
