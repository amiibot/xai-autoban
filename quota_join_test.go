package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadQuotaJoinFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "grok-quota-state.json")
	payload := map[string]any{
		"accounts": []map[string]any{
			{
				"auth_index":  "idx-a",
				"email":       "a@example.com",
				"tokens_24h":  1_500_000,
				"quota_limit": 2_000_000,
				"quota_health": "ok",
			},
		},
	}
	raw, _ := json.Marshal(payload)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	idx := loadQuotaJoin(path)
	if !idx.OK {
		t.Fatalf("join not ok: %s", idx.Error)
	}
	a, ok := idx.lookup("auth-id", "idx-a", "")
	if !ok || a.Tokens24h != 1_500_000 {
		t.Fatalf("lookup by auth_index failed: %#v ok=%v", a, ok)
	}
	b, ok := idx.lookup("", "", "a@example.com")
	if !ok || b.Tokens24h != 1_500_000 {
		t.Fatalf("lookup by email failed: %#v", b)
	}
}

func TestLoadQuotaJoinMissingIsSoftFailure(t *testing.T) {
	idx := loadQuotaJoin(filepath.Join(t.TempDir(), "nope.json"))
	if idx.OK {
		t.Fatal("expected missing file to be not ok")
	}
	if idx.Error == "" {
		t.Fatal("expected error message")
	}
}

func TestBuildChartsSkipZeros(t *testing.T) {
	byStatus := buildStatusChart(map[int]int{403: 3, 401: 1})
	if len(byStatus) != 2 {
		t.Fatalf("status slices: %#v", byStatus)
	}
	byClass := buildClassChart(map[string]int{classPermission: 2, classRateLimit: 1})
	if len(byClass) != 2 {
		t.Fatalf("class slices: %#v", byClass)
	}
}
