package config

import (
	"testing"
	"time"
)

func TestParseDurationRange(t *testing.T) {
	fixed, err := ParseDurationRange("10s")
	if err != nil {
		t.Fatalf("parse fixed: %v", err)
	}
	if !fixed.Fixed() || fixed.Min != 10*time.Second || fixed.Max != 10*time.Second {
		t.Fatalf("unexpected fixed range: %+v", fixed)
	}

	rng, err := ParseDurationRange("10s-30s")
	if err != nil {
		t.Fatalf("parse range: %v", err)
	}
	if rng.Fixed() || rng.Min != 10*time.Second || rng.Max != 30*time.Second {
		t.Fatalf("unexpected range: %+v", rng)
	}

	if _, err := ParseDurationRange("30s-10s"); err == nil {
		t.Fatal("expected error for max < min")
	}
	if _, err := ParseDurationRange(""); err == nil {
		t.Fatal("expected error for empty")
	}
}

func TestDurationRangeRandom(t *testing.T) {
	fixed := DurationRange{Min: 5 * time.Second, Max: 5 * time.Second}
	if got := fixed.Random(); got != 5*time.Second {
		t.Fatalf("fixed Random() = %v", got)
	}

	rng := DurationRange{Min: 10 * time.Second, Max: 30 * time.Second}
	for i := 0; i < 1000; i++ {
		got := rng.Random()
		if got < 10*time.Second || got > 30*time.Second {
			t.Fatalf("Random() out of range: %v", got)
		}
	}
}

func TestDurationRangeUnmarshalText(t *testing.T) {
	var d DurationRange
	if err := d.UnmarshalText([]byte("1m-2m")); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.Min != time.Minute || d.Max != 2*time.Minute {
		t.Fatalf("unexpected unmarshaled range: %+v", d)
	}
}
