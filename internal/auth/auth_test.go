package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v2/models"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// 造一个不会真的连库的 DB（DryRun + 跳过版本探测 + 关掉自动 Ping）。
func dryDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(mysql.New(mysql.Config{
		DSN:                       "u:p@tcp(127.0.0.1:3306)/x",
		SkipInitializeWithVersion: true,
	}), &gorm.Config{DryRun: true, DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("open dry-run db: %v", err)
	}
	return db
}

func TestHash(t *testing.T) {
	// 钉住算法：sha256 的十六进制（换个算法就会红，提醒兼容性）
	if got, want := Hash("abc"), "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"; got != want {
		t.Fatalf("Hash(abc) = %s, want %s", got, want)
	}
	h1, h2 := Hash("abc"), Hash("abc")
	if h1 != h2 {
		t.Fatal("Hash 应该确定")
	}
	if h1 == Hash("abd") {
		t.Fatal("Hash 应该区分不同输入")
	}
	if len(Hash("")) != 64 {
		t.Fatalf("Hash 长度 = %d, want 64", len(Hash("")))
	}
}

func TestExtract(t *testing.T) {
	cases := []struct {
		name string
		req  *http.Request
		want string
	}{
		{"Bearer", httptest.NewRequest("GET", "/api/x", nil).WithContext(context.Background()), "tok"},
		{"X-Auth-Key", httptest.NewRequest("GET", "/api/x", nil), "tok"},
		{"什么都没有", httptest.NewRequest("GET", "/api/x", nil), ""},
	}
	cases[0].req.Header.Set("Authorization", "Bearer tok")
	cases[1].req.Header.Set("X-Auth-Key", " tok ")
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Extract(c.req); got != c.want {
				t.Fatalf("Extract = %q, want %q", got, c.want)
			}
		})
	}

	// 刻意不看 ?key= —— 凭据进 URL 会落进历史与日志
	withQuery := httptest.NewRequest("GET", "/api/x?key=tok", nil)
	if got := Extract(withQuery); got != "" {
		t.Fatalf("Extract 不该认 ?key=，得到 %q", got)
	}
}

func TestOperatorContext(t *testing.T) {
	if got := OperatorFrom(context.Background()); got != "" {
		t.Fatalf("空上下文 = %q, want 空", got)
	}
	var nilCtx context.Context
	if got := OperatorFrom(nilCtx); got != "" {
		t.Fatalf("nil 上下文 = %q, want 空", got)
	}
	ctx := WithOperator(context.Background(), "张三")
	if got := OperatorFrom(ctx); got != "张三" {
		t.Fatalf("OperatorFrom = %q, want 张三", got)
	}
}

func TestVerifyGuardsWithoutDB(t *testing.T) {
	if _, ok, err := Verify(nil, "tok"); ok || err != nil {
		t.Fatalf("db 为 nil 时应 ok=false 且无错，得到 ok=%v err=%v", ok, err)
	}
	if _, ok, err := Verify(dryDB(t), ""); ok || err != nil {
		t.Fatalf("令牌为空时应 ok=false 且无错，得到 ok=%v err=%v", ok, err)
	}
}

// 校验查询必须按 hash 查、且带 enabled —— 不能拿明文去比。
func TestVerifiedQueryShape(t *testing.T) {
	db := dryDB(t)
	sql := db.ToSQL(func(tx *gorm.DB) *gorm.DB {
		return verifiedQuery(tx, "super-secret-token").Take(&models.AccessToken{})
	})
	if strings.Contains(sql, "super-secret-token") {
		t.Fatalf("SQL 里不该出现明文令牌：\n%s", sql)
	}
	if !strings.Contains(sql, Hash("super-secret-token")) {
		t.Fatalf("SQL 应按 hash 查：\n%s", sql)
	}
	if !strings.Contains(sql, "enabled") {
		t.Fatalf("SQL 应带 enabled 条件：\n%s", sql)
	}
}

// 没配过令牌（表是空的）→ 放行，视为"未配置凭据"；DryRun 下计数恒为 0，正好覆盖这一支。
func TestVerifierUnconfiguredAllows(t *testing.T) {
	v := Verifier(dryDB(t), nil)
	req := httptest.NewRequest("GET", "/api/x", nil)
	if _, ok := v(req); !ok {
		t.Fatal("表为空时应放行（未配置凭据）")
	}
	// nil db 视同未配置
	if _, ok := Verifier(nil, nil)(req); !ok {
		t.Fatal("db 为 nil 时应放行（未配置凭据）")
	}
}

type captureLogger struct{ lines []string }

func (c *captureLogger) Errorf(format string, args ...any) {
	c.lines = append(c.lines, fmt.Sprintf(format, args...))
}

func TestWarnIfNoToken(t *testing.T) {
	log := &captureLogger{}
	WarnIfNoToken(dryDB(t), log) // DryRun 下 total == 0 → 应警告
	if len(log.lines) != 1 || !strings.Contains(log.lines[0], "还没有任何访问令牌") {
		t.Fatalf("警告 = %v", log.lines)
	}
	WarnIfNoToken(dryDB(t), nil) // nil logger 不该 panic
}
