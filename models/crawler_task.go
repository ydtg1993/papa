package models

import (
	"github.com/ydtg1993/papa/v4/core"
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
	LastStartedAt  *time.Time `gorm:"type:datetime(3);serializer:papa_utc;comment:最近一次Handler开始记录时间UTC"`
	LastFinishedAt *time.Time `gorm:"type:datetime(3);serializer:papa_utc;comment:最近一次Handler结束记录时间UTC；不代表终态已落库"`
	DelayMS        int64      `gorm:"not null;default:0;comment:首次投递延迟毫秒；不在重试时重复施加"`
	NotBefore      *time.Time `gorm:"type:datetime(3);serializer:papa_utc;index:idx_task_due,priority:2;comment:最早执行时间UTC；NULL为无约束"`
	FlowID         string     `gorm:"type:varchar(64);not null;default:'';index;comment:调度运行ID"`
	FlowLane       string     `gorm:"type:varchar(64);not null;default:'';comment:调度范围"`
	FlowBranch     uint       `gorm:"not null;default:0;comment:分支根任务"`
	FlowPath       string     `gorm:"type:varchar(700);not null;default:'';comment:稳定前序路径"`
	ID             uint       `gorm:"primarykey;comment:任务ID"`
	PID            uint       `gorm:"index;type:int(11);default:0;comment:父级任务ID"`
	// Stage 阶段名（fetcher 的 GetStage() 原值，**不带站点前缀**）。只在本站内唯一 ——
	// 两个站可以各有自己的 catalog，所以它与 Site 一起才构成"哪个阶段"，唯一索引三者都带。
	Stage string `gorm:"type:varchar(128);not null;index;uniqueIndex:idx_task_identity,priority:2;comment:所属阶段(例如catalog/detail等；只在本站内唯一)"`
	Site  string `gorm:"type:varchar(128);index;uniqueIndex:idx_task_identity,priority:1;comment:所属站点(站点声明的 Key，必填；没有未归属那一档)"`
	// AccountPool is task ownership, not part of deduplication. Retries/recovery
	// obtain this pool's current identity instead of binding a permanent account.
	AccountPool                 string     `gorm:"type:varchar(256);not null;default:'';index;comment:所属站点账号池；空表示未使用账号池"`
	FailureKind                 string     `gorm:"type:varchar(64);not null;default:'';comment:最近终态失败分类"`
	CompensationMode            string     `gorm:"type:varchar(32);not null;default:'';index:idx_compensation_due,priority:1;comment:空为默认自动补偿；retry_after/account_ready/manual/permanent"`
	NextCompensationAt          *time.Time `gorm:"index:idx_compensation_due,priority:2;comment:最早补偿时间；NULL为无时间约束"`
	CompensationReason          string     `gorm:"type:varchar(512);not null;default:'';comment:脱敏补偿等待原因"`
	CompensationVersion         uint64     `gorm:"not null;default:0;comment:补偿状态乐观锁版本"`
	LastAccountID               string     `gorm:"type:varchar(256);not null;default:'';index;comment:最近认领使用账号，不包含凭据"`
	LastAccountSelectionVersion uint64     `gorm:"not null;default:0;comment:最近认领的账号池选择版本"`
	// AccountAttempts retains the latest eight claimed rounds, independently of
	// optional trace retention. Each item includes account ID, versions and time;
	// older identity associations are deliberately outside this query coverage.
	AccountAttempts datatypes.JSON `gorm:"type:json;comment:最近8次认领的有界账号关联；非完整历史"`
	// URL 任务 URL。**不建索引，也不限长度**：整串进唯一索引会撞 InnoDB 的键长上限
	//（utf8mb4 下 3072 字节 ≈ 768 字符），而带一串 query 的 URL 很容易更长。
	URL string `gorm:"type:text;not null;comment:任务URL"`
	// IdentityType/Value are immutable task identity, independent of execution URL.
	IdentityType  string `gorm:"type:varchar(8);not null;default:'';comment:任务身份类型id或url"`
	IdentityValue string `gorm:"type:text;comment:稳定资源ID或规范URL"`
	IdentityHash  string `gorm:"type:char(64);not null;uniqueIndex:idx_task_identity,priority:3;comment:版本化站点阶段身份SHA256"`

	// Meta 业务键（如 series_id/episode_id），与 URL 解耦。**随行落库** ——
	// 恢复/轮询/错误队列/后台重投这几条「从行重建 Task」的路都按它还原任务身份，
	// 不落库的话任务重投一次身份就没了（handler 拿不到业务键，报错还指不到这里）。
	Meta  datatypes.JSON `gorm:"type:json;comment:业务键(如 series_id/episode_id)，随行落库"`
	Retry int            `gorm:"default:0;comment:错误重试次数"`
	// Status 挂了**两个**索引：
	//   - idx_repeat_due 的第二列（轮询判据的复合索引，leading 是 repeatable）；
	//   - idx_task_status 单列：恢复队列 `status IN (0,1)`、去重预热 `status IN (...) OR repeatable=1`
	//     都只按 status 过滤，复合索引 leading 不是 status 时用不上，数据量大后会退化成全表扫描。
	// gorm 允许同一字段写多个 `index:` tag（schema.ParseIndexes 会解析出两条；别被 TagSettings
	// map 只留最后一个值误导 —— parseFieldIndexes 读的是原始 tag）。
	Status     TaskStatus       `gorm:"default:0;index:idx_repeat_due,priority:2;index:idx_task_status;index:idx_task_due,priority:1;comment:0:待处理 1:处理中 2:成功 3:失败"`
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
	resourceID := ""
	if t.IdentityType == "id" {
		resourceID = t.IdentityValue
	}
	identity, err := core.NormalizeTaskIdentity(t.Site, t.Stage, resourceID, t.URL)
	if err != nil {
		return err
	}
	t.IdentityType, t.IdentityValue, t.IdentityHash = identity.Type, identity.Value, identity.Hash

	return nil
}
