package frameworkdata

import (
	"database/sql"
	"encoding/json"
	"os"
	"testing"

	"github.com/ydtg1993/papa/v3/internal/database"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// This opt-in test only SELECTs existing tables, in a read-only transaction.
// It never creates a database, migrates, changes config or prints row payloads.
func TestFrameworkExistingMySQLReadOnly(t *testing.T) {
	dsn := os.Getenv("PAPA_FRAMEWORK_DATA_MYSQL_DSN")
	if dsn == "" {
		t.Skip("PAPA_FRAMEWORK_DATA_MYSQL_DSN not set; existing-schema MySQL reads not verified")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal("could not open configured MySQL connection")
	}
	conn, err := db.DB()
	if err != nil {
		t.Fatal("could not get SQL connection")
	}
	defer conn.Close()
	tx := db.Begin(&sql.TxOptions{ReadOnly: true})
	if tx.Error != nil {
		t.Fatal("could not start read-only transaction")
	}
	defer tx.Rollback()
	mux := testMux(t, tx)
	available, missing := 0, 0
	for _, m := range database.AllFrameworkModels() {
		w := request(mux, "GET", "/api/framework-tables/"+m.Name, true)
		var status tableStatus
		if json.Unmarshal(w.Body.Bytes(), &status) != nil {
			t.Fatal("invalid status response", m.Name)
		}
		if status.State == "missing" {
			missing++
			continue
		}
		if status.State != "available" {
			t.Fatal("table unavailable", m.Name)
		}
		w = request(mux, "GET", "/api/oao/"+m.Name+"?size=1", true)
		if w.Code != 200 {
			t.Fatal("read-only query failed", m.Name)
		}
		available++
	}
	if available == 0 {
		t.Fatal("no existing framework tables verified")
	}
	t.Logf("existing framework tables: available=%d missing=%d; all operations read-only", available, missing)
}
