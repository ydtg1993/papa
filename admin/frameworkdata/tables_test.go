package frameworkdata

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/ydtg1993/oao"
	"github.com/ydtg1993/papa/v3/internal/database"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// A real database/sql -> GORM -> HTTP path, with a bounded synthetic driver.
// SQL shape is asserted separately; this driver does not emulate MySQL syntax.
type fixture struct {
	mu      sync.Mutex
	queries []string
	err     error
	columns []string
	row     []driver.Value
}
type fixtureDriver struct{}
type fixtureConn struct{ f *fixture }
type fixtureRows struct {
	columns []string
	values  []driver.Value
	done    bool
}

var fixtures sync.Map
var fixtureID atomic.Uint64

func init() { sql.Register("papa-framework-data-test", fixtureDriver{}) }
func (fixtureDriver) Open(name string) (driver.Conn, error) {
	f, _ := fixtures.Load(name)
	return &fixtureConn{f.(*fixture)}, nil
}
func (*fixtureConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (*fixtureConn) Close() error { return nil }
func (*fixtureConn) Begin() (driver.Tx, error) {
	return nil, errors.New("read-only fixture: transaction rejected")
}
func (c *fixtureConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.queries = append(c.f.queries, query)
	if c.f.err != nil {
		return nil, c.f.err
	}
	if strings.Contains(query, "count(*)") {
		return &fixtureRows{columns: []string{"count"}, values: []driver.Value{int64(1)}}, nil
	}
	if strings.HasPrefix(query, "SELECT 1 ") {
		return &fixtureRows{columns: []string{"1"}, done: true}, nil
	}
	return &fixtureRows{columns: c.f.columns, values: c.f.row, done: len(c.f.row) == 0}, nil
}
func (r *fixtureRows) Columns() []string { return r.columns }
func (*fixtureRows) Close() error        { return nil }
func (r *fixtureRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	copy(dest, r.values)
	r.done = true
	return nil
}

func testDB(t *testing.T) (*gorm.DB, *fixture) {
	t.Helper()
	f := &fixture{columns: []string{"id"}, row: []driver.Value{int64(1)}}
	name := fmt.Sprint(fixtureID.Add(1))
	fixtures.Store(name, f)
	conn, err := sql.Open("papa-framework-data-test", name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(); fixtures.Delete(name) })
	db, err := gorm.Open(mysql.New(mysql.Config{Conn: conn, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatal(err)
	}
	return db, f
}
func authenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test-Auth") != "yes" {
			w.WriteHeader(401)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func testMux(t *testing.T, db *gorm.DB) *http.ServeMux {
	t.Helper()
	tables, err := Tables(db)
	if err != nil {
		t.Fatal(err)
	}
	o, err := oao.New(oao.Config{Tables: tables, Auth: authenticated})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	o.Mount(mux)
	Register(mux, db, authenticated)
	return mux
}
func request(mux *http.ServeMux, method, path string, authorized bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	if authorized {
		r.Header.Set("X-Test-Auth", "yes")
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func TestFrameworkCatalogCoversAllModelsReadOnly(t *testing.T) {
	tables, err := Tables(nil)
	if err != nil {
		t.Fatal(err)
	}
	models := database.AllFrameworkModels()
	if len(tables) != 18 || len(tables) != len(models) {
		t.Fatalf("catalog/model count = %d/%d", len(tables), len(models))
	}
	for i, table := range tables {
		if table.Key != models[i].Name || len(table.Actions) != 0 || !strings.Contains(table.Header, table.Key) {
			t.Fatalf("invalid declaration: %s", table.Key)
		}
		s, err := schema.Parse(models[i].Value, &sync.Map{}, schema.NamingStrategy{})
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, col := range table.Columns {
			if s.FieldsByDBName[col.Field] == nil || hidden(table.Key, col.Field) {
				t.Fatalf("invalid projection %s.%s", table.Key, col.Field)
			}
			seen[col.Field] = true
		}
		for _, f := range s.Fields {
			if f.DBName != "" && !hidden(table.Key, f.DBName) && !seen[f.DBName] {
				t.Fatalf("missing %s.%s", table.Key, f.DBName)
			}
		}
		if table.Key == "crawler_tasks" && !seen["p_id"] {
			t.Fatal("GORM PID column must be p_id")
		}
	}
	if _, err := oao.New(oao.Config{Tables: tables}); err != nil {
		t.Fatal(err)
	}
}

func TestFrameworkHTTPReadOnlyAuthAndBusinessIsolation(t *testing.T) {
	db, f := testDB(t)
	mux := testMux(t, db)
	for _, m := range database.AllFrameworkModels() {
		for _, path := range []string{"/api/oao/" + m.Name, "/api/framework-tables/" + m.Name} {
			if w := request(mux, "GET", path, false); w.Code != 401 {
				t.Fatal(w.Code, path)
			}
			if w := request(mux, "GET", path, true); w.Code != 200 {
				t.Fatal(w.Code, w.Body.String(), path)
			}
			for _, method := range []string{"POST", "PUT", "DELETE"} {
				if w := request(mux, method, path, true); w.Code < 400 {
					t.Fatal("write accepted", method, path)
				}
			}
		}
		if w := request(mux, "POST", "/api/oao/"+m.Name+"/action/delete", true); w.Code < 400 {
			t.Fatal("action accepted")
		}
	}
	for _, path := range []string{"/api/oao/crawler_business", "/api/framework-tables/crawler_business"} {
		if w := request(mux, "GET", path, true); w.Code != 404 {
			t.Fatal(w.Code, path)
		}
	}
	for _, q := range f.queries {
		if !strings.HasPrefix(q, "SELECT ") {
			t.Fatal("non-read SQL", q)
		}
	}
}

func TestFrameworkSQLProjectionPaginationAndCompositeOrder(t *testing.T) {
	db, f := testDB(t)
	mux := testMux(t, db)
	for _, name := range []string{"crawler_access_token", "crawler_config", "crawler_tasks", "crawler_operation_log", "crawler_rule_versions", "crawler_agent_steps"} {
		f.queries = nil
		params := url.Values{"page": {"2"}, "size": {"50"}, "sort": {"-status,evil"}, "filter[table]": {"task"}, "filter[token_hash]": {"forbidden"}}
		if name == "crawler_rule_versions" {
			params.Set("sort", "-rule_id")
		}
		w := request(mux, "GET", "/api/oao/"+name+"?"+params.Encode(), true)
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		q := strings.Join(f.queries, "\n")
		for _, bad := range []string{"token_hash", "cfg_val", "idempotency_key", "evil", "forbidden", "SELECT *"} {
			if strings.Contains(q, bad) {
				t.Fatalf("unsafe projection %s: %s", bad, q)
			}
		}
		if !strings.Contains(q, "count(*)") || !strings.Contains(q, "LIMIT ? OFFSET ?") {
			t.Fatal(q)
		}
		if !strings.Contains(q, "LEFT(") || !strings.Contains(q, "65537") {
			t.Fatal("unbounded text", q)
		}
		if name == "crawler_operation_log" && !strings.Contains(q, "`table` = ?") {
			t.Fatal(q)
		}
		if strings.Contains(f.queries[0], "ORDER BY") {
			t.Fatal("count must not inherit tie breakers", q)
		}
		if name == "crawler_rule_versions" && !strings.Contains(q, "ORDER BY `rule_id` DESC,`rule_id` ASC,`version` ASC") {
			t.Fatal("compound order", q)
		}
		if name == "crawler_agent_steps" && !strings.Contains(q, "ORDER BY `status` DESC,`run_id` ASC,`sequence` ASC") {
			t.Fatal("user sort/tiebreaker", q)
		}
	}
}

func TestFrameworkStatusMissingUnavailableAndRecovery(t *testing.T) {
	db, f := testDB(t)
	mux := testMux(t, db)
	for _, tc := range []struct {
		err   error
		state string
	}{
		{&mysqlDriver.MySQLError{Number: 1146, Message: "private schema secret"}, "missing"},
		{&mysqlDriver.MySQLError{Number: 1142, Message: "private password"}, "unavailable"},
		{errors.New("private connection secret"), "unavailable"}, {nil, "available"},
	} {
		f.err = tc.err
		w := request(mux, "GET", "/api/framework-tables/crawler_agent_runs", true)
		var status tableStatus
		if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		if status.State != tc.state || strings.Contains(w.Body.String(), "private") {
			t.Fatal(w.Body.String())
		}
		if status.State == "missing" && !strings.Contains(status.Message, "papa migrate") {
			t.Fatal(status)
		}
	}
}

func TestFrameworkResponseSanitizesNestedJSONAndBinaryIDs(t *testing.T) {
	db, f := testDB(t)
	f.columns = []string{"id", "snapshot_json", "usage_json"}
	f.row = []driver.Value{[]byte("run-1"), []byte(`{"url":"https://user:pw@example.test/page?key=secret#token","nested":{"api_key":"must-hide","headers":{"Cookie":"must-hide"}},"count":9007199254740993}`), []byte(`invalid secret`)}
	w := request(testMux(t, db), "GET", "/api/oao/crawler_agent_runs", true)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for _, bad := range []string{"must-hide", "user:pw", "?key=secret", "#token", "invalid secret"} {
		if strings.Contains(w.Body.String(), bad) {
			t.Fatal(w.Body.String())
		}
	}
	if !strings.Contains(w.Body.String(), `"id":"run-1"`) || !strings.Contains(w.Body.String(), "9007199254740993") {
		t.Fatal(w.Body.String())
	}
}

func TestFrameworkSanitizerBoundsAndPlainSecrets(t *testing.T) {
	for _, value := range []any{[]byte(strings.Repeat("a", maxInput+1)), strings.Repeat("界", maxText+1), `Bearer must-hide password=must-hide https://user:must-hide@example.test/?secret=must-hide`} {
		got := fmt.Sprint(sanitizeValue("message", value, 0))
		if strings.Contains(got, "must-hide") || len([]rune(got)) > maxText+100 {
			t.Fatal(got[:100])
		}
	}
	if got := sanitizeValue("token_hash", "must-hide", 0); got != redacted {
		t.Fatal(got)
	}
}

func TestFrameworkTypedJSONCannotBypassSanitizer(t *testing.T) {
	db, f := testDB(t)
	f.columns = []string{"id", "data"}
	f.row = []driver.Value{int64(1), []byte(`{"password":"must-hide","nested":{"cookie":"must-hide"}}`)}
	w := request(testMux(t, db), "GET", "/api/oao/crawler_task_trace", true)
	if w.Code != 200 || strings.Contains(w.Body.String(), "must-hide") {
		t.Fatal("typed JSON leaked or failed", w.Code)
	}
}

func TestFrameworkConcurrentReadsKeepQueriesIsolated(t *testing.T) {
	db, f := testDB(t)
	mux := testMux(t, db)
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := "crawler_tasks"
			if i%2 == 0 {
				key = "crawler_access_token"
			}
			w := request(mux, "GET", "/api/oao/"+key+"?sort=-id", true)
			if w.Code != 200 {
				t.Errorf("read status %d", w.Code)
			}
		}(i)
	}
	wg.Wait()
	for _, q := range f.queries {
		if strings.Contains(q, "crawler_access_token") && strings.Contains(q, "p_id") {
			t.Fatal("cross-table query contamination", q)
		}
	}
}

func TestSelectedFrameworkCatalogAndProbeExcludeOtherTables(t *testing.T) {
	tables, err := Tables(nil, "crawler_task_trace")
	if err != nil || len(tables) != 1 || tables[0].Key != "crawler_task_trace" {
		t.Fatalf("selection: %v %v", tables, err)
	}
	mux := http.NewServeMux()
	Register(mux, nil, authenticated, "crawler_task_trace")
	if w := request(mux, "GET", "/api/framework-tables/crawler_config", true); w.Code != 404 {
		t.Fatal("unselected probe exposed", w.Code)
	}
	if w := request(mux, "GET", "/api/framework-tables/crawler_task_trace", true); w.Code != 200 {
		t.Fatal("selected probe absent", w.Code)
	}
}
