package middleware

import (
	"path/filepath"
	"testing"
)

func TestSanitizeOutputPath(t *testing.T) {
	sub := filepath.Join("downloads", "video")
	cases := []struct {
		dir, file string
		wantDir   string
		wantFile  string
		ok        bool
	}{
		{filepath.FromSlash("downloads/video"), "video.ts", sub, "video.ts", true},
		{"", "video.ts", "", "video.ts", true},
		{".", "video.ts", "", "video.ts", true},
		// 文件名里的 ../ 被 Base 中和为安全名
		{"downloads/video", "../escape.ts", sub, "escape.ts", true},
		{"downloads/video", "a/b/c.ts", sub, "c.ts", true},
		// 文件名恰为 . / .. / 空 时拒绝
		{"downloads/video", "..", "", "", false},
		// 目录逃逸拒绝
		{"../../etc", "x.ts", "", "", false},
	}
	for _, c := range cases {
		d, f, ok := SanitizeOutputPath(c.dir, c.file)
		if ok != c.ok || d != c.wantDir || f != c.wantFile {
			t.Fatalf("SanitizeOutputPath(%q, %q) = (%q, %q, %v), want (%q, %q, %v)",
				c.dir, c.file, d, f, ok, c.wantDir, c.wantFile, c.ok)
		}
	}
}
