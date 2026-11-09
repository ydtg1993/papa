// Package strategymigrate provides explicit migration inspection. Inspection
// never authorizes a cutover or table deletion and never changes source data.
package strategymigrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	mysql "github.com/go-sql-driver/mysql"
	"regexp"
)

type TableSpec struct {
	Name        string `json:"name"`
	Disposition string `json:"disposition"`
}
type TableInventory struct {
	TableSpec
	Exists bool   `json:"exists"`
	Rows   *int64 `json:"rows"`
}
type Report struct {
	Version        int              `json:"version"`
	ReadOnly       bool             `json:"read_only"`
	MigrationReady bool             `json:"migration_ready"`
	Notice         string           `json:"notice"`
	Tables         []TableInventory `json:"tables"`
}

// FrameworkTables names only framework-owned objects. Business manifests live
// in their own modules, not in this package.
func FrameworkTables() []TableSpec {
	return []TableSpec{
		{"crawler_rule_versions", "retire_after_verified_migration"},
		{"crawler_rule_samples", "retire_after_verified_migration"},
		{"crawler_rule_sample_sets", "retire_after_verified_migration"},
		{"crawler_rule_verifications", "retire_after_verified_migration"},
		{"crawler_rule_releases", "retire_after_verified_migration"},
		{"crawler_rule_sites", "retire_after_verified_migration"},
		{"crawler_rule_schedules", "adapt"},
		{"crawler_rule_reanalysis_requests", "retire_after_verified_migration"},
		{"crawler_rule_observations", "adapt"},
		{"crawler_rule_quality_reports", "adapt"},
		{"crawler_agent_runs", "retain"},
		{"crawler_agent_steps", "retain"},
		{"crawler_tasks", "adapt"},
		{"crawler_task_trace", "retain"},
		{"crawler_config", "adapt"},
		{"crawler_strategy_sessions", "target"},
		{"crawler_strategy_trials", "target"},
		{"crawler_strategy_versions", "target"},
		{"crawler_strategy_trial_rows", "target"},
		{"crawler_strategy_trial_files", "target"},
	}
}

var identifier = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func ValidateTables(extra []TableSpec) error {
	all := append(FrameworkTables(), extra...)
	if len(all) > 64 {
		return errors.New("strategy migration: at most 64 inventory tables")
	}
	seen := map[string]bool{}
	for _, s := range all {
		if !identifier.MatchString(s.Name) || seen[s.Name] {
			return errors.New("strategy migration: invalid or duplicate table name")
		}
		switch s.Disposition {
		case "retire_after_verified_migration", "retain", "adapt", "target":
		default:
			return errors.New("strategy migration: invalid table disposition")
		}
		seen[s.Name] = true
	}
	return nil
}

// Inspect counts existing tables within a read-only transaction. Counts are
// inventory only: they cannot prove row integrity, file availability, reference
// completeness, frozen writers, or a successful import. Missing is not zero.
func Inspect(ctx context.Context, db *sql.DB, extra []TableSpec) (Report, error) {
	r := Report{Version: 1, ReadOnly: true, Notice: "Inventory only; content, references, files and writer freeze are not verified. No migration or deletion is authorized.", Tables: []TableInventory{}}
	if e := ValidateTables(extra); e != nil {
		return r, e
	}
	if db == nil {
		return r, errors.New("strategy migration: database required")
	}
	tx, e := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if e != nil {
		return r, errors.New("strategy migration: cannot open read-only inventory transaction")
	}
	defer tx.Rollback()
	for _, s := range append(FrameworkTables(), extra...) {
		x := TableInventory{TableSpec: s}
		var count int64
		e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM `"+s.Name+"`").Scan(&count)
		if e != nil {
			// information_schema can hide tables from a restricted account.
			// Only the explicit missing-table error means absent, never denial.
			var me *mysql.MySQLError
			if !errors.As(e, &me) || me.Number != 1146 {
				return r, fmt.Errorf("strategy migration: cannot count table: %s", s.Name)
			}
		} else {
			x.Exists = true
			x.Rows = &count
		}
		r.Tables = append(r.Tables, x)
	}
	return r, nil
}
