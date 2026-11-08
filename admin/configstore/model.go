package configstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/crawlagent"
	"github.com/ydtg1993/papa/v4/pkg/crawlagent/model"
	"github.com/ydtg1993/papa/v4/pkg/egress"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const KeyAgentModel = "agent_model"

var ErrModelConflict = errors.New("model settings changed; reload before saving")
var ErrModelSettings = errors.New("invalid model settings")

// ModelSettings exposes no credential in JSON or formatting. Hosts freeze the
// provider and per-run budgets; daily admission uses current enabled/limit values.
type ModelSettings struct {
	Revision              string `json:"revision"`
	Enabled               bool   `json:"enabled"`
	BaseURL               string `json:"base_url"`
	Model                 string `json:"model"`
	DailyTokenLimit       int64  `json:"daily_token_limit"`
	KeyConfigured         bool   `json:"key_configured"`
	apiKey                string
	secretCipher          string
	APIKeyEnv             string                 `json:"api_key_env"`
	RequestSeconds        int                    `json:"request_seconds"`
	RunSeconds            int                    `json:"run_seconds"`
	MaxModelCalls         int                    `json:"max_model_calls"`
	MaxCandidatesPerStage int                    `json:"max_candidates_per_stage"`
	MaxOutputTokens       int                    `json:"max_output_tokens"`
	MaxInputBytes         int                    `json:"max_input_bytes"`
	MaxTotalInputBytes    int                    `json:"max_total_input_bytes"`
	MaxResponseBytes      int                    `json:"max_response_bytes"`
	OutputTokenField      model.OutputTokenField `json:"output_token_field"`
	JSONOutput            bool                   `json:"json_output"`
	ThinkingMode          string                 `json:"thinking_mode"`
	Currency              string                 `json:"currency"`
	PriceVersion          string                 `json:"price_version"`
	InputPriceMicros      int64                  `json:"input_price_micros"`
	OutputPriceMicros     int64                  `json:"output_price_micros"`
	MaxRunMicros          int64                  `json:"max_run_micros"`
}

func DefaultModelSettings() ModelSettings {
	return ModelSettings{Revision: "0", DailyTokenLimit: 1_000_000, RequestSeconds: 30, RunSeconds: 180, MaxModelCalls: 12, MaxCandidatesPerStage: 3, MaxOutputTokens: 4096, MaxInputBytes: 128 << 10, MaxTotalInputBytes: 1 << 20, MaxResponseBytes: 256 << 10, OutputTokenField: model.MaxCompletionTokens, Currency: ""}
}

