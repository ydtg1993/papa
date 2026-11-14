package taskflow

import "testing"

func TestValidateGraph(t *testing.T) {
	for _, tc := range []struct {
		name  string
		graph map[string][]string
		valid bool
	}{
		{"empty", map[string][]string{}, true},
		{"pipeline", map[string][]string{"catalog": {"detail"}, "detail": {"episode"}, "episode": nil}, true},
		{"diamond", map[string][]string{"root": {"left", "right"}, "left": {"end"}, "right": {"end"}, "end": nil}, true},
		{"self", map[string][]string{"catalog": {"catalog"}}, false},
		{"cycle", map[string][]string{"catalog": {"detail"}, "detail": {"catalog"}}, false},
		{"unknown", map[string][]string{"catalog": {"detail"}}, false},
		{"duplicate", map[string][]string{"catalog": {"detail", "detail"}, "detail": nil}, false},
		{"whitespace", map[string][]string{"catalog": {" detail"}, "detail": nil}, false},
		{"cross_site", map[string][]string{"catalog": {"b:detail"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := Validate("a", tc.graph); (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}
