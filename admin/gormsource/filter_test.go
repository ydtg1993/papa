package gormsource

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ydtg1993/oao"
	"github.com/ydtg1993/papa/v2/models"
)

// allOpsTable 一份把各算子都声明齐的表格：声明驱动意味着"没声明就进不来"，
// 所以要验算子就必须先在声明里放出来。
func allOpsTable(source oao.Source) oao.Table {
	return oao.Table{
		Key:    "task",
		Source: source,
		Columns: []oao.Column{
			{Field: "id", Kind: oao.KindNumber}, {Field: "url"}, {Field: "stage"},
			{Field: "pid", Kind: oao.KindNumber}, {Field: "status", Kind: oao.KindNumber},
			{Field: "retry", Kind: oao.KindNumber}, {Field: "reprocess", Kind: oao.KindNumber},
			{Field: "created_at", Kind: oao.KindTime}, {Field: "repeatable", Kind: oao.KindBool},
		},
		Filters: []oao.Filter{
			{Field: "url", Op: oao.OpLike},
			{Field: "stage", Op: oao.OpPrefix},
			{Field: "status", Kind: oao.KindNumber, Op: oao.OpIn},
			{Field: "pid", Kind: oao.KindNumber, Op: oao.OpEq},
			{Field: "retry", Kind: oao.KindNumber, Op: oao.OpLt},
			{Field: "reprocess", Kind: oao.KindNumber, Op: oao.OpGt},
			{Field: "id", Kind: oao.KindNumber, Op: oao.OpBetween},
			{Field: "created_at", Kind: oao.KindTime, Op: oao.OpBetween},
			{Field: "repeatable", Kind: oao.KindBool, Op: oao.OpIn},
			{Field: "repeat", Kind: oao.KindBool, Op: oao.OpEq}, // 复用 bool 等值：随便挂一个列
		},
	}
}

// querySQL 起一个只挂这一张表的组件，发一次请求，返回它生成的 SQL。
//
// lg 必须是构造 table.Source 时用的那个 —— Source 里的 *gorm.DB 才是真正执行查询的那个。
func querySQL(t *testing.T, table oao.Table, query string, lg *captureLogger) string {
	t.Helper()
	o, err := oao.New(oao.Config{Tables: []oao.Table{table}})
	if err != nil {
		t.Fatalf("oao.New: %v", err)
	}
	mux := http.NewServeMux()
	o.Mount(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/oao/" + table.Key + "?" + query)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return lg.all()
}

// 每个算子都要落到正确的 SQL 形态。这里一次把能填的都填上，
// 断言的是"算子真的被解释了"，而不是"某一条恰好对"。
func TestApplyFilterRendersEveryOperator(t *testing.T) {
	lg := &captureLogger{}
	db := dryDB(t, lg)
	table := allOpsTable(New(Config{DB: db, Model: &models.CrawlerTask{}}))

	sql := querySQL(t, table, strings.Join([]string{
		"filter[url]=abc",                           // OpLike：前后通配
		"filter[stage]=pre",                         // OpPrefix：只有后通配
		"filter[status]=0,1",                        // OpIn + number
		"filter[repeatable]=true",                   // OpIn + bool
		"filter[pid]=7",                             // OpEq + number
		"filter[retry]=3",                           // OpLt
		"filter[reprocess]=5",                       // OpGt
		"filter[id]=10..20",                         // OpBetween + number → BETWEEN
		"filter[created_at]=2024-01-01..2024-01-31", // OpBetween + time → 半开区间
	}, "&"), lg)

	for _, want := range []string{
		"`url` LIKE '%abc%'",
		"`stage` LIKE 'pre%'",
		"`status` IN (0,1)",
		"`repeatable` IN (true)",
		"`pid` = 7",
		// 数值一律以**字面量**绑定：声明里写了 KindNumber 就按整数解析，
		// 不能像以前那样把 Range()/Raw() 的字符串直接绑上去让 MySQL 隐式转换。
		"`retry` < 3",
		"`reprocess` > 5",
		"`id` BETWEEN 10 AND 20",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("SQL 里缺少 %q：\n%s", want, sql)
		}
	}

	// 反向钉住：数值条件里**不许**出现带引号的数字字面量。
	// 这些串在生成的 SQL 里不会由别处产生（LIKE 的 pattern、两端的日期字面量都不是这个形态）。
	for _, quoted := range []string{"'10'", "'20'", "'3'", "'5'", "'7'"} {
		if strings.Contains(sql, quoted) {
			t.Errorf("数值被当字符串绑定了（出现 %s）：\n%s", quoted, sql)
		}
	}

	// 日期区间用半开 [from, end)：只有这样才能把"结束日当天"整段框进来。
	// 不管底层怎么渲染，必须同时出现 >= 与 < 两个比较。
	if !strings.Contains(sql, "`created_at` >=") || !strings.Contains(sql, "`created_at` <") {
		t.Errorf("日期区间应是半开区间（>= from AND < end）：\n%s", sql)
	}
	if strings.Contains(sql, "`created_at` <=") {
		t.Errorf("日期区间不该用闭区间（会漏掉结束日当天）：\n%s", sql)
	}
}

