package engine

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/internal/database"
	"gorm.io/gorm/schema"
)

// 手写 SQL 里的**列名**只要撞上 MySQL 保留字，就必须加引号 —— 否则 MySQL 把它当关键字，
// 整条语句 Error 1064；而且往往是静默的（调用处常常没判那条语句的错）。
//
// 这坑真踩过两次，都在已发布版本里：`repeat`（crawler_tasks，v3.1.0 起，写 `repeat + 1`）
// 与 `key`（crawler_sites，v3.2.0 起，写 `Where("key = ?")`）—— 见 CHANGELOG「修复 6」。
// （`crawler_sites` 后来并进了 `crawler_config` 的 `sites` 键，`key` 这一列没了；
// 判据是从 models 反射出来的，所以它自动跟着走，不用改清单。）
// 引擎测试跑的是假库（不解析 SQL），所以这类错只能靠"扫源码"或"真库跑一次"发现；这里做前者。
//
// 判据**不靠手写清单**：
//   - 保留字 = testdata/mysql_reserved_words.txt（从 MySQL 自己的 INFORMATION_SCHEMA.KEYWORDS 导的）；
//   - 列名 = models 包的 gorm schema（database.FrameworkModels）反射出来。
//
// 两者相交 = 必须加引号的列（今天是 `key` / `repeat`）；将来加新列、或换了 MySQL 版本新增保留字，
// 这条自动覆盖。只扫非测试 .go：生产代码里的手写 SQL 才是要拦的。
func TestRawSQLQuotesReservedWordColumns(t *testing.T) {
	reserved := loadReservedWords(t)
	mustQuote := map[string]bool{}
	for col := range frameworkColumns(t) {
		if reserved[strings.ToUpper(col)] {
			mustQuote[col] = true
		}
	}
	if len(mustQuote) == 0 {
		t.Fatal("没算出任何「必须加引号的列」—— models 或保留字清单没读到？这条测试会白跑")
	}
	names := make([]string, 0, len(mustQuote))
	for c := range mustQuote {
		names = append(names, c)
	}
	sort.Strings(names)
	t.Logf("必须加引号的列：%v（保留字共 %d 个）", names, len(reserved))

	// 自检：这条 lint 本身要有分辨力，否则只是"永远绿"
	tick, dq := "`", `"`
	for _, c := range []struct {
		lit  string
		want bool
	}{
		{dq + "repeat + 1" + dq, true},                     // 真踩过的那个
		{dq + tick + "repeat" + tick + " + 1" + dq, false}, // 加了引号
		{dq + "repeat" + dq, false},                        // map 键那种单字面量（gorm 自己会加引号）
		{dq + "ON DUPLICATE KEY UPDATE" + dq, false},       // SQL 关键字（大写）不算
		{dq + "repeat_interval = ?" + dq, false},           // 别的列名里含 repeat（词边界要认对）
	} {
		if got := len(bareReserved(c.lit, mustQuote)) > 0; got != c.want {
			t.Errorf("探针 %s：bareReserved = %v, want %v", c.lit, got, c.want)
		}
	}

	var hits []string
	fset := token.NewFileSet()
	_ = filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error { // engine/ → 模块根
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".idea", ".claude", "testdata", "node_modules", "bin", "dist", "logs", "downloads", "docs":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			bad := bareReserved(lit.Value, mustQuote)
			if len(bad) == 0 {
				return true
			}
			hits = append(hits, fmt.Sprintf("%s:%d 裸写的保留字列名 %v —— raw SQL 里要写 %s：%s",
				strings.TrimPrefix(path, ".."+string(filepath.Separator)), fset.Position(lit.Pos()).Line,
				bad, tick+strings.Join(bad, tick+", "+tick)+tick, snippet(lit.Value)))
			return true
		})
		return nil
	})
	sort.Strings(hits)
	if len(hits) > 0 {
		t.Errorf("手写 SQL 里出现未加引号的保留字列名（MySQL 会报 Error 1064）：\n  %s", strings.Join(hits, "\n  "))
	}
}

