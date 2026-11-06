package rulestore

import (
	"testing"

	"gorm.io/gorm"
)

// Publication must own the outermost transaction: releasing a savepoint cannot
// prove that the operation has durably committed to the database.
func TestMySQLRepositoryRejectsCallerOwnedTransactions(t *testing.T) {
	repo, db, _ := mysqlRepository(t)
	for _, prepared := range []bool{false, true} {
		name := "plain"
		if prepared {
			name = "prepared"
		}
		t.Run(name, func(t *testing.T) {
			session := db.Session(&gorm.Session{PrepareStmt: prepared})
			if _, err := New(session, repo.blobs); err != nil {
				t.Fatalf("ordinary connection must remain supported: %v", err)
			}
			tx := session.Begin()
			if tx.Error != nil {
				t.Fatal(tx.Error)
			}
			defer tx.Rollback()
			if _, err := New(tx, repo.blobs); err == nil {
				t.Fatal("repository accepted caller-owned transaction: a publication could report success before outer commit")
			}
		})
	}
}
