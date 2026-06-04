// Copyright 2026 Zibby Lab. Apache-2.0.

package cli

import (
	"testing"
)

// TestParseDDBScan_FiltersActiveSharedOnly verifies the DynamoDB-backed
// MapSource parse + filter: only status=active && mode in {shared, ""} rows
// with a non-empty host+upstream become routes.
func TestParseDDBScan_FiltersActiveSharedOnly(t *testing.T) {
	raw := []byte(`{
      "Items": [
        {"host":{"S":"a.apps.zibby.dev"},"upstream":{"S":"10.0.0.1:3000"},"status":{"S":"active"},"mode":{"S":"shared"}},
        {"host":{"S":"b.apps.zibby.dev"},"upstream":{"S":"10.0.0.2:8080"},"status":{"S":"active"},"mode":{"S":""}},
        {"host":{"S":"paused.apps.zibby.dev"},"upstream":{"S":"10.0.0.3:3000"},"status":{"S":"paused"},"mode":{"S":"shared"}},
        {"host":{"S":"ded.apps.zibby.dev"},"upstream":{"S":"10.0.0.4:3000"},"status":{"S":"active"},"mode":{"S":"dedicated"}},
        {"host":{"S":""},"upstream":{"S":"10.0.0.5:3000"},"status":{"S":"active"},"mode":{"S":"shared"}},
        {"host":{"S":"noup.apps.zibby.dev"},"upstream":{"S":""},"status":{"S":"active"},"mode":{"S":"shared"}}
      ]
    }`)

	routes, err := parseDDBScan(raw)
	if err != nil {
		t.Fatalf("parseDDBScan error: %v", err)
	}
	// Expect only the two valid active+shared(or empty-mode) rows.
	got := map[string]string{}
	for _, r := range routes {
		got[r.Host] = r.Upstream
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 routes, got %d: %+v", len(got), got)
	}
	if got["a.apps.zibby.dev"] != "10.0.0.1:3000" {
		t.Errorf("missing/wrong route a: %+v", got)
	}
	if got["b.apps.zibby.dev"] != "10.0.0.2:8080" {
		t.Errorf("empty-mode row should be treated as shared: %+v", got)
	}
	if _, ok := got["paused.apps.zibby.dev"]; ok {
		t.Errorf("paused row must be dropped: %+v", got)
	}
	if _, ok := got["ded.apps.zibby.dev"]; ok {
		t.Errorf("dedicated-mode row must be dropped: %+v", got)
	}
}

func TestParseDDBScan_EmptyAndMalformed(t *testing.T) {
	if rs, err := parseDDBScan([]byte(`{"Items":[]}`)); err != nil || len(rs) != 0 {
		t.Fatalf("empty scan should yield 0 routes, got %d err=%v", len(rs), err)
	}
	if _, err := parseDDBScan([]byte(`not json`)); err == nil {
		t.Fatalf("expected error on malformed json")
	}
}
