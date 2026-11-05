package htmlfetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
)

// Representation is scoped to one exact URL AND account/headers/locale chosen by
// the caller. It is not a shared cache. Save it only after successful data landing.
type Representation struct {
	URL          string
	ETag         string
	LastModified string
	SHA256       string
}

func ContentHash(body []byte) string { hash := sha256.Sum256(body); return hex.EncodeToString(hash[:]) }

type IncrementalResult struct {
	Response       *Response
	Representation Representation
	NotModified    bool // true for 304; Response.Body is absent
	Changed        bool // content hash differs or there was no previous representation
}

// FetchIncremental issues a GET without following redirects so validators never
// migrate to a different resource. 304 is accepted only after a matching validator
// was sent; unexpected 304/redirects/non-200 responses are errors. Existing Do and
// Fetch keep their original non-2xx behavior. No automatic storage/retry occurs.
func (c *Client) FetchIncremental(ctx context.Context, rawURL string, previous *Representation) (*IncrementalResult, error) {
	req := Request{URL: rawURL, NoRedirect: true, Headers: map[string]string{"If-None-Match": "", "If-Modified-Since": "", "Range": ""}}
	matching := previous != nil && previous.URL == rawURL
	conditional := false
	if matching {
		if previous.ETag != "" {
			req.Headers["If-None-Match"] = previous.ETag
			conditional = true
		} else if previous.LastModified != "" {
			req.Headers["If-Modified-Since"] = previous.LastModified
			conditional = true
		}
	}
	response, err := c.Do(ctx, req)
	if response != nil && response.StatusCode == http.StatusNotModified {
		// Do returns only a status error on a well-formed empty 304 response.
		status, ok := err.(*StatusError)
		if !ok || status.Code != 304 || !conditional {
			return nil, fmt.Errorf("unexpected 304 without valid conditional exchange")
		}
		state := *previous
		if tag := response.Header.Get("ETag"); tag != "" {
			state.ETag = tag
		}
		if modified := response.Header.Get("Last-Modified"); modified != "" {
			state.LastModified = modified
		}
		return &IncrementalResult{Response: response, Representation: state, NotModified: true}, nil
	}
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("incremental GET requires 200 or conditional 304, got %d", response.StatusCode)
	}
	state := Representation{URL: rawURL, ETag: response.Header.Get("ETag"), LastModified: response.Header.Get("Last-Modified"), SHA256: ContentHash(response.Body)}
	return &IncrementalResult{Response: response, Representation: state, Changed: !matching || state.SHA256 != previous.SHA256}, nil
}
