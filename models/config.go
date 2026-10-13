package models

import (
	"time"

	"gorm.io/datatypes"
)

// Config 框架的**运行期配置**表：一行一个键，值统一是 JSON。
//
// 为什么单独一张表、为什么值是 JSON：这些配置要能**运行期改、改完立刻生效**（后台设置页），
// 而 config.yaml 是"改了要重启"的心智模型；值用 JSON 则"以后加一项配置"不必改表结构 ——
// 每一项的默认值 / 解析 / 校验 / 是否热生效都写在 Go 侧（见 admin/configstore）。
// 第一项是来源 IP 白名单（cfg_key = ip_whitelist，值是 IP 或 CIDR 的字符串数组）。
//
// **列名显式钉住**，不用 `key` / `val`：`KEY` 是 MySQL 保留字，这个仓库为它吃过一次
// Error 1064（见 docs/issues/changes.md 里那条"两条裸 SQL 的列名撞上保留字"）；
// `cfg_` 前缀顺带把"这是配置表"写进列名里。
type Config struct {
	ID     uint   `gorm:"primarykey;comment:配置ID"`
	CfgKey string `gorm:"column:cfg_key;type:varchar(128);not null;uniqueIndex:idx_cfg_key;comment:配置键（唯一）"`
	// CfgVal 值（JSON）：按 cfg_key 各自的约定解释 —— 表结构不随配置项变化。
	// MySQL 的 json 类型由 gorm 的 datatypes.JSON 承载（任务表的 meta 列用的是同一套）。
	CfgVal datatypes.JSON `gorm:"column:cfg_val;type:json;comment:配置值（JSON，按 cfg_key 解释）"`
	// UpdatedBy 最后一次改这一行的人（后台令牌对应的 operator；直接用 SQL 改则为空）。
	// 审计的另一半在 crawler_operation_log，而那个开关可以关 —— 所以"谁改的"记在行上。
	UpdatedBy string `gorm:"column:updated_by;type:varchar(128);comment:最后修改人（后台令牌的 operator）"`

	CreatedAt time.Time `gorm:"autoCreateTime"`
	UpdatedAt time.Time `gorm:"autoUpdateTime"`
}

// TableName 指定表名，与其它框架模型保持 crawler_ 前缀。
func (Config) TableName() string { return "crawler_config" }
