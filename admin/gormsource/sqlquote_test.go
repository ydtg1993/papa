package gormsource

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/ydtg1993/oao"
	"github.com/ydtg1993/papa/v3/models"
)

// 列名加引号这件事的**守护测试**，两层一起看才成立：
//
//  1. TestEveryDeclaredColumnIsQuoted（行为层）：把一张表的**整个声明面**（每个筛选算子、全局搜索、
//     多字段排序）一次跑过，断言每个列名在 SQL 里只以反引号形态出现、绝不裸奔。
//     列名清单是从**表格声明**里取的，不手写 —— 以后在声明里加一列/换个算子，这条自动跟上。
//  2. TestOnlyQuoteEmitsBackticks（结构层）：扫本包的**非测试**源码，反引号字面量只许出现在 quote() 里。
//     也就是"给列名加引号"只有一个出口；谁在别处手拼一对反引号，就等于绕开了那个出口。
//
// 为什么值得单独钉：列名来自代码声明、HTTP 参数进不来（那道墙见 TestUndeclaredFilterIsRejected），
// 所以这不是防注入 —— 是防**语法错**。内置「操作日志」表有一个叫 `table` 的列，`repeat` 也是保留字，
// 裸拼进 SQL 就是 MySQL Error 1064，表现是整个表格页 500。这类错 DryRun 看不见（只渲染不执行）、
// 假模型也看不见（字段不从模型来），只有真库跑一次或者靠静态检查才现形。

// TestEveryDeclaredColumnIsQuoted 把声明面全跑一遍，逐个列名验"带引号出现、且从不裸写"。
//
// 每个列名要求同时满足两件事：
//   - **至少以带引号的形态出现一次** —— 这是自检：某个算子要是压根没进 SQL（声明没生效、值被丢掉），
//     下面那条"不许裸奔"会因为它没出现而白过；
//   - **绝不裸奔** —— 把 SQL 里所有反引号标识符挖掉之后再找它，一次都不该找到。
func TestEveryDeclaredColumnIsQuoted(t *testing.T) {
	lg := &captureLogger{}
	db := dryDB(t, lg)
	// stage 不在 allOpsTable 的列里，专给全局搜索用（搜索列由业务声明，不在表格里）
	searchCols := []string{"url", "stage"}
	table := allOpsTable(New(Config{DB: db, Model: &models.CrawlerTask{}, Search: searchCols}))
	table.DefaultSort = "id" // 默认排序与前端排序都要过一遍

	// 一次带上全部算子（与 TestApplyFilterRendersEveryOperator 同一份声明面）：
	// 值都填成"解析得出来"的，免得被丢掉的条目不进 SQL、把下面的断言变成空转。
	sql := querySQL(t, table, strings.Join([]string{
		"filter[url]=abc",                           // OpLike
		"filter[stage]=pre",                         // OpPrefix
		"filter[status]=0,1",                        // OpIn + number
		"filter[repeatable]=true",                   // OpIn + bool
		"filter[repeat]=true",                       // OpEq + bool（列名本身就是 MySQL 保留字）
		"filter[pid]=7",                             // OpEq + number
		"filter[retry]=3",                           // OpLt
		"filter[reprocess]=5",                       // OpGt
		"filter[id]=10..20",                         // OpBetween + number
		"filter[created_at]=2024-01-01..2024-01-31", // OpBetween + time
		"sort=stage,-pid",                           // 多字段排序（升 + 降）
		"search=kw",                                 // 全局搜索
	}, "&"), lg)

	// 先确认这一轮真的把三类子句都带上了，否则下面全是空转的白过
	for _, want := range []string{"LIKE", "ORDER BY"} {
		if !strings.Contains(sql, want) {
			t.Fatalf("这轮请求没带上 %s，下面的断言会白过：\n%s", want, sql)
		}
	}

	bare := blankQuotedIdents(sql)
	var bad []string
	for _, col := range declaredColumns(table, searchCols) {
		if !strings.Contains(sql, "`"+col+"`") {
			bad = append(bad, fmt.Sprintf("%s：没以带引号的形态出现过（算子没生效？还是这条断言该换判据）", col))
			continue
		}
		for _, hit := range bareIdentifier(bare, col) {
			bad = append(bad, fmt.Sprintf("%s：有裸写（…%s…）", col, hit))
		}
	}
	if len(bad) > 0 {
		t.Errorf("列名必须一律经 quote() 加反引号，裸写会撞保留字报 1064：\n  %s\n生成的 SQL：\n%s",
			strings.Join(bad, "\n  "), sql)
	}
}

