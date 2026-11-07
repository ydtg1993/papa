package auth

import (
	"net/http/httptest"
	"os"
	"regexp"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Run only against a newly created, disposable database, never a business DB.
func TestAuthenticationMySQLEmptyCreateDisableDelete(t *testing.T) {
	dsn := os.Getenv("PAPA_AUTH_TEST_DSN")
	if dsn == "" {
		t.Skip("PAPA_AUTH_TEST_DSN not configured")
	}
	cfg, err := driver.ParseDSN(dsn)
	if err != nil || !regexp.MustCompile(`^papa_auth_test_[a-f0-9]{16}$`).MatchString(cfg.DBName) {
		t.Fatal("requires dedicated papa_auth_test_<16 hex> database")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal("cannot connect to isolated authentication database")
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if db.Migrator().HasTable(&models.AccessToken{}) {
		t.Fatal("authentication test requires a fresh database")
	}
	if err := db.AutoMigrate(&models.AccessToken{}); err != nil {
		t.Fatal(err)
	}
	counter := NewTokenCounter(db, time.Hour)
	counter.CountAll()
	counter.CountEnabled()
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	check := func(value string, want bool) {
		t.Helper()
		r := httptest.NewRequest("GET", "/api/monitor", nil)
		r.Header.Set("Authorization", "Bearer "+value)
		op, ok := counter.Verifier(nil)(r)
		if ok != want || (ok && op != "integration-operator") {
			t.Fatalf("request authentication: allowed=%v want=%v", ok, want)
		}
		if _, ok := counter.Confirmer(nil)(value); ok != want {
			t.Fatalf("confirmation: allowed=%v want=%v", ok, want)
		}
		if !counter.Required() {
			t.Fatal("login must remain required")
		}
	}
	check(token, false)
	row := models.AccessToken{Operator: "integration-operator", TokenHash: Hash(token), Enabled: true}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal("cannot create isolated token")
	}
	check(token, true) // No cache invalidation, equivalent to a separate CLI writer.
	check("wrong-token", false)
	check("", false)
	if err := db.Model(&row).Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	check(token, false)
	if err := db.Model(&row).Update("enabled", true).Error; err != nil {
		t.Fatal(err)
	}
	check(token, true)
	if err := db.Delete(&row).Error; err != nil {
		t.Fatal(err)
	}
	check(token, false) // An externally emptied table must never reopen the API.
}