// 值解析不出来时**丢掉这个条件**，而不是拼一条错的、也不是报错整页 500。
// 这条覆盖的是几个 `if !ok { return db }` 分支。
func TestApplyFilterDropsUnparsableValues(t *testing.T) {
	cases := []struct {
		name  string
		query string
		col   string
	}{
		{"number 等值给非数字", "filter[pid]=abc", "`pid`"},
		{"number IN 给非数字", "filter[status]=abc", "`status`"},
		{"number BETWEEN 没给区间", "filter[id]=5", "`id`"},
		{"number BETWEEN 两端非数字", "filter[id]=abc..def", "`id`"},
		{"number BETWEEN 只有一端是数字", "filter[id]=10..def", "`id`"},
		{"number 大于给非数字", "filter[reprocess]=abc", "`reprocess`"},
		{"number 小于给非数字", "filter[retry]=abc", "`retry`"},
		{"time BETWEEN 给的不是日期区间", "filter[created_at]=notadate", "`created_at`"},
		{"bool IN 给认不出的词", "filter[repeatable]=maybe", "`repeatable`"},
		{"bool 等值给认不出的词", "filter[repeat]=maybe", "`repeat`"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lg := &captureLogger{}
			db := dryDB(t, lg)
			table := allOpsTable(New(Config{DB: db, Model: &models.CrawlerTask{}}))

			sql := querySQL(t, table, c.query, lg)
			if strings.Contains(sql, c.col) {
				t.Fatalf("值解析不出来时该丢掉这条条件，不该拿 %s 去拼 SQL：\n%s", c.col, sql)
			}
		})
	}
}

// 值为空（没填这个筛选）时压根不该进 WHERE —— 否则每次列表都带一堆恒真条件。
func TestEmptyFilterValueIsIgnored(t *testing.T) {
	lg := &captureLogger{}
	db := dryDB(t, lg)
	table := allOpsTable(New(Config{DB: db, Model: &models.CrawlerTask{}}))

	sql := querySQL(t, table, "filter[url]=&filter[stage]=%20%20", lg)
	if strings.Contains(sql, "`url`") || strings.Contains(sql, "`stage`") {
		t.Fatalf("空值/纯空白不该进 WHERE：\n%s", sql)
	}
}

// 未在声明里登记的字段，HTTP 参数里带了也进不来（列名全部来自代码声明，这就是防注入的那道墙）。
func TestUndeclaredFilterIsRejected(t *testing.T) {
	lg := &captureLogger{}
	db := dryDB(t, lg)
	table := allOpsTable(New(Config{DB: db, Model: &models.CrawlerTask{}}))

	sql := querySQL(t, table, "filter[idempotency_key]=x&filter[url]=ok", lg)
	if strings.Contains(sql, "idempotency_key") {
		t.Fatalf("未声明的字段不该进 SQL：\n%s", sql)
	}
	if !strings.Contains(sql, "`url`") {
		t.Fatalf("已声明的字段应照常生效：\n%s", sql)
	}
}

