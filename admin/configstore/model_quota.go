package configstore

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/crawlagent/model"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const quotaPrefix = "agent_model_usage:"

type ModelDailyUsage struct {
	Day          string `json:"day"`
	Used         int64  `json:"used_tokens"`
	Reserved     int64  `json:"reserved_tokens"`
	UnknownCalls int64  `json:"unknown_calls"`
	Limit        int64  `json:"limit"`
	Timezone     string `json:"timezone"`
}
type dailyReservation struct {
	day    string
	amount int64
}

func databaseDay(ctx context.Context, db *gorm.DB) (string, error) {
	var day string
	err := db.WithContext(ctx).Raw("SELECT DATE_FORMAT(DATE_ADD(UTC_TIMESTAMP(), INTERVAL 8 HOUR), '%Y-%m-%d')").Scan(&day).Error
	if _, e := time.Parse("2006-01-02", day); err != nil || e != nil {
		return "", model.ErrDailyAccounting
	}
	return day, nil
}
func decodeDaily(raw []byte, day string) (ModelDailyUsage, error) {
	var d ModelDailyUsage
	if json.Unmarshal(raw, &d) != nil || d.Day != day || d.Used < 0 || d.Reserved < 0 || d.UnknownCalls < 0 || d.Reserved > math.MaxInt64-d.Used {
		return d, model.ErrDailyAccounting
	}
	return d, nil
}
func readDaily(tx *gorm.DB, day string, lock bool) (models.Config, ModelDailyUsage, error) {
	var row models.Config
	if lock {
		tx = tx.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	err := tx.Where("cfg_key = ?", quotaPrefix+day).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) && !lock {
		return row, ModelDailyUsage{Day: day}, nil
	}
	if err != nil {
		return row, ModelDailyUsage{}, model.ErrDailyAccounting
	}
	d, err := decodeDaily(row.CfgVal, day)
	return row, d, err
}
func writeDaily(tx *gorm.DB, row models.Config, d ModelDailyUsage) error {
	raw, err := json.Marshal(d)
	if err != nil {
		return model.ErrDailyAccounting
	}
	if tx.Model(&row).Update("cfg_val", datatypes.JSON(raw)).Error != nil {
		return model.ErrDailyAccounting
	}
	return nil
}

func (s *Store) LoadModelDailyUsage(ctx context.Context) (ModelDailyUsage, error) {
	var result ModelDailyUsage
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Same lock order as reservation, including limit edits in settings.
		var settings models.Config
		e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("cfg_key = ?", KeyAgentModel).Take(&settings).Error
		m := DefaultModelSettings()
		if e == nil {
			m, e = decodeModelSettings(settings.CfgVal)
		}
		if e != nil && !errors.Is(e, gorm.ErrRecordNotFound) {
			return model.ErrDailyAccounting
		}
		day, e := databaseDay(ctx, tx)
		if e != nil {
			return e
		}
		_, result, e = readDaily(tx, day, false)
		result.Limit = m.DailyTokenLimit
		result.Timezone = "Asia/Shanghai"
		return e
	})
	if err != nil {
		return ModelDailyUsage{}, model.ErrDailyAccounting
	}
	return result, nil
}
func (s *Store) reserveModelTokens(ctx context.Context, amount int64) (dailyReservation, error) {
	ticket := dailyReservation{amount: amount}
	if amount < 1 {
		return ticket, model.ErrDailyAccounting
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var settings models.Config
		if tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("cfg_key = ?", KeyAgentModel).Take(&settings).Error != nil {
			return model.ErrDailyAccounting
		}
		m, e := decodeModelSettings(settings.CfgVal)
		if e != nil {
			return model.ErrDailyAccounting
		}
		if !m.Enabled {
			return model.ErrConfig
		}
		day, e := databaseDay(ctx, tx)
		if e != nil {
			return e
		}
		ticket.day = day
		raw, _ := json.Marshal(ModelDailyUsage{Day: day})
		if tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&models.Config{CfgKey: quotaPrefix + day, CfgVal: datatypes.JSON(raw)}).Error != nil {
			return model.ErrDailyAccounting
		}
		row, d, e := readDaily(tx, day, true)
		if e != nil {
			return e
		}
		if d.Used+d.Reserved > m.DailyTokenLimit || amount > m.DailyTokenLimit-d.Used-d.Reserved {
			return model.ErrDailyLimit
		}
		d.Reserved += amount
		return writeDaily(tx, row, d)
	})
	if err != nil && !errors.Is(err, model.ErrDailyLimit) && !errors.Is(err, model.ErrConfig) {
		err = model.ErrDailyAccounting
	}
	return ticket, err
}
func settleDaily(d ModelDailyUsage, ticket dailyReservation, actual int64, known bool) (ModelDailyUsage, error) {
	if d.Day != ticket.day || ticket.amount < 1 || d.Reserved < ticket.amount {
		return d, model.ErrDailyAccounting
	}
	if !known {
		if d.UnknownCalls == math.MaxInt64 {
			return d, model.ErrDailyAccounting
		}
		d.UnknownCalls++
		return d, nil
	}
	if actual < 0 || actual > math.MaxInt64-d.Used-(d.Reserved-ticket.amount) {
		return d, model.ErrDailyAccounting
	}
	d.Reserved -= ticket.amount
	d.Used += actual
	return d, nil
}
func (s *Store) settleModelTokens(ctx context.Context, ticket dailyReservation, actual int64, known bool) error {
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row, d, e := readDaily(tx, ticket.day, true)
		if e != nil {
			return e
		}
		d, e = settleDaily(d, ticket, actual, known)
		if e != nil {
			return e
		}
		return writeDaily(tx, row, d)
	})
	if err != nil {
		return model.ErrDailyAccounting
	}
	return nil
}

