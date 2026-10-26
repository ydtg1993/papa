package app

import (
	"bytes"
	"database/sql/driver"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/v3/admin/configstore"
	"github.com/ydtg1993/papa/v3/internal/gormfake"
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
		// 警告里必须点出**去哪儿改** —— 白名单搬到 crawler_config 之后，
		// 还写着"往 server.whitelist 里写"就是把运维指到一条已经废弃的路上。
		for _, want := range []string{"白名单为空", "任何来源", "ip_whitelist"} {
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

// 读库那一跳的语义：**行不存在 / 空值 / 值不是字符串数组 → 不限制来源**。
//
// 值的形状由 store 那层把关（用例在 admin/configstore），这里钉的是"读不出来时怎么继续"：
// 记一条 Error 然后按不限制来源走 —— 这一档等于把限制悄悄放宽了，日志里必须留痕。
func TestLoadIPWhitelistFallsBackToUnrestricted(t *testing.T) {
	newLog := func() (*logrus.Logger, *bytes.Buffer) {
		var buf bytes.Buffer
		log := logrus.New()
		log.SetOutput(&buf)
		log.SetLevel(logrus.ErrorLevel)
		return log, &buf
	}

	t.Run("行不存在 → 空（不限制）", func(t *testing.T) {
		f := gormfake.New()
		log, buf := newLog()
		if got := loadIPWhitelist(configstore.NewCachedStringListReader(configstore.New(gormfake.MustOpen(f)), time.Minute), log); len(got) != 0 {
			t.Fatalf("没设过白名单时应当是空，实得 %v", got)
		}
		if buf.Len() != 0 {
			t.Fatalf("「没配」是正常状态，不该记 Error：%s", buf.String())
		}
	})

	t.Run("正常值 → 读出来", func(t *testing.T) {
		f := gormfake.New()
		f.SetRow(map[string]driver.Value{"cfg_key": "ip_whitelist", "cfg_val": []byte(`["10.0.0.0/8"]`)})
		log, _ := newLog()
		got := loadIPWhitelist(configstore.NewCachedStringListReader(configstore.New(gormfake.MustOpen(f)), time.Minute), log)
		if len(got) != 1 || got[0] != "10.0.0.0/8" {
			t.Fatalf("实得 %v", got)
		}
	})

	t.Run("值不是字符串数组 → 不限制，并记 Error", func(t *testing.T) {
		f := gormfake.New()
		f.SetRow(map[string]driver.Value{"cfg_key": "ip_whitelist", "cfg_val": []byte(`{"a":1}`)})
		log, buf := newLog()
		if got := loadIPWhitelist(configstore.NewCachedStringListReader(configstore.New(gormfake.MustOpen(f)), time.Minute), log); len(got) != 0 {
			t.Fatalf("坏值应当按不限制来源继续，实得 %v", got)
		}
		if !strings.Contains(buf.String(), "不限制来源") {
			t.Fatalf("坏值必须留下痕迹，实得日志：%s", buf.String())
		}
	})

	t.Run("查库出错 → 不限制，并记 Error", func(t *testing.T) {
		f := gormfake.New()
		f.SetQueryError("crawler_config", "库抖了")
		log, buf := newLog()
		if got := loadIPWhitelist(configstore.NewCachedStringListReader(configstore.New(gormfake.MustOpen(f)), time.Minute), log); len(got) != 0 {
			t.Fatalf("库抖动时按不限制来源继续，实得 %v", got)
		}
		if buf.Len() == 0 {
			t.Fatal("查库出错必须记 Error")
		}
	})
}

// 读库那一跳的键名与 store 的常量必须一致 —— 两边各写一个字面量就会读到空。
func TestWhitelistKeyIsTheStoreConstant(t *testing.T) {
	if configstore.KeyIPWhitelist != "ip_whitelist" {
		t.Fatalf("键名变了：%q —— 与 docs 和配置注释里的说明要对得上", configstore.KeyIPWhitelist)
	}
}

// TTL 缓存：重载循环每 30 秒调一次 loadIPWhitelist，但 TTL 内不应该重复查库。
func TestLoadIPWhitelistUsesTTLCache(t *testing.T) {
	f := gormfake.New()
	f.SetRow(map[string]driver.Value{"cfg_key": "ip_whitelist", "cfg_val": []byte(`["10.0.0.0/8"]`)})
	reader := configstore.NewCachedStringListReader(configstore.New(gormfake.MustOpen(f)), time.Minute)
	log := logrus.New()
	log.SetLevel(logrus.FatalLevel)

	for i := 0; i < 3; i++ {
		if got := loadIPWhitelist(reader, log); len(got) != 1 || got[0] != "10.0.0.0/8" {
			t.Fatalf("第 %d 次读白名单 = %v", i+1, got)
		}
	}
	if hits := strings.Count(f.Written(), "crawler_config"); hits != 1 {
		t.Fatalf("TTL 内查库次数 = %d，want 1；实际 SQL：\n%s", hits, f.Written())
	}

	reader.Invalidate(configstore.KeyIPWhitelist)
	if got := loadIPWhitelist(reader, log); len(got) != 1 {
		t.Fatalf("失效后重读 = %v", got)
	}
	if hits := strings.Count(f.Written(), "crawler_config"); hits != 2 {
		t.Fatalf("Invalidate 后查库次数 = %d，want 2", hits)
	}
}
