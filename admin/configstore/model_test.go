package configstore

import (
	"context"
	"encoding/json"
	"github.com/ydtg1993/papa/v4/internal/gormfake"
	"testing"
	"time"
)

func TestModelSettingsLegacyPricesDoNotGateAnalysis(t *testing.T) {
	m := DefaultModelSettings()
	m.Currency = "CNY"
	m.InputPriceMicros = 1
	m.OutputPriceMicros = 1
	m.MaxRunMicros = 1
	raw, _ := json.Marshal(m)
	restored, err := decodeModelSettings(raw)
	if err != nil || restored.MaxRunMicros != 0 || restored.InputPriceMicros != 0 || restored.Currency != "" {
		t.Fatalf("legacy price blocks load: %+v %v", restored, err)
	}
	if m.Budgets().Prices.MaxMicros != 0 || m.Budgets().Prices.InputPerMillionMicros != 0 {
		t.Fatal("old price still applied")
	}
	if restored.Budgets().MaxModelCalls != 12 || restored.Budgets().MaxOutputTokens != 4096 {
		t.Fatal("nonprice limits lost")
	}
}

func TestModelSettingsBudgetsAndValidation(t *testing.T) {
	good := DefaultModelSettings()
	good.Enabled = true
	good.BaseURL = "https://models.example/v1"
	good.Model = "example-model"
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*ModelSettings){
		"unbounded calls":   func(m *ModelSettings) { m.MaxModelCalls = 0 },
		"excess calls":      func(m *ModelSettings) { m.MaxModelCalls = 65 },
		"duration":          func(m *ModelSettings) { m.RunSeconds = 601 },
		"timeout":           func(m *ModelSettings) { m.RequestSeconds = 181 },
		"input total":       func(m *ModelSettings) { m.MaxTotalInputBytes = 1 },
		"tokens":            func(m *ModelSettings) { m.MaxOutputTokens = 16385 },
		"key value":         func(m *ModelSettings) { m.APIKeyEnv = "sk-secret" },
		"credentials":       func(m *ModelSettings) { m.BaseURL = "https://user:secret@models.example/v1" },
		"query secret":      func(m *ModelSettings) { m.BaseURL = "https://models.example/v1?key=secret" },
		"http":              func(m *ModelSettings) { m.BaseURL = "http://models.example/v1" },
		"model empty":       func(m *ModelSettings) { m.Model = "" },
		"price incomplete":  func(m *ModelSettings) { m.MaxRunMicros = 1 },
		"price overflow":    func(m *ModelSettings) { m.InputPriceMicros = 1_000_000_001 },
		"revision format":   func(m *ModelSettings) { m.Revision = "01" },
		"revision overflow": func(m *ModelSettings) { m.Revision = "18446744073709551615" },
		"thinking":          func(m *ModelSettings) { m.ThinkingMode = "auto" },
	} {
		t.Run(name, func(t *testing.T) {
			m := good
			mutate(&m)
			if m.Validate() == nil {
				t.Fatal("accepted invalid settings")
			}
		})
	}
	frozen := good.Budgets()
	good.MaxModelCalls = 2
	if frozen.MaxModelCalls != 12 || frozen.MaxDuration != 180*time.Second {
		t.Fatal("budgets not frozen")
	}
}

func TestModelSettingsDefaultAndGenericSaveGuard(t *testing.T) {
	f := gormfake.New()
	s := New(gormfake.MustOpen(f))
	m, err := s.LoadModelSettings(context.Background())
	if err != nil || m != DefaultModelSettings() {
		t.Fatalf("%+v %v", m, err)
	}
	before := f.Written()
	if s.Save(KeyAgentModel, m, "alice") == nil {
		t.Fatal("generic Save bypassed revision guard")
	}
	if f.Written() != before {
		t.Fatal("generic Save wrote protected settings")
	}
}