// 多字段排序按声明顺序拼（前端传的字段也只在声明白名单里才认）。
func TestSortFieldsReachSQLInOrder(t *testing.T) {
	lg := &captureLogger{}
	db := dryDB(t, lg)
	table := allOpsTable(New(Config{DB: db, Model: &models.CrawlerTask{}}))
	table.DefaultSort = "-id"

	sql := querySQL(t, table, "sort=stage,-pid", lg)
	// 至少 DefaultSort 或前端给的排序要落进 ORDER BY，且列名带反引号
	if !strings.Contains(sql, "ORDER BY") {
		t.Fatalf("应生成 ORDER BY：\n%s", sql)
	}
	if !strings.Contains(sql, "`id` DESC") && !strings.Contains(sql, "`stage`") {
		t.Fatalf("排序字段没落进 SQL：\n%s", sql)
	}
}

// 分页：offset/limit 必须由 page/size 算出来。
func TestPaginationReachesSQL(t *testing.T) {
	lg := &captureLogger{}
	db := dryDB(t, lg)
	table := allOpsTable(New(Config{DB: db, Model: &models.CrawlerTask{}}))

	sql := querySQL(t, table, "page=3&size=10", lg)
	if !strings.Contains(sql, "LIMIT 10") {
		t.Fatalf("LIMIT 不对：\n%s", sql)
	}
	if !strings.Contains(sql, "OFFSET 20") {
		t.Fatalf("OFFSET 应为 (page-1)*size：\n%s", sql)
	}
}

// 全局搜索：Search 声明的列做 OR LIKE，且列名带反引号（保留字列才拼得进去）。
func TestSearchUsesOrLikeOverDeclaredColumns(t *testing.T) {
	lg := &captureLogger{}
	db := dryDB(t, lg)
	o := New(Config{DB: db, Model: &models.OperationLog{}, Search: []string{"table", "action"}})
	table := oao.Table{
		Key: "oplog", Source: o,
		Columns: []oao.Column{{Field: "id", Kind: oao.KindNumber}, {Field: "table"}, {Field: "action"}},
	}

	sql := querySQL(t, table, "search=kw", lg)
	if !strings.Contains(sql, "`table` LIKE '%kw%'") || !strings.Contains(sql, "`action` LIKE '%kw%'") {
		t.Fatalf("全局搜索应在声明列上做 OR LIKE：\n%s", sql)
	}
	if !strings.Contains(sql, " OR ") {
		t.Fatalf("多列搜索应当是 OR：\n%s", sql)
	}
}

// 没声明 Search 列时，search 参数不该凭空生成条件。
func TestSearchWithoutDeclaredColumnsIsIgnored(t *testing.T) {
	lg := &captureLogger{}
	db := dryDB(t, lg)
	table := allOpsTable(New(Config{DB: db, Model: &models.CrawlerTask{}})) // Search 为空

	sql := querySQL(t, table, "search=kw", lg)
	if strings.Contains(sql, "LIKE") {
		t.Fatalf("没声明可搜列时不该生成 LIKE：\n%s", sql)
	}
}

// 非数值列的大小/区间比较保持字符串语义 —— 按 Kind 分流，不能一刀切全转成整数。
// （现仓库里没有这样的声明，但分流写错了会在业务第一次这么声明时静默变味。）
func TestComparisonKeepsStringSemanticsForNonNumberKinds(t *testing.T) {
	// 一个字段只能声明一次（oao 按字段名建表），所以每种算子分两次请求。
	for _, c := range []struct {
		name  string
		op    oao.Op
		query string
		want  string
	}{
		{"大于", oao.OpGt, "filter[title]=abc", "`title` > 'abc'"},
		{"小于", oao.OpLt, "filter[title]=abc", "`title` < 'abc'"},
		{"区间", oao.OpBetween, "filter[title]=a..z", "`title` BETWEEN 'a' AND 'z'"},
	} {
		t.Run(c.name, func(t *testing.T) {
			lg := &captureLogger{}
			db := dryDB(t, lg)
			table := oao.Table{
				Key:     "task",
				Source:  New(Config{DB: db, Model: &models.CrawlerTask{}}),
				Columns: []oao.Column{{Field: "id", Kind: oao.KindNumber}, {Field: "title"}},
				// 没写 Kind = 字符串列
				Filters: []oao.Filter{{Field: "title", Op: c.op}},
			}

			sql := querySQL(t, table, c.query, lg)
			if !strings.Contains(sql, c.want) {
				t.Fatalf("应保持字符串绑定（%s）：\n%s", c.want, sql)
			}
		})
	}
}