func (m ModelSettings) Validate() error {
	rev, err := strconv.ParseUint(m.Revision, 10, 64)
	if err != nil || strconv.FormatUint(rev, 10) != m.Revision || rev == ^uint64(0) {
		return ErrModelSettings
	}
	for _, v := range []struct{ n, max int }{{m.RequestSeconds, 600}, {m.RunSeconds, 600}, {m.MaxModelCalls, 64}, {m.MaxCandidatesPerStage, 8}, {m.MaxOutputTokens, 16384}, {m.MaxInputBytes, 256 << 10}, {m.MaxTotalInputBytes, 4 << 20}, {m.MaxResponseBytes, 1 << 20}} {
		if v.n < 1 || v.n > v.max {
			return ErrModelSettings
		}
	}
	if m.RequestSeconds > m.RunSeconds || m.MaxInputBytes > m.MaxTotalInputBytes {
		return ErrModelSettings
	}
	if m.DailyTokenLimit < 1 || m.DailyTokenLimit > 1_000_000_000 {
		return ErrModelSettings
	}
	if m.apiKey != "" && !validModelKey(m.apiKey) {
		return ErrModelSettings
	}
	if m.APIKeyEnv != "" && !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`).MatchString(m.APIKeyEnv) {
		return ErrModelSettings
	}
	if m.InputPriceMicros < 0 || m.OutputPriceMicros < 0 || m.InputPriceMicros > 1_000_000_000 || m.OutputPriceMicros > 1_000_000_000 || m.MaxRunMicros < 0 || m.MaxRunMicros > 1_000_000_000 {
		return ErrModelSettings
	}
	if m.MaxRunMicros > 0 && (m.InputPriceMicros == 0 || m.OutputPriceMicros == 0 || m.Currency == "" || m.PriceVersion == "") {
		return ErrModelSettings
	}
	for _, v := range []string{m.Model, m.Currency, m.PriceVersion} {
		if len(v) > 128 || !utf8.ValidString(v) || strings.TrimSpace(v) != v || strings.ContainsAny(v, "\x00\r\n") {
			return ErrModelSettings
		}
	}
	if m.Enabled || m.BaseURL != "" || m.Model != "" {
		u, e := url.Parse(m.BaseURL)
		if e != nil || u.Scheme != "https" || u.Host == "" || len(m.BaseURL) > 2048 {
			return ErrModelSettings
		}
		// Validation constructs a frozen provider but performs no network call.
		if _, e = model.NewWithKey(m.ProviderConfig(egress.Config{}), "validation-placeholder"); e != nil {
			return ErrModelSettings
		}
	} else if m.OutputTokenField != model.MaxCompletionTokens && m.OutputTokenField != model.MaxTokens {
		return ErrModelSettings
	}
	if m.ThinkingMode != "" && m.ThinkingMode != "enabled" && m.ThinkingMode != "disabled" {
		return ErrModelSettings
	}
	return nil
}

func (m ModelSettings) ProviderConfig(network egress.Config) model.Config {
	return model.Config{Network: network, BaseURL: m.BaseURL, Model: m.Model, APIKeyEnv: m.APIKeyEnv, Timeout: time.Duration(m.RequestSeconds) * time.Second, MaxOutputTokens: m.MaxOutputTokens, MaxInputBytes: int64(m.MaxInputBytes), MaxResponseBytes: int64(m.MaxResponseBytes), OutputTokenField: m.OutputTokenField, JSONOutput: m.JSONOutput, ThinkingMode: m.ThinkingMode}
}
func (m ModelSettings) Budgets() crawlagent.Budgets {
	return crawlagent.Budgets{MaxDuration: time.Duration(m.RunSeconds) * time.Second, MaxModelCalls: m.MaxModelCalls, MaxCandidatesPerStage: m.MaxCandidatesPerStage, MaxOutputTokens: m.MaxOutputTokens, MaxInputBytes: m.MaxInputBytes, MaxTotalInputBytes: m.MaxTotalInputBytes}
}

// Legacy pricing fields remain decodable for existing rows/clients. The OA
// model profile no longer estimates or gates spending from user-entered prices.
func (m *ModelSettings) clearPricing() {
	m.Currency = ""
	m.PriceVersion = ""
	m.InputPriceMicros = 0
	m.OutputPriceMicros = 0
	m.MaxRunMicros = 0
}

func decodeModelSettings(raw []byte) (ModelSettings, error) {
	var stored storedModelSettings
	if json.Unmarshal(raw, &stored) != nil {
		return ModelSettings{}, ErrModelSettings
	}
	m := stored.ModelSettings
	m.secretCipher = stored.Cipher
	m.KeyConfigured = m.secretCipher != ""
	if m.DailyTokenLimit == 0 {
		m.DailyTokenLimit = 1_000_000
	} // legacy row
	m.clearPricing()
	if m.Validate() != nil {
		return m, ErrModelSettings
	}
	return m, nil
}

// LoadModelSettings deliberately bypasses the generic TTL cache: each new
// analysis sees one committed revision, never an in-process stale override.
func (s *Store) LoadModelSettings(ctx context.Context) (ModelSettings, error) {
	var row models.Config
	err := s.db.WithContext(ctx).Where("cfg_key = ?", KeyAgentModel).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return DefaultModelSettings(), nil
	}
	if err != nil {
		return ModelSettings{}, err
	}
	return decodeModelSettings(row.CfgVal)
}

func (s *Store) SaveModelSettings(ctx context.Context, m ModelSettings, operator string) (ModelSettings, error) {
	m.clearPricing()
	if m.Validate() != nil || strings.TrimSpace(operator) == "" || len(operator) > 128 || strings.ContainsAny(operator, "\x00\r\n") {
		return ModelSettings{}, ErrModelSettings
	}
	want := m.Revision
	n, _ := strconv.ParseUint(want, 10, 64)
	m.Revision = strconv.FormatUint(n+1, 10)
	if m.Validate() != nil {
		return ModelSettings{}, ErrModelSettings
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Seed the immutable default only if missing; then serialize updates on
		// this one row, including concurrent first saves from two settings pages.
		initial, _ := json.Marshal(DefaultModelSettings())
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&models.Config{CfgKey: KeyAgentModel, CfgVal: datatypes.JSON(initial)}).Error; err != nil {
			return err
		}
		var row models.Config
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("cfg_key = ?", KeyAgentModel).Take(&row).Error; err != nil {
			return err
		}
		current, err := decodeModelSettings(row.CfgVal)
		if err != nil {
			return err
		}
		if current.Revision != want {
			return ErrModelConflict
		}
		m.secretCipher = current.secretCipher
		if m.apiKey != "" {
			m.secretCipher, err = s.encryptModelKey(m.apiKey)
			if err != nil {
				return err
			}
		}
		if m.Enabled && m.secretCipher == "" {
			return ErrModelKeyMissing
		}
		m.apiKey = ""
		m.APIKeyEnv = ""
		m.KeyConfigured = m.secretCipher != ""
		raw, err := json.Marshal(storedModelSettings{ModelSettings: m, Cipher: m.secretCipher})
		if err != nil {
			return err
		}
		return tx.Model(&row).Updates(map[string]any{"cfg_val": datatypes.JSON(raw), "updated_by": operator}).Error
	})
	if err != nil {
		return ModelSettings{}, err
	}
	return m, nil
}

func validateModelSettingsValue(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return ErrModelSettings
	}
	_, err = decodeModelSettings(b)
	return err
}

// ModelRevision identifies the exact operator-selected configuration in audit
// sources without exposing provider URL or credential names there.
func (m ModelSettings) ModelRevision() string { return fmt.Sprintf("model-settings-%s", m.Revision) }

// WithAPIKey sets a write-only replacement. Empty means retain the current key.
func (m ModelSettings) WithAPIKey(key string) ModelSettings { m.apiKey = key; return m }

// Formatting and JSON never reveal pending plaintext or stored ciphertext.
func (m ModelSettings) String() string   { b, _ := json.Marshal(m); return string(b) }
func (m ModelSettings) GoString() string { return m.String() }

type storedModelSettings struct {
	ModelSettings
	Cipher string `json:"key_ciphertext,omitempty"`
}
