package server

import (
	"regexp"
	"strings"
	"testing"
)

func appearanceStylesheet(t *testing.T, name string) string {
	t.Helper()
	b, err := staticFS.ReadFile("static/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(string(b), "")
}

// Inspect declaration blocks, including those inside media queries. Conditions
// must live on html so body-mounted oao menus and dialogs inherit the same tokens.
func TestMonitorAppearanceTokensAreRootScoped(t *testing.T) {
	css := appearanceStylesheet(t, "layout.css")
	rules := regexp.MustCompile(`([^{}]+)\{([^{}]*)\}`).FindAllStringSubmatch(css, -1)
	tokens := regexp.MustCompile(`--(?:background|foreground|border-width|shadow-color|font-display)\s*:`)
	conditions := map[string]bool{}
	for _, rule := range rules {
		if !tokens.MatchString(rule[2]) {
			continue
		}
		selector := strings.TrimSpace(rule[1])
		if selector != `:root:where([data-appearance="simple"])` && selector != `:root:where([data-appearance="simple"][data-theme="dark"])` {
			t.Fatalf("simple tokens would leak into classic or miss body-mounted controls: %s", selector)
		}
		conditions[selector] = true
	}
	if len(conditions) != 2 {
		t.Fatal("simple appearance needs independent light/default and dark root tokens")
	}
	classic := appearanceStylesheet(t, "mo.css")
	if !regexp.MustCompile(`--border-width:\s*2px`).MatchString(classic) || !strings.Contains(classic, `"Archivo Black"`) {
		t.Fatal("classic foundation lost its border width or display typography")
	}
}

func TestMonitorAppearanceKeepsSharedNavigationAndPortalStyles(t *testing.T) {
	css := appearanceStylesheet(t, "layout.css")
	for _, selector := range []string{
		`.sidebar #main-nav.nav-open`, `#section-nav:empty`, `button:focus-visible`,
	} {
		if !regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(selector) + `(?:\s*\{|\s*,)`).MatchString(css) {
			t.Errorf("shared navigation/focus rule is missing or tied to one appearance: %s", selector)
		}
	}
	if !regexp.MustCompile(`(?s)\.sidebar #main-nav\.nav-open\s*\{[^}]*grid-template-columns:repeat\(2,minmax\(0,1fr\)\)`).MatchString(css) {
		t.Fatal("mobile categories must retain two shrinkable columns in both appearances")
	}
	if !regexp.MustCompile(`(?s):where\(html\[data-appearance="classic"\]\) \.nav-category\s*\{[^}]*border:2px[^}]*box-shadow:`).MatchString(css) {
		t.Fatal("four-category navigation needs a classic border and shadow treatment")
	}
	for _, selector := range []string{
		`.oao-dlg`, `.oao-dlg h3`, `.oao-dlg.danger h3`, `.oao-select-content`,
		`.oao-field > input`, `.oao-field > textarea`, `.oao-toast`,
	} {
		if !strings.Contains(css, `:where(html[data-appearance="simple"]) `+selector) {
			t.Errorf("body-mounted component has no simple appearance override: %s", selector)
		}
	}
	if strings.Contains(css, ".content-top h1:focus { outline:none;") {
		t.Fatal("module navigation must not hide the focused heading")
	}
}

func TestMonitorAppearanceReducedMotionIncludesNewControls(t *testing.T) {
	css := appearanceStylesheet(t, "visuals.css")
	feedback := `:is(.nav-category,.oao-btn):is(:hover,:active) { transform:none !important; }`
	for _, prefix := range []string{
		`html[data-oao-motion="reduced"] `,
		`@media(prefers-reduced-motion:reduce) { `,
	} {
		if !strings.Contains(css, prefix+feedback) {
			t.Errorf("reduced motion misses category or body-mounted button feedback: %s", prefix)
		}
	}
	if strings.Contains(css, "* { transform:none") {
		t.Fatal("motion preferences must retain structural transforms, including switch state")
	}
}
