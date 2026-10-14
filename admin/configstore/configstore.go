// Package configstore 是 `crawler_config` 表的读写口：一行一个键，值统一 JSON。
//
// 表结构不随配置项变化，所以"这一项是什么类型、什么值算合法、没配时用什么"只能写在 Go 侧 ——
// 那就是下面这张**键注册表**（Spec）。没有它，`cfg_val` 就是一个谁都能塞任意 JSON 的洞：
// 键名打错（`ip_whitlist`）会静默多出一行没人读的配置，值写错形状要等到读的时候才发现。
//
// 它**只管形状与键名，不管业务语义**：`ip_whitelist` 的元素是不是合法 CIDR 由写它的那一层
// 判（后台是 admin/server 的严格校验）—— 读路径的语义是"坏条目丢掉、丢完为空就不限制"，
// 这里再拦一道会把"手改 SQL 写坏"从"放宽限制"变成"读不出来"。
//
// 读路径是"启动读一次 + 低频重载一次"，**不在请求路径上查库**：后台的每请求判定用的
// 是宿主缓存好的那份（见 admin/server 的 SetWhitelist）。
package configstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/internal/database"
	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// KeyIPWhitelist 来源 IP/CIDR 白名单：值是字符串数组，每条一个 IP 或 CIDR（裸 IP 按 /32、IPv6 按 /128）。
//
// 生效语义（宿主按这个解释）：**行不存在 → 不限制 · 空数组 → 不限制 · 值解析不出来 → 不限制** ·
// 数组里有效的那几条 → 只允许这些来源。写入口是后台设置页（`POST /api/settings/whitelist`）。
const KeyIPWhitelist = "ip_whitelist"

// Spec 一项运行期配置的规格。加一项配置 = 往下面那张表里加一条 + 想清楚它的默认值与形状，
// 表结构不用动、也不用迁移。
type Spec struct {
	// Key 键名（与 registry 的 map 键一致；有小用例钉这条，防复制粘贴时改漏一边）。
	Key string
	// Doc 一句话说明：报错文案与文档都从这里取，省得两处各写一遍。
	Doc string
	// Default 这一行**不存在**时这项配置取什么值（JSON 形态）；nil = 没有默认值。
	// 注意它与"显式写了空值"是两回事，需要分清的调用方用 Load（它带 found）。
	Default any
	// Validate 校验一份待写入的值（nil = 不校验）。**只管形状**，不管业务语义（见包注释）。
	Validate func(v any) error
}

// registry 已知配置项。**只有这里的键能读能写** —— 键名打错时报错会把它列出来。
var registry = map[string]Spec{
	KeyIPWhitelist: {
		Key:      KeyIPWhitelist,
		Doc:      "来源 IP/CIDR 白名单（字符串数组）；空数组 = 不限制来源",
		Default:  []string{},
		Validate: validateStringSlice,
	},
	KeySites: {
		Key: KeySites,
		Doc: "站点快照（Key / BaseURL / 阶段数 + 运行期暂停状态）；**引擎按声明重建**、" +
			"暂停/恢复时整块回写，不是给人手改的配置",
		Default:  []core.SiteStat{},
		Validate: validateSiteStats,
	},
}

// validateSiteStats 形状校验：站点数组（`[]core.SiteStat`，或 JSON 解出来的 []any 里全是对象）。
//
// 与 validateStringSlice 同一条分工：**只管形状**，不管"哪些站点该在里面"—— 那是引擎与声明的事。
func validateSiteStats(v any) error {
	switch t := v.(type) {
	case []core.SiteStat:
		return nil
	case []any:
		for i, item := range t {
			if _, ok := item.(map[string]any); !ok {
				return fmt.Errorf("第 %d 项不是对象（%T）", i+1, item)
			}
		}
		return nil
	default:
		return fmt.Errorf("要的是站点数组，实得 %T", v)
	}
}

// validateStringSlice 形状校验：字符串数组（[]string，或 JSON 解出来的 []any 里全是 string）。
func validateStringSlice(v any) error {
	switch t := v.(type) {
	case []string:
		return nil
	case []any:
		for i, item := range t {
			if _, ok := item.(string); !ok {
				return fmt.Errorf("第 %d 项不是字符串（是 %T）", i+1, item)
			}
		}
		return nil
	default:
		return fmt.Errorf("应当是字符串数组，实得 %T", v)
	}
}

// SpecOf 取一项配置的规格；未登记的键返回 error（错误里列出已知的，键名打错时能自己看出来）。
func SpecOf(key string) (Spec, error) {
	sp, ok := registry[key]
	if !ok {
		return Spec{}, fmt.Errorf("未登记的配置键 %q（已知的：%s）—— 加一项配置要同时在 configstore 的注册表里登记", key, strings.Join(Keys(), " · "))
	}
	return sp, nil
}

