package auth

import (
	"database/sql/driver"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v3/internal/gormfake"
)

// 门禁的 fail-closed 矩阵。
//
// 以前这几条只能靠读代码相信：本包的 dryDB 是 DryRun（只拼 SQL、不返回行），
// 「查库出错就拒绝」「配过但全停用也拒绝」这些分支根本走不到。用 internal/gormfake
// （能返回行、能按语句配错误）把它们全走一遍 —— 这三条是安全相关，值得钉死。
func TestVerifierFailClosed(t *testing.T) {
	alice := map[string]driver.Value{"operator": "alice"}
	for _, tc := range []struct {
		name   string
		setup  func(*gormfake.DB)
		header map[string]string
		wantOp string
		wantOK bool
	}{
		{
			name:   "一条令牌都没配 → 放行（还没配凭据）",
			setup:  func(f *gormfake.DB) { f.SetCounts(0, 0) },
			header: map[string]string{"Authorization": "Bearer whatever"},
			wantOK: true,
		},
		{
			name:   "配过但全被停用 → 拒绝（fail closed：停用最后一条不等于敞开）",
			setup:  func(f *gormfake.DB) { f.SetCounts(0, 3) },
			header: map[string]string{"Authorization": "Bearer whatever"},
			wantOK: false,
		},
		{
			name:   "有启用令牌 + 令牌对 → 放行并带出操作人",
			setup:  func(f *gormfake.DB) { f.SetCounts(1, 1); f.SetRow(alice) },
			header: map[string]string{"Authorization": "Bearer good-token"},
			wantOp: "alice",
			wantOK: true,
		},
		{
			name:   "X-Auth-Key 头也认（不是只有 Authorization）",
			setup:  func(f *gormfake.DB) { f.SetCounts(1, 1); f.SetRow(alice) },
			header: map[string]string{"X-Auth-Key": "good-token"},
			wantOp: "alice",
			wantOK: true,
		},
		{
			name:   "有启用令牌 + 令牌不对（查不到行） → 拒绝",
			setup:  func(f *gormfake.DB) { f.SetCounts(1, 1); f.SetRow(nil) },
			header: map[string]string{"Authorization": "Bearer bad"},
			wantOK: false,
		},
		{
			name:   "统计启用的令牌出错 → 拒绝（fail closed）",
			setup:  func(f *gormfake.DB) { f.SetCounts(1, 1); f.SetQueryError("count(", "db down") },
			header: map[string]string{"Authorization": "Bearer good"},
			wantOK: false,
		},
		{
			name:   "一条都没配时，第二次统计（总数）出错 → 拒绝",
			setup:  func(f *gormfake.DB) { f.SetCounts(0, 0); f.SetQueryError("count(", "db down") },
			header: map[string]string{"Authorization": "Bearer good"},
			wantOK: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := gormfake.New()
			tc.setup(f)
			verify := Verifier(gormfake.MustOpen(f), nil)

			r := httptest.NewRequest("GET", "/api/monitor", nil)
			for k, v := range tc.header {
				r.Header.Set(k, v)
			}
			op, ok := verify(r)
			if ok != tc.wantOK || op != tc.wantOp {
				t.Fatalf("Verifier = (%q, %v), want (%q, %v)", op, ok, tc.wantOp, tc.wantOK)
			}
		})
	}
}

// 取令牌那一句自己出错（数数正常）→ 也要拒绝。这条单独测，因为它的错误来自另一条语句。
func TestVerifierDeniesWhenTokenLookupFails(t *testing.T) {
	f := gormfake.New()
	f.SetCounts(1, 1)
	f.SetQueryError("token_hash", "db down")
	verify := Verifier(gormfake.MustOpen(f), nil)

	r := httptest.NewRequest("GET", "/api/monitor", nil)
	r.Header.Set("Authorization", "Bearer good")
	if op, ok := verify(r); ok || op != "" {
		t.Fatalf("取令牌出错时应当拒绝，实得 (%q, %v)", op, ok)
	}
}

// 二次确认（关停这类高危操作）：语义必须与 Verifier 一致，否则会出现"能进后台却关不掉"的死角。
func TestConfirmerMatchesVerifierSemantics(t *testing.T) {
	t.Run("一条都没配 → 放行（不看传什么）", func(t *testing.T) {
		f := gormfake.New()
		f.SetCounts(0, 0)
		if op, ok := Confirmer(gormfake.MustOpen(f), nil)(""); !ok || op != "" {
			t.Fatalf("还没配凭据时应当放行，实得 (%q, %v)", op, ok)
		}
	})
	t.Run("配过 + 对的令牌 → 放行并带出操作人", func(t *testing.T) {
		f := gormfake.New()
		f.SetCounts(1, 1)
		f.SetRow(map[string]driver.Value{"operator": "alice"})
		if op, ok := Confirmer(gormfake.MustOpen(f), nil)("  good  "); !ok || op != "alice" {
			t.Fatalf("对的令牌应当放行（两侧空白要裁掉），实得 (%q, %v)", op, ok)
		}
	})
	t.Run("配过 + 令牌不对 → 拒绝", func(t *testing.T) {
		f := gormfake.New()
		f.SetCounts(1, 1)
		f.SetRow(nil)
		if op, ok := Confirmer(gormfake.MustOpen(f), nil)("bad"); ok || op != "" {
			t.Fatalf("令牌不对应当拒绝，实得 (%q, %v)", op, ok)
		}
	})
	t.Run("统计出错 → 拒绝", func(t *testing.T) {
		f := gormfake.New()
		f.SetQueryError("count(", "db down")
		if op, ok := Confirmer(gormfake.MustOpen(f), nil)("good"); ok || op != "" {
			t.Fatalf("查库出错应当拒绝，实得 (%q, %v)", op, ok)
		}
	})
}

// 启动警告：一条都没有 → 喊一声"对白名单完全开放"；有令牌 → 不喊；查库出错 → 记 Error。
func TestWarnIfNoTokenBranches(t *testing.T) {
	t.Run("一条都没有 → 警告", func(t *testing.T) {
		f := gormfake.New()
		f.SetCounts(0, 0)
		log := &captureLogger{}
		WarnIfNoToken(gormfake.MustOpen(f), log)
		if !strings.Contains(strings.Join(log.lines, " "), "还没有任何访问令牌") {
			t.Fatalf("应当警告「还没有任何访问令牌」，实得 %q", strings.Join(log.lines, " "))
		}
	})
	t.Run("有启用的令牌 → 不喊", func(t *testing.T) {
		f := gormfake.New()
		f.SetCounts(2, 3)
		log := &captureLogger{}
		WarnIfNoToken(gormfake.MustOpen(f), log)
		if strings.Join(log.lines, " ") != "" {
			t.Fatalf("配了令牌就不该喊，实得 %q", strings.Join(log.lines, " "))
		}
	})
	t.Run("查库出错 → 记 Error（别静默）", func(t *testing.T) {
		f := gormfake.New()
		f.SetQueryError("count(", "db down")
		log := &captureLogger{}
		WarnIfNoToken(gormfake.MustOpen(f), log)
		if !strings.Contains(strings.Join(log.lines, " "), "db down") {
			t.Fatalf("查库出错要留下痕迹，实得 %q", strings.Join(log.lines, " "))
		}
	})
}