// TestOnlyQuoteEmitsBackticks 结构层：本包非测试源码里的反引号**字面量**只许出现在 quote() 里。
//
// 判据取"字面量里含反引号"：给列名加引号的写法必然是 `"`" + col + "`"` 或 `"`col`"`，两种都带
// 反引号字面量；而 SQL 的其余部分（WHERE / ORDER BY / LIKE 那些片段）本来一个反引号都不含。
// 于是"别处冒出反引号字面量"就等价于"有人另开了一个加引号的出口"—— 那正是这条要拦的。
// 注释不算（只看 AST 里的字符串字面量），所以注释里写 `table` 不会误报。
//
// **盲区（写清楚）**：拦不住 `fmt.Sprintf("%s = ?", col)` 这类**不加引号**的拼法 —— 那种靠
// TestEveryDeclaredColumnIsQuoted 兜；也拦不住把列名塞进本包没扫到的 gorm 入口（Select/Group/Joins…），
// 今天这三处（筛选、搜索、排序）就是全部出口，将来新增出口时请同时把它加进行为层那份扫描。
func TestOnlyQuoteEmitsBackticks(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读包目录：%v", err)
	}
	var (
		hits    []string
		inQuote int
		scanned []string
	)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue // 只扫生产代码：测试里为了断言本来就写满了带引号的 SQL 片段
		}
		scanned = append(scanned, name)
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("解析 %s：%v", name, err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING || !strings.Contains(lit.Value, "`") {
					return true
				}
				if fn.Name.Name == "quote" {
					inQuote++
					return true
				}
				hits = append(hits, fmt.Sprintf("%s:%d %s() 里手拼了反引号字面量 %s",
					name, fset.Position(lit.Pos()).Line, fn.Name.Name, lit.Value))
				return true
			})
		}
	}
	// 自检：quote() 里找不到那对反引号，说明扫描本身失效了（改了写法、或被 _test 过滤掉），
	// 这条会静默变成"永远绿"。
	if inQuote == 0 {
		t.Fatalf("自检失败：在 %v 里没扫到 quote() 的反引号字面量 —— 这条守卫白跑了", scanned)
	}
	sort.Strings(hits)
	if len(hits) > 0 {
		t.Errorf("列名加引号只该走 quote()（gormsource.go），别处手拼相当于绕开这个出口：\n  %s",
			strings.Join(hits, "\n  "))
	}
}

var quotedIdentRe = regexp.MustCompile("`[^`]*`")

// blankQuotedIdents 把 SQL 里所有 `反引号标识符` 换成一个 @，剩下的部分就不该再出现任何列名。
// （连表名 `crawler_tasks` 也一起被挖掉 —— 那正是想要的：只留"裸奔的标识符"给下面找。）
func blankQuotedIdents(sql string) string { return quotedIdentRe.ReplaceAllString(sql, "@") }

// bareIdentifier 在 s 里找**独立的** col（前后都不是标识符字符），返回带上下文的片段便于定位。
//
//   - 前后判标识符字符是为了不让 `pid` 命中 `id`、`created_at` 命中 `at`；
//   - **区分大小写**：列名习惯小写、SQL 关键字习惯大写，于是 `ORDER BY` 不会被当成裸写的 order
//     （代价：真把列名写成大写会漏 —— 与 engine/sqlreserved_test.go 同一处取舍）。
func bareIdentifier(s, col string) []string {
	re := regexp.MustCompile(`(?:^|[^0-9A-Za-z_$])` + regexp.QuoteMeta(col) + `(?:[^0-9A-Za-z_$]|$)`)
	var out []string
	for _, loc := range re.FindAllStringIndex(s, -1) {
		lo, hi := loc[0]-12, loc[1]+12
		if lo < 0 {
			lo = 0
		}
		if hi > len(s) {
			hi = len(s)
		}
		out = append(out, strings.TrimSpace(s[lo:hi]))
	}
	return out
}

// declaredColumns 从**表格声明**里取列名（Columns + Filters），再并上搜索列 —— 不手写清单，
// 声明加一列这条自动跟上。
func declaredColumns(tbl oao.Table, search []string) []string {
	set := make(map[string]bool, len(tbl.Columns)+len(tbl.Filters)+len(search))
	for _, c := range tbl.Columns {
		set[c.Field] = true
	}
	for _, f := range tbl.Filters {
		set[f.Field] = true
	}
	for _, c := range search {
		set[c] = true
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}
