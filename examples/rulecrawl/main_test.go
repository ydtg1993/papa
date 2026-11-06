package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestRunHTMLAndCursorJSON(t *testing.T) {
	for _, mode := range []string{"html", "json"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var first, second bytes.Buffer
			if err := run(ctx, &first, mode); err != nil {
				t.Fatal(err)
			}
			var got report
			if err := json.Unmarshal(first.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Mode != mode || got.CatalogPages != 2 || got.CatalogItems != 2 || got.DetailPages != 2 || !got.ReplayChecked {
				t.Fatalf("pagination, duplicate suppression or replay failed: %+v", got)
			}
			if len(got.Records) != 2 || got.Records[0].ID != "A" || got.Records[1].ID != "B" || len(got.Pages) != 4 {
				t.Fatalf("unexpected accepted data: %+v", got)
			}
			for _, record := range got.Records {
				if record.Rule.ID != "demo-detail" || record.Rule.Version != "html-v1" || len(record.BodySHA256) != 64 || record.Description == "" {
					t.Fatalf("missing business data or provenance: %+v", record)
				}
			}
			for _, page := range got.Pages {
				if page.Stage == "catalog" && (page.Rule.Version != mode+"-v1" || page.Items != 1) {
					t.Fatalf("catalog version or cross-page dedup lost: %+v", page)
				}
			}
			if err := run(ctx, &second, mode); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(first.Bytes(), second.Bytes()) {
				t.Fatal("report changes between independent runs")
			}
		})
	}
}

func TestRunRejectsUnknownMode(t *testing.T) {
	if err := run(context.Background(), &bytes.Buffer{}, "unknown"); err == nil {
		t.Fatal("unknown mode accepted")
	}
}