type dailyProvider struct {
	store    *Store
	provider model.Provider
}

func (p dailyProvider) String() string   { return "model.DailyQuota{redacted}" }
func (p dailyProvider) GoString() string { return p.String() }

// WithDailyModelQuota wraps an explicitly supplied host provider. All calls using
// this application's settings share the same day counter, independent of model/key.
func (s *Store) WithDailyModelQuota(provider model.Provider) model.Provider {
	return dailyProvider{s, provider}
}

func (p dailyProvider) Complete(ctx context.Context, messages []model.Message, maxOutput int) (model.Reply, error) {
	if ctx == nil || p.provider == nil || len(messages) == 0 {
		return model.Reply{}, model.ErrInput
	}
	if ctx.Err() != nil {
		return model.Reply{}, ctx.Err()
	}
	if maxOutput < 1 || maxOutput > model.HardMaxOutputTokens {
		return model.Reply{}, model.ErrOutputLimit
	}
	data, e := json.Marshal(messages)
	if e != nil || len(data) > int(model.HardMaxInputBytes) {
		return model.Reply{}, model.ErrInputLimit
	}
	// Byte-based conservative reservation includes framing allowance; actual
	// service usage settles it. This is not a provider-specific tokenizer.
	amount := int64(len(data)) + 4096 + int64(maxOutput)
	ticket, e := p.store.reserveModelTokens(ctx, amount)
	if e != nil {
		return model.Reply{}, e
	}
	reply, callErr := p.provider.Complete(ctx, messages, maxOutput)
	actual, known := int64(0), false
	if reply.Usage.Known && reply.Usage.InputTokens >= 0 && reply.Usage.OutputTokens >= 0 && reply.Usage.InputTokens <= math.MaxInt64-reply.Usage.OutputTokens {
		actual = reply.Usage.InputTokens + reply.Usage.OutputTokens
		known = true
	} else if errors.Is(callErr, model.ErrUnauthorized) || errors.Is(callErr, model.ErrRateLimit) || errors.Is(callErr, model.ErrInput) || errors.Is(callErr, model.ErrInputLimit) || errors.Is(callErr, model.ErrOutputLimit) {
		known = true
	}
	// Cancellation cannot silently erase a reservation; settlement has its own
	// short deadline. Unknown commit outcomes are never retried automatically.
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := p.store.settleModelTokens(finish, ticket, actual, known); err != nil {
		return model.Reply{}, err
	}
	return reply, callErr
}
