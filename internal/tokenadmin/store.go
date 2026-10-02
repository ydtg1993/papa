// Package tokenadmin 是后台「访问令牌」页的数据与接口：列表、新增、停用/启用、删除。
//
// 它取代了原先那个 oao 表格声明。原因是表格组件的动作只回 {"status":"ok"}、**不回数据**，
// 而「新增」必须把服务端生成的明文令牌交给操作人看一次（库里只存哈希，过后无从显示）。
// 于是这一页做成后台自带的模块：自己的接口（本包的 API）+ 自己的表格渲染
// （internal/server/static/mo.js 的令牌页），菜单在「设置」上方。
//
// 令牌只存 sha256：库或备份泄漏也拿不到可用的令牌；代价是明文只在创建时出现一次。
package tokenadmin

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ydtg1993/papa/v2/internal/auth"
	"github.com/ydtg1993/papa/v2/models"
	"gorm.io/gorm"
)

// TableKey 令牌在审计日志里的表名（与原来 oao 表格页的 key 一致，老日志不会断代）。
const TableKey = "access_token"

var (
	ErrNotFound         = errors.New("令牌不存在")
	ErrChanged          = errors.New("该令牌状态已变，请刷新后重试")
	ErrOperatorRequired = errors.New("操作人不能为空")
)

// Token 一行令牌 —— **不含哈希**，页面与接口都不该看到它。
type Token struct {
	ID        uint      `json:"id"`
	Operator  string    `json:"operator"`
	Enabled   bool      `json:"enabled"`
	Note      string    `json:"note"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Store 令牌存储。定义成接口是为了让接口层能在没有数据库时有实现可用
// （测试与 cmd/monitor-demo 用 MemStore）。
type Store interface {
	// List 按 ID 倒序返回全部令牌。
	List() ([]Token, error)
	// Create 生成一把新令牌并入库，返回**明文**（只此一次，服务端不保存）与它的 ID。
	Create(operator, note string) (token string, id uint, err error)
	// SetEnabled 置为启用/停用；条件更新（当前状态必须与目标相反），防重复点击。
	SetEnabled(id uint, enabled bool) error
	// Delete 删除一把令牌。
	Delete(id uint) error
}

// DBStore 直接操作 crawler_access_token 表。
type DBStore struct{ db *gorm.DB }

// NewStore 创建令牌存储。
func NewStore(db *gorm.DB) *DBStore { return &DBStore{db: db} }

// enabledScope 条件更新的 WHERE：状态必须还是"另一头"，否则说明有人先改过了。
// 抽成包级函数是为了让测试能对**同一份条件**做 ToSQL 断言，而不是复制一份。
func enabledScope(db *gorm.DB, id uint, to bool) *gorm.DB {
	return db.Model(&models.AccessToken{}).Where("id = ? AND enabled = ?", id, !to)
}

// deleteScope 删除按主键；抽出来的理由同上。
func deleteScope(db *gorm.DB, id uint) *gorm.DB {
	return db.Where("id = ?", id)
}

// tokenColumns 只取页面需要的列 —— 哈希不进内存、更不会顺着接口出去。
const tokenColumns = "id, operator, enabled, note, created_at, updated_at"

// List 返回全部令牌（新的在前）。
func (s *DBStore) List() ([]Token, error) {
	rows := make([]Token, 0, 8)
	err := s.db.Model(&models.AccessToken{}).Select(tokenColumns).Order("id DESC").Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	return rows, nil
}

// Create 生成并入库一把令牌，返回明文与它的 ID。
func (s *DBStore) Create(operator, note string) (string, uint, error) {
	operator = strings.TrimSpace(operator)
	if operator == "" {
		return "", 0, ErrOperatorRequired
	}
	token, err := auth.NewToken()
	if err != nil {
		return "", 0, fmt.Errorf("new token: %w", err)
	}
	rec := models.AccessToken{
		Operator:  operator,
		Note:      strings.TrimSpace(note),
		Enabled:   true,
		TokenHash: auth.Hash(token),
	}
	if err := s.db.Create(&rec).Error; err != nil {
		return "", 0, fmt.Errorf("create token for %q: %w", operator, err)
	}
	return token, rec.ID, nil
}

// SetEnabled 启用 / 停用一把令牌。
func (s *DBStore) SetEnabled(id uint, enabled bool) error {
	res := enabledScope(s.db, id, enabled).Update("enabled", enabled)
	if res.Error != nil {
		return fmt.Errorf("set token %d enabled=%v: %w", id, enabled, res.Error)
	}
	if res.RowsAffected == 0 {
		return s.whyRejected(id, ErrChanged)
	}
	return nil
}

// Delete 删除一把令牌（行还在就算成功；不存在返回 ErrNotFound）。
func (s *DBStore) Delete(id uint) error {
	res := deleteScope(s.db, id).Delete(&models.AccessToken{})
	if res.Error != nil {
		return fmt.Errorf("delete token %d: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// whyRejected 条件更新影响 0 行时，再查一次把原因说清楚。
func (s *DBStore) whyRejected(id uint, fallback error) error {
	var t models.AccessToken
	err := s.db.Select("id").First(&t, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("load token %d: %w", id, err)
	}
	return fallback
}

// 编译期确认 DBStore 满足接口。
var _ Store = (*DBStore)(nil)
