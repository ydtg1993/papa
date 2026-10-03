package gormsource

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/oao"
	"github.com/ydtg1993/papa/v2/models"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// captureLogger 记下 GORM 生成的 SQL —— DryRun 下不连库，但语句照样会过日志。
type captureLogger struct{ sql []string }

func (l *captureLogger) LogMode(gormlogger.LogLevel) gormlogger.Interface { return l }
func (l *captureLogger) Info(context.Context, string, ...any)             {}
func (l *captureLogger) Warn(context.Context, string, ...any)             {}
func (l *captureLogger) Error(context.Context, string, ...any)            {}

func (l *captureLogger) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	s, _ := fc()
	l.sql = append(l.sql, s)
}

func (l *captureLogger) all() string { return strings.Join(l.sql, "\n") }

// dryDB 造一个不会真的连库的 DB：DryRun + 跳过版本探测 + 关掉 Open 后的自动 Ping。
func dryDB(t *testing.T, lg *captureLogger) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(mysql.New(mysql.Config{
		DSN:                       "u:p@tcp(127.0.0.1:3306)/x",
		SkipInitializeWithVersion: true,
	}), &gorm.Config{
		DryRun:               true,
		DisableAutomaticPing: true,
		Logger:               lg,
	})
	if err != nil {
		t.Fatalf("open dry-run db: %v", err)
	}
	return db
}

// recordingSource 包一层，把组件交给 Source 的 Query 记下来。
type recordingSource struct {
	inner *Source
	got   oao.Query
}

func (r *recordingSource) List(ctx context.Context, q oao.Query) ([]map[string]any, int64, error) {
	r.got = q
	return r.inner.List(ctx, q)
}

// 走完整的 组件 → Query → Source → SQL 链路，断言 OpPrefix 真的变成「只有后通配」的 LIKE。
// 这条覆盖的是曾经的静默错误：OpPrefix 落到 default 分支被当成等值。
func TestOpPrefixReachesSQL(t *testing.T) {
	lg := &captureLogger{}
	db := dryDB(t, lg)
	rec := &recordingSource{inner: New(Config{
		DB: db, Model: &models.CrawlerTask{}, Search: []string{"url"},
	})}

	o, err := oao.New(oao.Config{Tables: []oao.Table{{
		Key: "task", Source: rec,
		Columns: []oao.Column{{Field: "id", Kind: oao.KindNumber}, {Field: "url"}},
		Filters: []oao.Filter{
			{Field: "url", Op: oao.OpPrefix},
			{Field: "id", Kind: oao.KindNumber, Op: oao.OpEq},
			{Field: "stage", Op: oao.OpEq}, // 未填值，不该进 SQL
		},
	}}})
	if err != nil {
		t.Fatalf("oao.New: %v", err)
	}
	mux := http.NewServeMux()
	o.Mount(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/oao/task?filter[url]=abc&filter[id]=42")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// 1) 算子原样透传给 Source（未声明的字段进不来，声明过的带算子）
	if got := rec.got.Get("url").Op(); got != oao.OpPrefix {
		t.Fatalf("Source 拿到的 url 算子 = %q, want prefix", got)
	}
	if got := rec.got.Get("stage"); !got.Empty() {
		t.Fatalf("未填值的筛选不该出现：%q", got.Raw())
	}

	// 2) 落到 SQL：前缀只补后通配；等值仍然是 =
	sql := lg.all()
	if !strings.Contains(sql, "LIKE") {
		t.Fatalf("SQL 里没有 LIKE：\n%s", sql)
	}
	if strings.Contains(sql, "%abc%") {
		t.Fatalf("prefix 不该补前导通配：\n%s", sql)
	}
	if !strings.Contains(sql, "abc%") {
		t.Fatalf("prefix 应补后通配 abc%%：\n%s", sql)
	}
	if !strings.Contains(sql, "= 42") {
		t.Fatalf("OpEq 应生成等值条件：\n%s", sql)
	}
}

// 筛选/搜索拼的列名必须带反引号。回归点：内置「操作日志」表有一个叫 `table` 的列，
// 而 table 是 MySQL 保留字 —— 裸拼进 SQL 是语法错，整页 500（排序那条一直有引号，这两条漏了）。
func TestFilterAndSearchColumnsAreQuoted(t *testing.T) {
	lg := &captureLogger{}
	db := dryDB(t, lg)
	o, err := oao.New(oao.Config{Tables: []oao.Table{{
		Key:    "oplog",
		Source: New(Config{DB: db, Model: &models.OperationLog{}, Search: []string{"table", "action"}}),
		Columns: []oao.Column{
			{Field: "id", Kind: oao.KindNumber}, {Field: "table"}, {Field: "action"}, {Field: "ok", Kind: oao.KindBool},
		},
		Filters: []oao.Filter{
			{Field: "table", Op: oao.OpEq},
			{Field: "action", Op: oao.OpLike},
			{Field: "ok", Kind: oao.KindBool, Op: oao.OpIn,
				Options: map[string]string{"true": "成功", "false": "失败"}},
		},
	}}})
	if err != nil {
		t.Fatalf("oao.New: %v", err)
	}
	mux := http.NewServeMux()
	o.Mount(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/oao/oplog?search=task&filter[table]=crawler_task&filter[action]=edit&filter[ok]=true")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	sql := lg.all()
	// 1) 列名一律带反引号（保留字 table 才拼得进去）
	for _, want := range []string{"`table` LIKE", "`action` LIKE", "`table` ="} {
		if !strings.Contains(sql, want) {
			t.Fatalf("SQL 里缺少带引号的 %q：\n%s", want, sql)
		}
	}
	// 2) 别再有裸列名
	for _, bad := range []string{"WHERE table ", "AND table ", " table LIKE", " action LIKE"} {
		if strings.Contains(sql, bad) {
			t.Fatalf("列名没加反引号（%q）：\n%s", bad, sql)
		}
	}
	// 3) bool 的 IN 必须是布尔字面量，不能是字符串 —— 否则「成功」筛出失败行
	if !strings.Contains(sql, "`ok` IN (true)") {
		t.Fatalf("bool 的 OpIn 应渲染成 `ok` IN (true)：\n%s", sql)
	}
	if strings.Contains(sql, "'true'") {
		t.Fatalf("bool 不该以字符串形式进 SQL：\n%s", sql)
	}
}
