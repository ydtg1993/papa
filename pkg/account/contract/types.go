package contract

import (
	"context"
	"errors"
	"time"
)

var ErrPoolConflict = errors.New("account pool version conflict")

type Key struct {
	Site string `json:"site"`
	ID   string `json:"id"`
}

type PoolKey struct {
	Site string `json:"site"`
	Name string `json:"name"`
}

type Transport string

type PoolConfig struct {
	Key            PoolKey   `json:"key"`
	Members        []string  `json:"members"`
	Transport      Transport `json:"transport"`
	Disabled       bool      `json:"disabled"`
	VerifyAttempts int       `json:"verify_attempts"`
	// SwitchLimit bounds candidates checked in one scheduling round. Later rounds
	// continue after Cooldown, never spin or silently reset invalid accounts.
	SwitchLimit   int           `json:"switch_limit"`
	Cooldown      time.Duration `json:"cooldown"`
	StandbyTarget int           `json:"standby_target"`
	StandbyTTL    time.Duration `json:"standby_ttl"`
}

type SwitchRecord struct {
	From             string    `json:"from"`
	To               string    `json:"to"`
	Reason           string    `json:"reason"`
	TaskID           uint64    `json:"task_id,omitempty"`
	SelectionVersion uint64    `json:"selection_version"`
	At               time.Time `json:"at"`
}

type PoolState struct {
	PoolConfig
	Revision         uint64         `json:"revision"`
	CurrentID        string         `json:"current_id"`
	SelectionVersion uint64         `json:"selection_version"`
	History          []SwitchRecord `json:"history"`
	Deleted          bool           `json:"deleted,omitempty"`
}

type ManagementState struct {
	Key     Key    `json:"key"`
	Version uint64 `json:"version"`
	// LastControlVersion is the latest Version that changed policy or health.
	// Pure usage and event writes advance Version without invalidating OA edits.
	LastControlVersion  uint64           `json:"last_control_version"`
	Generation          uint64           `json:"generation"`
	Disabled            bool             `json:"disabled"`
	Quarantined         bool             `json:"quarantined"`
	CooldownUntil       time.Time        `json:"cooldown_until"`
	Reason              string           `json:"reason,omitempty"`
	PendingInvalidation bool             `json:"pending_invalidation"`
	TriggerTaskID       uint64           `json:"trigger_task_id,omitempty"`
	UpdatedAt           time.Time        `json:"updated_at"`
	Suspicious          bool             `json:"suspicious"`
	ProxyUnavailable    bool             `json:"proxy_unavailable"`
	VerificationMode    VerificationMode `json:"verification_mode,omitempty"`
	VerificationPending bool             `json:"verification_pending"`
	CredentialVersion   uint64           `json:"credential_version"`
	BrowserVersion      uint64           `json:"browser_version"`
	CheckTransport      Transport        `json:"check_transport,omitempty"`
	NextVerifyAt        time.Time        `json:"next_verify_at"`
	SuspectSince        time.Time        `json:"suspect_since"`
	InvalidChecks       int              `json:"invalid_checks"`
	UnknownChecks       int              `json:"unknown_checks"`
	ProxyChecks         int              `json:"proxy_checks"`
	LastVerifiedAt      time.Time        `json:"last_verified_at"`
	LastVerification    string           `json:"last_verification,omitempty"`
	Policy              Policy           `json:"policy"`
	Usage               Usage            `json:"usage"`
	Events              []HealthEvent    `json:"events,omitempty"`
}

type PoolStore interface {
	LoadPools(context.Context) ([]PoolState, error)
	LoadManagement(context.Context) ([]ManagementState, error)
	SavePoolCAS(context.Context, PoolState, uint64) error
	SaveManagementCAS(context.Context, ManagementState, uint64) error
}

type VerificationMode string

type Policy struct {
	Buffer               time.Duration `json:"buffer"`
	InvalidConfirmations int           `json:"invalid_confirmations"`
	CheckInterval        time.Duration `json:"check_interval"`
	VerifyTimeout        time.Duration `json:"verify_timeout"`
	UnknownBackoff       time.Duration `json:"unknown_backoff"`
	MaxBackoff           time.Duration `json:"max_backoff"`
	UnknownAlertAfter    time.Duration `json:"unknown_alert_after"`
	OperationInterval    time.Duration `json:"operation_interval"`
	DailyLimit           int64         `json:"daily_limit"`
	Timezone             string        `json:"timezone"`
	WorkStartMinute      int           `json:"work_start_minute"`
	WorkEndMinute        int           `json:"work_end_minute"`
	LoginInterval        time.Duration `json:"login_interval"`
	RecoveryInterval     time.Duration `json:"recovery_interval"`
	TrackUsage           bool          `json:"track_usage"`
}

type Usage struct {
	Unit             string    `json:"unit"`
	Day              string    `json:"day"`
	Operations       int64     `json:"operations"`
	LastOperation    time.Time `json:"last_operation"`
	LastLoginAttempt time.Time `json:"last_login_attempt"`
}

type HealthEvent struct {
	Sequence uint64    `json:"sequence"`
	Key      Key       `json:"key"`
	Pool     PoolKey   `json:"pool,omitempty"`
	Kind     string    `json:"kind"`
	Reason   string    `json:"reason"`
	TaskID   uint64    `json:"task_id,omitempty"`
	At       time.Time `json:"at"`
	Version  uint64    `json:"version"`
}
