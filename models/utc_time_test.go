package models

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm/schema"
)

func TestUTCTaskTimeSerializerIgnoresDriverLocation(t *testing.T) {
	s, err := schema.Parse(&CrawlerTask{}, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatal(err)
	}
	f := s.LookUpField("NotBefore")
	serializer := utcTimeSerializer{}
	zone := time.FixedZone("driver+08", 8*60*60)
	// Driver attaches its Loc to a zone-less UTC DATETIME. Preserve wall
	// components rather than applying the attached location a second time.
	want := time.Date(2026, 11, 12, 6, 30, 0, 123000000, time.UTC)
	for _, raw := range []any{time.Date(2026, 11, 12, 6, 30, 0, 123000000, zone), "2026-11-12 06:30:00.123", []byte("2026-11-12 06:30:00.123")} {
		var row CrawlerTask
		if err := serializer.Scan(context.Background(), f, reflect.ValueOf(&row).Elem(), raw); err != nil {
			t.Fatal(err)
		}
		if row.NotBefore == nil || !row.NotBefore.Equal(want) {
			t.Fatalf("driver location changed instant: %v", row.NotBefore)
		}
	}
	local := want.In(zone)
	got, err := serializer.Value(context.Background(), f, reflect.Value{}, &local)
	if err != nil || got != "2026-11-12 06:30:00.123" {
		t.Fatalf("write=%v err=%v", got, err)
	}
	var row CrawlerTask
	if err := serializer.Scan(context.Background(), f, reflect.ValueOf(&row).Elem(), nil); err != nil || row.NotBefore != nil {
		t.Fatalf("null=%v err=%v", row.NotBefore, err)
	}
}
