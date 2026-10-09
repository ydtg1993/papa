package tokenadmin

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ydtg1993/papa/v3/admin/auth"
)

// MemStore 内存实现：给 cmd/monitor-demo（不接 MySQL 的后台预览）和测试用。
//
// 它和 DBStore 一样按 sha256 存令牌，所以演示里「在后台新建一把 → 拿它登录」是真的走得通的
// （demo 的登录校验也查这个 store）。
type MemStore struct {
	mu   sync.Mutex
	seq  uint
	rows []memRow
}

type memRow struct {
	Token
	hash string
}

// NewMemStore 创建空的内存存储。
func NewMemStore() *MemStore { return &MemStore{} }

// Seed 塞一条**明文已知**的令牌，返回它的 ID —— 演示需要一个固定的登录令牌（每次启动都一样），
// 而 Create 生成的是随机的。
func (m *MemStore) Seed(token, operator, note string) uint {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	m.rows = append(m.rows, memRow{
		Token: Token{ID: m.seq, Operator: operator, Enabled: true, Note: note,
			CreatedAt: time.Now(), UpdatedAt: time.Now()},
		hash: auth.Hash(token),
	})
	return m.seq
}

// List 返回全部令牌（新的在前）。
func (m *MemStore) List() ([]Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Token, 0, len(m.rows))
	for _, r := range m.rows {
		out = append(out, r.Token)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

// Create 生成一把令牌并返回明文与它的 ID。
func (m *MemStore) Create(operator, note string) (string, uint, error) {
	operator = strings.TrimSpace(operator)
	if operator == "" {
		return "", 0, ErrOperatorRequired
	}
	token, err := auth.NewToken()
	if err != nil {
		return "", 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	m.rows = append(m.rows, memRow{
		Token: Token{ID: m.seq, Operator: operator, Enabled: true, Note: strings.TrimSpace(note),
			CreatedAt: time.Now(), UpdatedAt: time.Now()},
		hash: auth.Hash(token),
	})
	return token, m.seq, nil
}

// SetEnabled 启用 / 停用（条件与 DBStore 一致：状态没被改过才生效）。
func (m *MemStore) SetEnabled(id uint, enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.rows {
		if m.rows[i].ID != id {
			continue
		}
		if m.rows[i].Enabled == enabled {
			return ErrChanged
		}
		m.rows[i].Enabled = enabled
		m.rows[i].UpdatedAt = time.Now()
		return nil
	}
	return ErrNotFound
}

// Delete 删除一把令牌。
// Delete 删除一把令牌；与 DBStore 同一份约束：**不许把最后一条删掉**（见 deleteScope）。
func (m *MemStore) Delete(id uint) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.rows {
		if m.rows[i].ID == id {
			if len(m.rows) == 1 {
				return ErrLastToken
			}
			m.rows = append(m.rows[:i], m.rows[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}

// Verify 按令牌查人（启用中才算命中）—— 演示的登录校验走它，与 admin/auth.Verify 同语义。
// 生产走 admin/auth 的库表校验，不用这个方法。
func (m *MemStore) Verify(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	h := auth.Hash(token)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.hash == h && r.Enabled {
			return r.Operator, true
		}
	}
	return "", false
}

// 编译期确认 MemStore 满足接口。
var _ Store = (*MemStore)(nil)
