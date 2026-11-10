package configstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	mysql "github.com/go-sql-driver/mysql"
	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

const KeyRuntimePolicy = "runtime_policy"

var (
	ErrCASRequired = errors.New("运行策略必须通过版本化接口写入")
	ErrConflict    = errors.New("配置已被其他操作更新，请核对最新版本")
)

// LoadVersioned returns revision zero only for an absent policy. The database
// column is authoritative; the JSON document must not contain a second revision.
func (s *Store) LoadVersioned(ctx context.Context, key string) (json.RawMessage, uint64, error) {
	if key != KeyRuntimePolicy {
		return nil, 0, ErrCASRequired
	}
	var row models.Config
	err := s.db.WithContext(ctx).Select("cfg_val", "revision").Where("cfg_key = ?", key).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("读取运行策略（请确认已执行 migrate）: %w", err)
	}
	if row.Revision == 0 {
		return nil, 0, errors.New("运行策略版本为零，请修复配置，不能按首次创建覆盖")
	}
	if len(bytes.TrimSpace(row.CfgVal)) == 0 {
		return nil, 0, errors.New("运行策略内容为空，拒绝启用采集")
	}
	return json.RawMessage(row.CfgVal), row.Revision, nil
}

// SaveCAS inserts an absent key exclusively or updates exactly the expected
// revision. Resetting overrides is another write, never a delete/reinsert.
// Callers validate semantics before this persistence boundary and publish only
// after success. An ambiguous database error must be reconciled by reloading.
func (s *Store) SaveCAS(ctx context.Context, key string, val any, expected uint64, operator string) (uint64, error) {
	if key != KeyRuntimePolicy {
		return 0, ErrCASRequired
	}
	if expected == math.MaxUint64 {
		return 0, errors.New("运行策略版本已达上限")
	}
	raw, err := json.Marshal(val)
	if err != nil {
		return 0, err
	}
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(raw, &shape); err != nil || shape == nil {
		return 0, errors.New("运行策略必须是 JSON 对象")
	}
	if _, ok := shape["revision"]; ok {
		return 0, errors.New("revision 必须由独立列管理")
	}
	next := expected + 1
	if expected == 0 {
		row := models.Config{CfgKey: key, CfgVal: datatypes.JSON(raw), Revision: next, UpdatedBy: operator}
		err = s.db.WithContext(ctx).Create(&row).Error
		var mysqlErr *mysql.MySQLError
		if errors.Is(err, gorm.ErrDuplicatedKey) || (errors.As(err, &mysqlErr) && mysqlErr.Number == 1062) {
			return 0, ErrConflict
		}
		if err != nil {
			return 0, fmt.Errorf("创建运行策略: %w", err)
		}
		return next, nil
	}
	result := s.db.WithContext(ctx).Model(&models.Config{}).
		Where("cfg_key = ? AND revision = ?", key, expected).
		Updates(map[string]any{"cfg_val": datatypes.JSON(raw), "revision": next, "updated_by": operator, "updated_at": time.Now()})
	if result.Error != nil {
		return 0, fmt.Errorf("更新运行策略: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return 0, ErrConflict
	}
	return next, nil
}
