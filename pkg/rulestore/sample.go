package rulestore

import (
	"bytes"
	"errors"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

const MaxSampleURLBytes = 16 << 10
const MaxEvidenceIDBytes = 256

var (
	ErrSampleInvalid     = errors.New("rulestore: invalid sample metadata")
	ErrRedactionRequired = errors.New("rulestore: explicit body redaction policy required")
	ErrSampleRedaction   = errors.New("rulestore: sample redaction failed")
)

// SampleInput deliberately has no headers, cookies or request body. Rendered
// DOM must use status zero; HTTP input must preserve its actual status code.
type SampleInput struct {
	URL        string
	StatusCode int
	Rendered   bool
	Body       []byte
	EvidenceID string
}

// SanitizedSample keeps metadata serializable without copying the body into
// database JSON. Body is the detached, explicitly approved redaction result.
// URL paths, allowlisted query values and EvidenceID must be nonsecret: use
// RedactURL to remove any path identifiers and use an opaque evidence ID.
type SanitizedSample struct {
	URL        string `json:"url"`
	StatusCode int    `json:"status_code"`
	Rendered   bool   `json:"rendered"`
	EvidenceID string `json:"evidence_id,omitempty"`
	BodySHA256 string `json:"body_sha256"`
	BodyBytes  int    `json:"body_bytes"`
	Body       []byte `json:"-"`
}

// SanitizeSample requires explicit response-body approval; a missing callback
// never silently becomes an acceptable empty sample. Callbacks receive private
// data and their failures (including panics) return a fixed, nonsecret error.
// Headers in the shared htmlfetch policy are ignored: none are accepted here.
func SanitizeSample(input SampleInput, policy htmlfetch.SamplePolicy) (sample SanitizedSample, err error) {
	if len(input.Body) > MaxSampleBodyBytes || len(input.URL) > MaxSampleURLBytes || len(input.EvidenceID) > MaxEvidenceIDBytes {
		return sample, ErrSampleBudget
	}
	if !validSampleStatus(input.StatusCode, input.Rendered) || !utf8.ValidString(input.URL) || !utf8.ValidString(input.EvidenceID) || strings.IndexFunc(input.EvidenceID, unicode.IsControl) >= 0 {
		return sample, ErrSampleInvalid
	}
	if policy.RedactBody == nil {
		return sample, ErrRedactionRequired
	}
	defer func() {
		if recover() != nil {
			sample, err = SanitizedSample{}, ErrSampleRedaction
		}
	}()
	cleanURL, err := sanitizeSampleURL(input.URL, policy.QueryKeys)
	if err != nil {
		return SanitizedSample{}, err
	}
	if policy.RedactURL != nil {
		cleanURL, err = policy.RedactURL(cleanURL)
		if err != nil {
			return SanitizedSample{}, ErrSampleRedaction
		}
		if len(cleanURL) > MaxSampleURLBytes {
			return SanitizedSample{}, ErrSampleBudget
		}
		// Reapply filtering so a callback cannot accidentally restore credentials.
		cleanURL, err = sanitizeSampleURL(cleanURL, policy.QueryKeys)
		if err != nil {
			return SanitizedSample{}, ErrSampleRedaction
		}
	}
	if len(cleanURL) > MaxSampleURLBytes {
		return SanitizedSample{}, ErrSampleBudget
	}
	body, err := policy.RedactBody(false, bytes.Clone(input.Body))
	if err != nil {
		return SanitizedSample{}, ErrSampleRedaction
	}
	if len(body) > MaxSampleBodyBytes {
		return SanitizedSample{}, ErrSampleBudget
	}
	body = bytes.Clone(body)
	return SanitizedSample{URL: cleanURL, StatusCode: input.StatusCode, Rendered: input.Rendered, EvidenceID: input.EvidenceID, BodySHA256: bodySHA256(body), BodyBytes: len(body), Body: body}, nil
}

func validSampleStatus(status int, rendered bool) bool {
	if rendered {
		return status == 0
	}
	return status >= 100 && status <= 599
}

func sanitizeSampleURL(raw string, keys []string) (string, error) {
	if !utf8.ValidString(raw) {
		return "", ErrSampleInvalid
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" {
		return "", ErrSampleInvalid
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", ErrSampleInvalid
	}
	filtered := make(url.Values)
	for _, key := range keys {
		if values, ok := query[key]; ok {
			filtered[key] = values
		}
	}
	u.User, u.RawQuery, u.Fragment, u.RawFragment, u.ForceQuery = nil, filtered.Encode(), "", "", false
	return u.String(), nil
}
