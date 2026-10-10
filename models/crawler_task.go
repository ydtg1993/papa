package models

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type TaskStatus int

const (
	TaskStatusPending    TaskStatus = iota // 待处理
	TaskStatusProcessing                   // 处理中
	TaskStatusSuccess                      // 成功
	TaskStatusFailed                       // 失败
)

type RepeatableStatus int

const (
	RepeatableNo RepeatableStatus = iota
	RepeatableYes
)

type CrawlerTask struct {
	ID    uint   `gorm:"primarykey;comment:任务ID"`
	PID   uint   `gorm:"index;type:int(11);default:0;comment:父级任务ID"`
	Stage string `gorm:"type:varchar(128);not null;index;uniqueIndex:idx_stage_url_hash,priority:2;comment:所属阶段(例如catalog/detail等)"`
	Site  string `gorm:"type:varchar(128);index;comment:所属站点(站点声明的 Key)；空=未归属"`
	// URL 任务 URL。**不建索引，也不限长度**：整串进唯一索引会撞 InnoDB 的键长上限
	//（utf8mb4 下 3072 字节 ≈ 768 字符），而带一串 query 的 URL 很容易更长。
	URL string `gorm:"type:text;not null;comment:任务URL"`
	// URLHash URL 的 sha256（64 位十六进制小写，定长）。唯一索引建在 (url_hash, stage) 上 ——
	// 与 URL 长度无关。由 BeforeCreate 自动填（任何创建路径都覆盖），老库由 `papa migrate` 回填。
	URLHash        string `gorm:"type:char(64);not null;uniqueIndex:idx_stage_url_hash,priority:1;comment:URL的sha256(长URL建不了整串唯一索引)"`
	IdempotencyKey string `gorm:"type:varchar(500);index;comment:自定义幂等键，空则回退 stage|url"`
	// Meta 业务键（如 series_id/episode_id），与 URL 解耦。**随行落库** ——
	// 恢复/轮询/错误队列/后台重投这几条「从行重建 Task」的路都按它还原任务身份，
	// 不落库的话任务重投一次身份就没了（handler 拿不到业务键，报错还指不到这里）。
	Meta       datatypes.JSON   `gorm:"type:json;comment:业务键(如 series_id/episode_id)，随行落库"`
	Title      string           `gorm:"type:text;comment:页面标题"`
	Content    datatypes.JSON   `gorm:"type:json;comment:页面提取内容"`
	Retry      int              `gorm:"default:0;comment:错误重试次数"`
	Status     TaskStatus       `gorm:"default:0;index:idx_repeat_due,priority:2;comment:0:待处理 1:处理中 2:成功 3:失败"`
	Repeatable RepeatableStatus `gorm:"type:tinyint(1);default:0;index:idx_repeat_due,priority:1;comment:支持重试 0:不能 1:可以"`
	Repeat     int              `gorm:"type:int(11);default:0;comment:轮询重试次数"`
	// RepeatInterval 这条任务自己的轮询周期（秒）；0 = 用本站 RepeatQueue 的 Interval。
	// 只在首次入库时由 `Task.toModel` 播种；之后改它走 `Engine.SetTaskRepeatInterval`（后台「设轮询周期」动作）。
	RepeatInterval int `gorm:"type:int(11);default:0;comment:轮询周期(秒)；0=用本站RepeatQueue的Interval"`
	// NextRepeatAt 下次到点时间 —— 轮询队列判断"该不该重投"的**唯一**依据；NULL = 未排期（老行/从未轮询过）→ 算到点。
	// 用指针而非 time.Time：零值时间会被 gorm 写成 0001-01-01，而 MySQL DATETIME 下限是 1000-01-01，
	// 严格模式下直接报 1292（比 0000-00-00 更早失败）。索引 idx_repeat_due 见 Repeatable/Status 上的同名 tag。
	NextRepeatAt *time.Time `gorm:"index:idx_repeat_due,priority:3;comment:下次轮询到点；NULL=未排期"`
	// LastRepeatAt 上次被轮询队列**投递**的时间（只给后台看，不参与任何判断）：
	// 周期被改过之后，它与 NextRepeatAt 不再互为简单加减，所以不是可以推导出来的副本。
	LastRepeatAt *time.Time `gorm:"comment:上次被轮询投递的时间；NULL=从未"`
	Reprocess    int        `gorm:"type:int(11);default:0;comment:失败任务被错误队列再处理的代数"`
	// Urgent 加急：该任务投到所属阶段的快车道，插到常规队列前面。
	// 它是「排队位置」的概念 —— worker 认领（置为处理中）时一并归零，跑过一次即完成使命。
	Urgent    bool      `gorm:"type:tinyint(1);default:0;comment:加急 0:否 1:是"`
	Error     string    `gorm:"type:text;comment:错误信息"`
	CreatedAt time.Time `gorm:"autoCreateTime;comment:创建时间"`
	UpdatedAt time.Time `gorm:"autoUpdateTime;comment:更新时间"`
}

func (t *CrawlerTask) BeforeCreate(tx *gorm.DB) error {
	if t.Content == nil || len(t.Content) == 0 {
		t.Content = datatypes.JSON("{}")
	}
	// URL 的哈希在这里统一补上：任何创建路径（含业务直接 Create 这个模型）都覆盖到，
	// 不会出现"唯一索引上是一串空串"这种撞车。
	if t.URLHash == "" && t.URL != "" {
		t.URLHash = UrlHash(t.URL)
	}
	return nil
}

// UrlHash URL 的 sha256 十六进制（小写，64 字符）—— 唯一索引用它，长 URL 建不了整串索引。
//
// 与 MySQL 的 `SHA2(url, 256)` 输出一致（同样是十六进制小写），所以迁移里的回填 SQL 和
// Go 侧算出来的是同一个值。
func UrlHash(url string) string {
	sum := sha256.Sum256([]byte(url))
	return hex.EncodeToString(sum[:])
}
