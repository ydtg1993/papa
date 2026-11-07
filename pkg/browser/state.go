package browser

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

const maxStateBytes = 8 << 20
const stateMagic = "PAPAST01"

// PageState contains credentials. Never log or save it as plaintext.
// Cookies are those applicable to the exported page URL, not the whole account.
type PageState struct {
	Version      int                    `json:"version"`
	Origin       string                 `json:"origin"`
	Cookies      []*proto.NetworkCookie `json:"cookies"`
	LocalStorage map[string]string      `json:"local_storage"`
}

func pageOrigin(page *rod.Page) (string, error) {
	if page == nil || page.Browser().BrowserContextID == "" {
		return "", fmt.Errorf("page state requires an isolated browser context")
	}
	info, err := page.Info()
	if err != nil {
		return "", err
	}
	u, err := url.Parse(info.URL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("page state requires an HTTP origin")
	}
	return u.Scheme + "://" + u.Host, nil
}

// ExportPageState snapshots the current page. Pause concurrent navigation/writes
// while exporting; Cookie and LocalStorage reads are not an atomic transaction.
func ExportPageState(page *rod.Page) (*PageState, error) {
	origin, err := pageOrigin(page)
	if err != nil {
		return nil, err
	}
	cookies, err := page.Cookies(nil)
	if err != nil {
		return nil, err
	}
	if _, err := httpCookies(cookies); err != nil {
		return nil, err
	}
	result, err := page.Eval(`() => ({origin:location.origin, values:Object.fromEntries(Object.entries(localStorage))})`)
	if err != nil {
		return nil, err
	}
	var snapshot struct {
		Origin string            `json:"origin"`
		Values map[string]string `json:"values"`
	}
	if err := result.Value.Unmarshal(&snapshot); err != nil {
		return nil, err
	}
	if snapshot.Origin != origin {
		return nil, fmt.Errorf("page navigated during state export")
	}
	return &PageState{Version: 1, Origin: origin, Cookies: cookies, LocalStorage: snapshot.Values}, nil
}

// ValidatePageState checks snapshot format and cookie scope without modifying a
// browser. Account stores can reject unrestorable snapshots before publication.
func ValidatePageState(s *PageState) error { return validatePageState(s) }

func validatePageState(s *PageState) error {
	if s == nil || s.Version != 1 {
		return fmt.Errorf("unsupported page state version")
	}
	u, err := url.Parse(s.Origin)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || s.Origin != u.Scheme+"://"+u.Host {
		return fmt.Errorf("invalid state origin")
	}
	for _, c := range s.Cookies {
		if c == nil {
			return fmt.Errorf("invalid state cookie")
		}
		if !strings.HasPrefix(c.Path, "/") {
			return fmt.Errorf("invalid state cookie path")
		}
		domain := strings.TrimPrefix(c.Domain, ".")
		if domain == "" || (u.Hostname() != domain && !(strings.HasPrefix(c.Domain, ".") && strings.HasSuffix(u.Hostname(), "."+domain))) {
			return fmt.Errorf("state cookie outside origin")
		}
	}
	_, err = httpCookies(s.Cookies)
	return err
}

// RestorePageState merges saved cookies and replaces LocalStorage on a same-origin
// isolated page. Reload afterwards so site scripts see restored values. On error,
// discard the context: browser writes are not transactional. Caller must prevent
// concurrent navigation, and use a deadline on page.Context/Timeout.
func RestorePageState(page *rod.Page, state *PageState) error {
	if err := validatePageState(state); err != nil {
		return err
	}
	origin, err := pageOrigin(page)
	if err != nil {
		return err
	}
	if origin != state.Origin {
		return fmt.Errorf("page state origin mismatch")
	}
	cookies := make([]*proto.NetworkCookieParam, 0, len(state.Cookies))
	for _, c := range state.Cookies {
		p := &proto.NetworkCookieParam{Name: c.Name, Value: c.Value, Path: c.Path, Secure: c.Secure, HTTPOnly: c.HTTPOnly, SameSite: c.SameSite}
		if strings.HasPrefix(c.Domain, ".") {
			p.Domain = c.Domain
		} else {
			p.URL = state.Origin + c.Path
		}
		if !c.Session {
			p.Expires = c.Expires
		}
		cookies = append(cookies, p)
	}
	// Guard the execution context's origin as well as Page.Info's observed URL.
	_, err = page.Eval(`(origin, values) => { if(location.origin!==origin) throw new Error('state origin mismatch'); localStorage.clear(); for(const [k,v] of Object.entries(values||{})) localStorage.setItem(k,v); }`, state.Origin, state.LocalStorage)
	if err != nil {
		return err
	}
	if len(cookies) > 0 {
		return page.SetCookies(cookies)
	}
	return nil
}

func stateCipher(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("page state key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// WritePageState encrypts with a caller-owned 32-byte key. No key is embedded.
// Caller owns file permissions and atomic publication if Writer fails partway.
func WritePageState(w io.Writer, state *PageState, key []byte) error {
	if err := validatePageState(state); err != nil {
		return err
	}
	aead, err := stateCipher(key)
	if err != nil {
		return err
	}
	plain, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode page state failed")
	}
	if len(plain) > maxStateBytes {
		return fmt.Errorf("page state exceeds 8 MiB")
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	data := append([]byte(stateMagic), nonce...)
	data = aead.Seal(data, nonce, plain, []byte(stateMagic))
	n, err := w.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}

// ReadPageState authenticates before decoding and caps encrypted input size.
func ReadPageState(r io.Reader, key []byte) (*PageState, error) {
	aead, err := stateCipher(key)
	if err != nil {
		return nil, err
	}
	limit := maxStateBytes + len(stateMagic) + aead.NonceSize() + aead.Overhead()
	data, err := io.ReadAll(io.LimitReader(r, int64(limit+1)))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("encrypted page state too large")
	}
	start := len(stateMagic)
	end := start + aead.NonceSize()
	if len(data) < end+aead.Overhead() || string(data[:start]) != stateMagic {
		return nil, fmt.Errorf("invalid encrypted page state")
	}
	plain, err := aead.Open(nil, data[start:end], data[end:], []byte(stateMagic))
	if err != nil {
		return nil, fmt.Errorf("page state authentication failed")
	}
	var state PageState
	if err := htmlfetch.DecodeJSON(plain, &state); err != nil {
		return nil, fmt.Errorf("invalid page state JSON")
	}
	if err := validatePageState(&state); err != nil {
		return nil, err
	}
	return &state, nil
}