// Keys 已登记的键（排序，报错与文档用）。
func Keys() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Store 一张表的读写口。
//
// 除了直连库的 Load / Save，它还有一层**内存缓存**（cache.go）：读优先走内存、写时先落库再更新内存。
type Store struct {
	db *gorm.DB

	// mu 保护 cache。**写锁要覆盖"读-改-写"的整段**（见 UpdateSites）——
	// 值是一整块 JSON，RMW 分成两次加锁就会互相盖。
	// 读锁只服务"命中缓存"这条快路径（监控页每 3 秒一刷）：miss 与所有写都走写锁。
	mu    sync.RWMutex
	cache map[string]cached
}

// New 造一个读写口。不连库、不检查表 —— 表的存在性由 EnsureTable 单独把关（启动期那一步）。
func New(db *gorm.DB) *Store { return &Store{db: db} }

// EnsureTable 启动期自检：这张表不存在就直接报错，让调用方 fail fast。
//
// 为什么值得单独一步：迁移是**手工**的一步（`papa migrate` / `make migrate`），而"读不到配置"
// 的语义是"不限制来源" —— 少了这张表，白名单会**静默失效**，一个字都不报。宁可起不来。
func (s *Store) EnsureTable() error {
	table := models.Config{}.TableName()
	var n int64
	err := s.db.Raw(
		"SELECT COUNT(*) AS n FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?",
		table).Scan(&n).Error
	if err != nil {
		return fmt.Errorf("检查 %s 表是否存在失败: %w", table, err)
	}
	if n == 0 {
		return fmt.Errorf("%s 表不存在 —— 后台的运行期配置（来源 IP 白名单等）存在这张表里。"+
			"先跑一次 `papa migrate`（业务项目里也可以 `make migrate`）再启动", table)
	}
	return nil
}

// Load 读一个键。found=false 表示**这一行不存在**（对白名单来说 = 运营没设过 = 不限制），
// 与"读了但读出错"（err != nil）分开报 —— 后者是配置系统不可用，前者是正常的"没配"。
func (s *Store) Load(key string) (val []byte, found bool, err error) {
	if _, err := SpecOf(key); err != nil {
		return nil, false, err
	}
	var row models.Config
	err = s.db.Model(&models.Config{}).Where("cfg_key = ?", key).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("读配置 %s: %w", key, err)
	}
	return []byte(row.CfgVal), true, nil
}

// LoadStrings 按 `ip_whitelist` 这一项的规格读成字符串数组 —— **这一项生效的值**：
// 行不存在时给规格里的 Default（空数组 = 不限制），值不是字符串数组则报错。
//
// 调用方（internal/app）拿到 error 之后按"不限制来源 + 记 Error"处理：那是这一项的**业务语义**，
// 不是这个包该决定的事（包注释里写了理由）。
func (s *Store) LoadStrings(key string) ([]string, error) {
	sp, err := SpecOf(key)
	if err != nil {
		return nil, err
	}
	raw, found, err := s.Load(key)
	if err != nil {
		return nil, err
	}
	if !found {
		if def, ok := sp.Default.([]string); ok {
			return def, nil
		}
		return nil, nil
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("配置 %s 的值不是字符串数组: %w", key, err)
	}
	return out, nil
}

// Save 写一个键（不存在则插入），并把"谁改的"记在行上。
//
// 两道前置：键必须在注册表里、值的形状必须过这一项的 Validate —— 写坏一项运行期配置
// 的代价是"限制悄悄放宽"这类安静的事故，而写入点就这么几个，拦在这里最便宜。
func (s *Store) Save(key string, val any, operator string) error {
	sp, err := SpecOf(key)
	if err != nil {
		return err
	}
	if sp.Validate != nil {
		if err := sp.Validate(val); err != nil {
			return fmt.Errorf("配置 %s（%s）的值不合法：%w", key, sp.Doc, err)
		}
	}
	raw, err := json.Marshal(val)
	if err != nil {
		return fmt.Errorf("序列化配置 %s: %w", key, err)
	}
	rec := &models.Config{CfgKey: key, CfgVal: datatypes.JSON(raw), UpdatedBy: operator}
	// updated_at 必须显式进更新列：gorm 的 autoUpdateTime 只在 Update/Updates 那条路上生效，
	// 而这里走的是手写的 ON DUPLICATE KEY UPDATE（`updated_at = VALUES(updated_at)`）——
	// 不带它就永远停在首次插入的时刻，"这行什么时候改的"直接失真。
	if err := database.Upsert(s.db, rec, []string{"cfg_key"}, []string{"cfg_val", "updated_by", "updated_at"}); err != nil {
		return fmt.Errorf("写配置 %s: %w", key, err)
	}
	return nil
}
