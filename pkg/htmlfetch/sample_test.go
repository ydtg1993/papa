package htmlfetch

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func sampleFixture() (Request, *Response) {
	u, _ := url.Parse("https://user:password@example.com/api?page=2&token=secret-query#secret-fragment")
	return Request{URL: u.String(), Method: "POST", Headers: map[string]string{"Authorization": "secret-auth", "Content-Type": "application/json"}, Body: []byte(`{"password":"secret-body"}`)}, &Response{URL: u, StatusCode: 200, Header: http.Header{"Set-Cookie": {"sid=secret-cookie"}, "Content-Type": {"application/json"}}, Body: []byte(`{"id":9007199254740993,"token":"secret-token"}`)}
}

func TestSampleDefaultOmissionAndAllowlist(t *testing.T) {
	req, res := sampleFixture()
	var out bytes.Buffer
	if err := WriteSample(&out, req, res, SamplePolicy{Headers: []string{"Content-Type", "Authorization", "Set-Cookie"}, QueryKeys: []string{"page"}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "secret") || strings.Contains(out.String(), "password") {
		t.Fatalf("credentials in sample: %s", out.String())
	}
	s, err := ReadSample(bytes.NewReader(out.Bytes()), int64(out.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if s.Response.URL != "https://example.com/api?page=2" || s.Request.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("sample: %+v", s)
	}
	if _, err := s.OfflineResponse(); err == nil {
		t.Fatal("omission must not appear as empty response")
	}
}

func TestSampleRedactedOfflineRoundTrip(t *testing.T) {
	req, res := sampleFixture()
	var out bytes.Buffer
	policy := SamplePolicy{RedactBody: func(request bool, b []byte) ([]byte, error) {
		var v map[string]any
		if err := DecodeJSON(b, &v); err != nil {
			return nil, err
		}
		delete(v, "password")
		delete(v, "token")
		return json.Marshal(v)
	}}
	if err := WriteSample(&out, req, res, policy); err != nil {
		t.Fatal(err)
	}
	s, err := ReadSample(&out, 0)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.OfflineResponse()
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := r.DecodeJSON(&v); err != nil {
		t.Fatal(err)
	}
	if v["id"] != json.Number("9007199254740993") || v["token"] != nil {
		t.Fatalf("offline data: %v", v)
	}
	r.Body[0] = 'x'
	if s.Response.Body[0] == 'x' {
		t.Fatal("offline response aliases sample")
	}
	if !strings.Contains(string(res.Body), "secret-token") {
		t.Fatal("original body mutated")
	}
}

func TestSampleFailuresWriteNothingAndBoundInput(t *testing.T) {
	req, res := sampleFixture()
	var out bytes.Buffer
	err := WriteSample(&out, req, res, SamplePolicy{RedactBody: func(bool, []byte) ([]byte, error) { return nil, errors.New("secret-error") }})
	if err == nil || out.Len() != 0 || strings.Contains(err.Error(), "secret-error") {
		t.Fatalf("failure: %v output=%s", err, out.String())
	}
	for _, raw := range []string{`{"version":2}`, `{"version":1} {}`, `garbage`} {
		if _, err := ReadSample(strings.NewReader(raw), 0); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	if _, err := ReadSample(strings.NewReader(`{"version":1}`), 3); err == nil {
		t.Fatal("limit ignored")
	}
}