// sqlishRe 判断一条字面量"像不像 SQL"。松了就误报（日志文案里的英文单词会撞上列名，比如
// "repeat queue not configured"、"invalid page key"），紧了就漏 —— 判据取这几样：
// 占位符 `?`、SQL 关键字、以及"标识符 + 算术 + 数字"（`repeat + 1` 这种片段只有这一条能认出来）。
var sqlishRe = regexp.MustCompile(`(?i)(\?|(select|update|insert|delete|where|set|from|like|between|order by|group by|limit|join|values|concat|coalesce|now\(|unix_timestamp|on duplicate)|\w+\s*[+*/-]\s*\d)`)

// bareReserved 在一条 Go 字符串字面量（**源码原文**，带引号）里找"裸写的必须加引号的列名"。
// 四条规矩都是为了不误报：
//   - 只有"像 SQL"的字面量才看（见 sqlishRe）—— 日志文案、JSON 骨架都在这一步被滤掉；
//   - 反引号里的跳过 —— 那种写法本来就对（`key` = ?）；
//   - 只认小写 —— 列名习惯小写、SQL 关键字习惯大写，于是 `ON DUPLICATE KEY UPDATE` 不会被当成
//     裸写的 key（代价：真写成大写 REPEAT 会漏，那种只有真库跑一次才现形）；
//   - 单个词的字面量跳过（updates["repeat"] 这种 map 键、表格声明里的字段名 —— 它们不是 SQL，
//     gorm 自己会加引号）。
func bareReserved(lit string, mustQuote map[string]bool) []string {
	inner := strings.Trim(strings.Trim(lit, "`"), `"`) // 去掉两头的引号（反引号 / 双引号）
	if !sqlishRe.MatchString(inner) {
		return nil
	}
	var (
		out    []string
		tok    strings.Builder
		inTick bool
	)
	flush := func() {
		if tok.Len() == 0 {
			return
		}
		w := tok.String()
		tok.Reset()
		if !inTick && mustQuote[w] {
			out = append(out, w)
		}
	}
	for _, r := range inner {
		switch {
		case r == 96: // 反引号：先按当前状态收词，再翻状态
			flush()
			inTick = !inTick
		case r == '_' || r == '$' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			tok.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	sort.Strings(out)
	return slices.Compact(out)
}

// loadReservedWords 读 testdata 里那份（从 MySQL 的 INFORMATION_SCHEMA.KEYWORDS 导出的）保留字清单。
func loadReservedWords(t *testing.T) map[string]bool {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "mysql_reserved_words.txt"))
	if err != nil {
		t.Fatalf("读保留字清单：%v", err)
	}
	defer f.Close()
	out := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out[strings.ToUpper(line)] = true
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("读保留字清单：%v", err)
	}
	if len(out) < 100 {
		t.Fatalf("保留字清单看着不对（只有 %d 条）", len(out))
	}
	return out
}

// frameworkColumns 反射出框架各表在库里的**列名**（gorm schema，不连库）。
// 用的是 database.FrameworkModels 那份清单 —— 与建表同一个来源，加表加列自动跟上。
func frameworkColumns(t *testing.T) map[string]bool {
	t.Helper()
	cfg := &config.Config{}
	cfg.Server.OperationLog = true // 把两个开关都打开，五张表全算进来
	cfg.Crawler.Trace.Enabled = true
	out := map[string]bool{}
	cache := &sync.Map{}
	for _, m := range database.FrameworkModels(cfg) {
		sch, err := schema.Parse(m.Value, cache, schema.NamingStrategy{})
		if err != nil {
			t.Fatalf("解析模型 %s 的 schema：%v", m.Name, err)
		}
		for _, f := range sch.Fields {
			out[f.DBName] = true
		}
	}
	if len(out) == 0 {
		t.Fatal("没反射出任何列名")
	}
	return out
}

// snippet 把字面量裁短，报错信息里够看就行。
func snippet(lit string) string {
	s := strings.ReplaceAll(lit, "\n", " ")
	if len([]rune(s)) > 70 {
		s = string([]rune(s)[:70]) + "…"
	}
	return s
}
