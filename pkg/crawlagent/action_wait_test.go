package crawlagent

import "testing"

func TestRenderWaitActionContract(t *testing.T) {
	for _, raw := range []string{
		`{"tool":"fetch_page","url":"seed:0","mode":"render","wait":{"selector":"#ready","state":"exists","timeout_ms":1}}`,
		`{"tool":"fetch_page","url":"seed:0","mode":"render","wait":{"selector":"article > h2","state":"visible","timeout_ms":30000}}`,
		`{"tool":"fetch_page","url":"seed:0","mode":"http"}`,
	} {
		if _, err := parseAction(raw); err != nil {
			t.Errorf("valid action rejected: %s: %v", raw, err)
		}
	}
	for _, raw := range []string{
		`{"tool":"fetch_page","mode":"http","wait":{"selector":"#x","state":"exists","timeout_ms":1}}`,
		`{"tool":"fetch_page","wait":{"selector":"#x","state":"exists","timeout_ms":1}}`,
		`{"tool":"record_api","wait":{"selector":"#x","state":"exists","timeout_ms":1}}`,
		`{"tool":"fetch_page","mode":"render","wait":null}`,
		`{"tool":"fetch_page","mode":"render","wait":{}}`,
		`{"tool":"fetch_page","mode":"render","wait":{"selector":null,"state":"exists","timeout_ms":1}}`,
		`{"tool":"fetch_page","mode":"render","wait":{"selector":"#x","state":null,"timeout_ms":1}}`,
		`{"tool":"fetch_page","mode":"render","wait":{"selector":"#x","state":"exists","timeout_ms":null}}`,
		`{"tool":"fetch_page","mode":"render","wait":{"selector":"#x","state":"exists","timeout_ms":30001}}`,
		`{"tool":"fetch_page","mode":"render","wait":{"selector":"#x","state":"exists","timeout_ms":1.2}}`,
		`{"tool":"fetch_page","mode":"render","wait":{"selector":"#x","state":"visible","timeout_ms":-1}}`,
		`{"tool":"fetch_page","mode":"render","wait":{"selector":"div:has(:has(a))","state":"exists","timeout_ms":10}}`,
		`{"tool":"fetch_page","mode":"render","wait":{"selector":"[","state":"exists","timeout_ms":10}}`,
		`{"tool":"fetch_page","mode":"render","wait":{"selector":"#x","state":"exists","timeout_ms":10,"script":"alert(1)"}}`,
	} {
		if _, err := parseAction(raw); err == nil {
			t.Errorf("invalid wait accepted: %s", raw)
		}
	}
}
