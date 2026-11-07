package frameworkdata

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/ydtg1993/papa/v3/internal/database"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const queryTimeout = 10 * time.Second

type tableStatus struct {
	State   string `json:"state"`
	Message string `json:"message"`
}

func statusFor(err error) tableStatus {
	if err == nil {
		return tableStatus{"available", "只读查询"}
	}
	var mysqlErr *mysqlDriver.MySQLError
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1146 {
		return tableStatus{"missing", "此框架表尚未迁移，当前不可查询。请确认对应功能配置，再由运维显式执行 papa migrate；后台不会自动建表。"}
	}
	return tableStatus{"unavailable", "暂时无法查询此框架表，请检查数据库连接、权限及表结构后重试。"}
}

// Register mounts a read-only, authenticated availability probe. No schema or
// table names received from the client are passed through to SQL unchecked.
func Register(mux *http.ServeMux, db *gorm.DB, auth func(http.Handler) http.Handler, names ...string) {
	known := map[string]bool{}
	for _, m := range database.AllFrameworkModels() {
		if len(names) == 0 {
			known[m.Name] = true
		}
		for _, name := range names {
			if name == m.Name {
				known[m.Name] = true
			}
		}
	}
	mux.Handle("/api/framework-tables/{table}", auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		key := r.PathValue("table")
		if !known[key] {
			http.NotFound(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
		defer cancel()
		var err error
		if db == nil {
			err = errors.New("database unavailable")
		} else {
			// LIMIT 1, no COUNT and no sensitive payload. Unlike HasTable, a
			// connection or permission error cannot masquerade as a missing table.
			rows, queryErr := db.WithContext(ctx).Session(&gorm.Session{Logger: logger.Discard}).Table(key).Select("1").Limit(1).Rows()
			err = queryErr
			if rows != nil {
				if closeErr := rows.Close(); err == nil {
					err = closeErr
				}
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(statusFor(err))
	})))
}
