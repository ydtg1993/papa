package configstore

import (
	"encoding/json"
	"math"
	"testing"
)

func TestDailySettlementOriginalDayUnknownAndOverflow(t *testing.T) {
	original := ModelDailyUsage{Day: "2026-11-08", Used: 90, Reserved: 1000}
	ticket := dailyReservation{day: "2026-11-08", amount: 500}
	known, err := settleDaily(original, ticket, 75, true)
	if err != nil || known.Used != 165 || known.Reserved != 500 || known.Day != original.Day {
		t.Fatalf("known: %+v %v", known, err)
	}
	unknown, err := settleDaily(original, ticket, 0, false)
	if err != nil || unknown.Used != 90 || unknown.Reserved != 1000 || unknown.UnknownCalls != 1 {
		t.Fatalf("unknown: %+v %v", unknown, err)
	}
	// A late response must never debit/refund the next day's counter.
	if _, err = settleDaily(ModelDailyUsage{Day: "2026-11-09", Reserved: 1000}, ticket, 75, true); err == nil {
		t.Fatal("cross-day settlement")
	}
	if _, err = settleDaily(original, ticket, math.MaxInt64, true); err == nil {
		t.Fatal("overflow accepted")
	}
	if _, err = settleDaily(original, dailyReservation{day: original.Day, amount: 1001}, 0, true); err == nil {
		t.Fatal("over-refund accepted")
	}
	if _, err = decodeDaily([]byte(`{"day":"2026-11-08","used_tokens":9223372036854775807,"reserved_tokens":1}`), original.Day); err == nil {
		t.Fatal("corrupt ledger accepted")
	}
}

func TestDailyQuotaValidationAndLegacyDefault(t *testing.T) {
	m := DefaultModelSettings()
	raw, _ := json.Marshal(m)
	var old map[string]any
	json.Unmarshal(raw, &old)
	delete(old, "daily_token_limit")
	raw, _ = json.Marshal(old)
	loaded, err := decodeModelSettings(raw)
	if err != nil || loaded.DailyTokenLimit != 1_000_000 {
		t.Fatal("legacy limit", err)
	}
	for _, v := range []int64{0, -1, 1_000_000_001} {
		m.DailyTokenLimit = v
		if m.Validate() == nil {
			t.Fatal("invalid quota accepted", v)
		}
	}
	for _, key := range []string{"space key", "newline\n", string([]byte{127})} {
		m = DefaultModelSettings().WithAPIKey(key)
		if m.Validate() == nil {
			t.Fatal("invalid key accepted")
		}
	}
}
