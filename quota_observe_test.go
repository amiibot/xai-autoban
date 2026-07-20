package main

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func openTestUsageDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usage.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE usage_events (
			id INTEGER PRIMARY KEY,
			timestamp_ms INTEGER NOT NULL,
			auth_provider_snapshot TEXT,
			auth_index TEXT,
			auth_file_snapshot TEXT,
			account_snapshot TEXT,
			failed INTEGER DEFAULT 0,
			total_tokens INTEGER DEFAULT 0,
			fail_status_code INTEGER DEFAULT 0,
			fail_summary TEXT,
			fail_body TEXT,
			header_quota_recover_at_ms INTEGER DEFAULT 0,
			header_error_kind TEXT,
			header_error_code TEXT
		);
	`)
	if err != nil {
		t.Fatal(err)
	}
	return path, db
}

func TestLoadQuotaObserveFromSQLite(t *testing.T) {
	// Reset observe cache so tests don't see each other's results.
	observeMu.Lock()
	observeAt = time.Time{}
	observeCache = quotaJoinIndex{}
	observeMu.Unlock()

	path, db := openTestUsageDB(t)
	now := time.Now().UTC()
	ts := now.Add(-1 * time.Hour).UnixMilli()
	_, err := db.Exec(`
		INSERT INTO usage_events (timestamp_ms, auth_provider_snapshot, auth_index, auth_file_snapshot, account_snapshot, failed, total_tokens)
		VALUES (?, 'xai', 'idx-a', 'xai-a@example.com.json', 'a@example.com', 0, 1500000),
		       (?, 'xai', 'idx-a', 'xai-a@example.com.json', 'a@example.com', 0, 500000)
	`, ts, ts+1)
	if err != nil {
		t.Fatal(err)
	}
	// Non-xAI should be ignored.
	_, err = db.Exec(`
		INSERT INTO usage_events (timestamp_ms, auth_provider_snapshot, auth_index, failed, total_tokens)
		VALUES (?, 'openai', 'other', 0, 999999)
	`, ts)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	idx := computeQuotaObserve(path, "")
	if !idx.OK {
		t.Fatalf("observe not ok: %s", idx.Error)
	}
	if idx.Source != "usage_sqlite" {
		t.Fatalf("source=%s", idx.Source)
	}
	a, ok := idx.lookup("auth-id", "idx-a", "")
	if !ok || a.Tokens24h != 2_000_000 {
		t.Fatalf("lookup failed: %#v ok=%v", a, ok)
	}
	if a.QuotaLimit < a.Tokens24h {
		t.Fatalf("limit should be at least usage: %#v", a)
	}
	// email join
	b, ok := idx.lookup("", "", "a@example.com")
	if !ok || b.Tokens24h != 2_000_000 {
		t.Fatalf("email lookup: %#v", b)
	}
}

func TestLoadQuotaObserveMissingIsSoftFailure(t *testing.T) {
	observeMu.Lock()
	observeAt = time.Time{}
	observeCache = quotaJoinIndex{}
	observeMu.Unlock()

	idx := computeQuotaObserve(filepath.Join(t.TempDir(), "nope.sqlite"), "")
	if idx.OK {
		t.Fatal("expected missing db to be not ok")
	}
	if idx.Error == "" {
		t.Fatal("expected error message")
	}
}

func TestLoadQuotaUsagePrefersSQLite(t *testing.T) {
	observeMu.Lock()
	observeAt = time.Time{}
	observeCache = quotaJoinIndex{}
	observeMu.Unlock()

	path, db := openTestUsageDB(t)
	ts := time.Now().UTC().Add(-30 * time.Minute).UnixMilli()
	_, err := db.Exec(`
		INSERT INTO usage_events (timestamp_ms, auth_provider_snapshot, auth_index, account_snapshot, failed, total_tokens)
		VALUES (?, 'xai', 'idx-b', 'b@example.com', 0, 100)
	`, ts)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	cfg := defaultRuntimeConfig()
	cfg.ObserveUsage = true
	cfg.UsageDBPath = path
	cfg.JoinQuotaState = false
	idx := loadQuotaUsage(cfg)
	if !idx.OK || idx.Source != "usage_sqlite" {
		t.Fatalf("idx=%#v", idx)
	}
	a, ok := idx.lookup("", "idx-b", "")
	if !ok || a.Tokens24h != 100 {
		t.Fatalf("%#v", a)
	}
}

func TestIsQuotaExhaustionFailure(t *testing.T) {
	if !isQuotaExhaustionFailure(403, `subscription:free-usage-exhausted`, "", "", "") {
		t.Fatal("expected free usage exhausted")
	}
	if isQuotaExhaustionFailure(403, "permission-denied", "", "", "") {
		t.Fatal("permission is not quota")
	}
	if isQuotaExhaustionFailure(429, "rate limit exceeded", "", "", "") {
		t.Fatal("plain rate limit is not quota")
	}
}
