package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/v2/config"
)

// 生效白名单为空时**必须喊出来**：它和"配了却没生效"从行为上分不出来，
// 而后者正是"后台怎么谁都能打开"这类排查里最难想到的方向。
// 两道门（来源 IP + 访问令牌）都空就是完全开放，各自都有一条启动警告。
func TestWarnIfOpenToAll(t *testing.T) {
	newLog := func() (*logrus.Logger, *bytes.Buffer) {
		var buf bytes.Buffer
		log := logrus.New()
		log.SetOutput(&buf)
		log.SetLevel(logrus.ErrorLevel)
		return log, &buf
	}

	t.Run("空白名单要报警", func(t *testing.T) {
		log, buf := newLog()
		warnIfOpenToAll(log, nil)
		out := buf.String()
		for _, want := range []string{"白名单为空", "任何来源", "server.whitelist"} {
			if !strings.Contains(out, want) {
				t.Fatalf("警告里缺 %q，实得：%s", want, out)
			}
		}
	})

	t.Run("有白名单时不报", func(t *testing.T) {
		log, buf := newLog()
		warnIfOpenToAll(log, []string{"10.0.0.0/8"})
		if buf.Len() != 0 {
			t.Fatalf("配了白名单就不该报警，实得：%s", buf.String())
		}
	})
}

// resolveWhitelist 的两个来源：文件优先（**存在即采用，即使为空** —— 空文件意味着
// 来源不限制，这是脚手架生成的初始状态），读不到才回退内联。
//
// 「文件只有注释 → 空、不回退内联」这条是**刻意的**：留空是"我不限制"的表达方式，
// 而它现在已经不静默了（启动会打上面那条警告）。所以这里只是把语义钉住。
func TestResolveWhitelistSources(t *testing.T) {
	dir := t.TempDir()
	emptyFile := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(emptyFile, []byte("# 只有注释\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fullFile := filepath.Join(dir, "full.txt")
	if err := os.WriteFile(fullFile, []byte("# 注释\n10.0.0.0/8\n 192.168.1.1 \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	a := &App{}
	for _, c := range []struct {
		name string
		cfg  config.ServerConfig
		want []string
	}{
		{
			"文件有内容 → 用文件，忽略内联",
			config.ServerConfig{WhitelistFile: fullFile, Whitelist: []string{"203.0.113.1"}},
			[]string{"10.0.0.0/8", "192.168.1.1"}, // 空行与注释被跳过、两侧空白被去掉
		},
		{
			"文件只有注释 → 空（即不限制），不回退内联",
			config.ServerConfig{WhitelistFile: emptyFile, Whitelist: []string{"203.0.113.1"}},
			[]string{},
		},
		{
			"文件读不到 → 回退内联",
			config.ServerConfig{WhitelistFile: filepath.Join(dir, "nope.txt"), Whitelist: []string{"203.0.113.1"}},
			[]string{"203.0.113.1"},
		},
		{
			"没配文件 → 用内联",
			config.ServerConfig{Whitelist: []string{"203.0.113.1"}},
			[]string{"203.0.113.1"},
		},
		{
			"两个都没配 → 空（= 不限制，启动会报警）",
			config.ServerConfig{},
			nil,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := a.resolveWhitelist(c.cfg)
			if len(got) != len(c.want) {
				t.Fatalf("resolveWhitelist = %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("resolveWhitelist = %v, want %v", got, c.want)
				}
			}
		})
	}
}
