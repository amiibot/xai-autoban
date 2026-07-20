package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	// defaultReferenceTokens is a UI baseline only — never a hard cap or pre-ban threshold.
	defaultReferenceTokens int64 = 2_000_000
	defaultUsageWindow           = 24 * time.Hour
	xaiProvider                  = "xai"
	sourceRollingUsage           = "cpamp_usage_events_rolling_24h"
)

var (
	quotaCodeRE = regexp.MustCompile(`(?i)(free-usage-exhausted|spending-limit|out of credits|personal-team-blocked:spending-limit|resource[_ ]?exhausted|insufficient.?credits|quota exceeded|quota_exceeded)`)
	notQuotaRE  = regexp.MustCompile(`(?i)(permission-denied|service.?capacity|overloaded|deactivated_workspace|invalid.?api.?key|unauthorized)`)
	plainRateRE = regexp.MustCompile(`(?i)rate[\s_-]?limit`)
)

type usageAgg struct {
	authIndex  string
	email      string
	authFile   string
	tokens24h  int64
	success24h int64
	failed24h  int64
	lastUsage  time.Time
}

type coolingAgg struct {
	authIndex  string
	email      string
	authFile   string
	reason     string
	failureAt  time.Time
	recoverAt  time.Time
	statusCode int
}

// observe cache — short TTL so panel refresh is cheap.
var (
	observeMu    sync.RWMutex
	observeCache quotaJoinIndex
	observeAt    time.Time
	observeTTL   = 5 * time.Second
)

func dynamicLimit(tokens24h, reference int64) int64 {
	if reference <= 0 {
		reference = defaultReferenceTokens
	}
	if tokens24h < 0 {
		tokens24h = 0
	}
	if tokens24h > reference {
		return tokens24h
	}
	return reference
}

func openUsageDB(path string) (*sql.DB, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("usage db path empty")
	}
	// Read-only URI keeps the live CPAMP writer unblocked.
	dsn := "file:" + filepath.ToSlash(path) + "?mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func isQuotaExhaustionFailure(status int, summary, body, errKind, errCode string) bool {
	blob := strings.ToLower(strings.Join([]string{summary, body, errKind, errCode}, "\n"))
	if quotaCodeRE.MatchString(blob) {
		return status == 402 || status == 429 || status == 403 || status == 0
	}
	if notQuotaRE.MatchString(blob) {
		return false
	}
	if plainRateRE.MatchString(blob) && !strings.Contains(blob, "usage") {
		return false
	}
	if status == 402 && (strings.Contains(blob, "credit") || strings.Contains(blob, "spending") || strings.Contains(blob, "quota")) {
		return true
	}
	return false
}

func extractQuotaReason(summary string) string {
	re := regexp.MustCompile(`"code"\s*:\s*"([^"]+)"`)
	if m := re.FindStringSubmatch(summary); len(m) == 2 {
		return m[1]
	}
	low := strings.ToLower(summary)
	switch {
	case strings.Contains(low, "free-usage-exhausted"):
		return "subscription:free-usage-exhausted"
	case strings.Contains(low, "spending-limit"):
		return "personal-team-blocked:spending-limit"
	default:
		return "quota_exhausted"
	}
}

func deriveEmail(accountSnapshot, authFile string) string {
	accountSnapshot = strings.TrimSpace(accountSnapshot)
	if strings.Contains(accountSnapshot, "@") {
		return accountSnapshot
	}
	base := filepath.Base(strings.TrimSpace(authFile))
	base = strings.TrimSuffix(base, ".json")
	if strings.HasPrefix(base, "xai-") {
		return strings.TrimPrefix(base, "xai-")
	}
	return base
}

func loadRollingUsage(db *sql.DB, now time.Time, window time.Duration) (map[string]usageAgg, error) {
	sinceMS := now.Add(-window).UnixMilli()
	rows, err := db.Query(`
		SELECT auth_index,
			COALESCE(MAX(auth_file_snapshot), ''),
			COALESCE(MAX(account_snapshot), ''),
			COALESCE(SUM(CASE WHEN failed = 0 THEN total_tokens ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN failed = 0 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN failed = 1 THEN 1 ELSE 0 END), 0),
			COALESCE(MAX(CASE WHEN failed = 0 THEN timestamp_ms ELSE NULL END), 0)
		FROM usage_events
		WHERE auth_provider_snapshot = ? AND timestamp_ms >= ?
		GROUP BY auth_index
	`, xaiProvider, sinceMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]usageAgg{}
	for rows.Next() {
		var authIndex, authFile, account string
		var tokens, success, failed, lastMS int64
		if err := rows.Scan(&authIndex, &authFile, &account, &tokens, &success, &failed, &lastMS); err != nil {
			return nil, err
		}
		authIndex = strings.TrimSpace(authIndex)
		if authIndex == "" {
			continue
		}
		agg := usageAgg{
			authIndex:  authIndex,
			email:      deriveEmail(account, authFile),
			authFile:   filepath.Base(strings.TrimSpace(authFile)),
			tokens24h:  tokens,
			success24h: success,
			failed24h:  failed,
		}
		if lastMS > 0 {
			agg.lastUsage = time.UnixMilli(lastMS).UTC()
		}
		out[authIndex] = agg
	}
	return out, rows.Err()
}

func loadActiveCoolings(db *sql.DB, now time.Time, lookback time.Duration) (map[string]coolingAgg, error) {
	sinceMS := now.Add(-lookback).UnixMilli()
	rows, err := db.Query(`
		SELECT id, timestamp_ms, fail_status_code,
			COALESCE(fail_summary, ''), COALESCE(fail_body, ''),
			COALESCE(auth_index, ''), COALESCE(auth_file_snapshot, ''),
			COALESCE(account_snapshot, ''),
			COALESCE(header_quota_recover_at_ms, 0),
			COALESCE(header_error_kind, ''), COALESCE(header_error_code, '')
		FROM usage_events
		WHERE failed = 1
		  AND auth_provider_snapshot = ?
		  AND timestamp_ms >= ?
		  AND fail_status_code IN (402, 403, 429)
		ORDER BY timestamp_ms ASC
	`, xaiProvider, sinceMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	latest := map[string]coolingAgg{}
	for rows.Next() {
		var id, tsMS, status, recoverMS int64
		var summary, body, authIndex, authFile, account, errKind, errCode string
		if err := rows.Scan(&id, &tsMS, &status, &summary, &body, &authIndex, &authFile, &account, &recoverMS, &errKind, &errCode); err != nil {
			return nil, err
		}
		if !isQuotaExhaustionFailure(int(status), summary, body, errKind, errCode) {
			continue
		}
		authIndex = strings.TrimSpace(authIndex)
		if authIndex == "" {
			continue
		}
		failAt := time.UnixMilli(tsMS).UTC()
		var recoverAt time.Time
		if recoverMS > tsMS {
			recoverAt = time.UnixMilli(recoverMS).UTC()
		} else {
			recoverAt = failAt.Add(defaultUsageWindow)
		}
		if !recoverAt.After(now) {
			continue
		}
		latest[authIndex] = coolingAgg{
			authIndex:  authIndex,
			email:      deriveEmail(account, authFile),
			authFile:   filepath.Base(strings.TrimSpace(authFile)),
			reason:     extractQuotaReason(summary),
			failureAt:  failAt,
			recoverAt:  recoverAt,
			statusCode: int(status),
		}
	}
	return latest, rows.Err()
}

// loadQuotaObserve reads CPAMP usage.sqlite (rolling 24h) and builds the same
// join index the panel already consumes. Observation only — never mutates auth.
func loadQuotaObserve(usageDBPath, authDir string) quotaJoinIndex {
	observeMu.RLock()
	if !observeAt.IsZero() && time.Since(observeAt) < observeTTL && observeCache.OK {
		idx := observeCache
		observeMu.RUnlock()
		return idx
	}
	observeMu.RUnlock()

	idx := computeQuotaObserve(usageDBPath, authDir)

	observeMu.Lock()
	observeCache = idx
	observeAt = time.Now()
	observeMu.Unlock()
	return idx
}

func computeQuotaObserve(usageDBPath, authDir string) quotaJoinIndex {
	idx := quotaJoinIndex{
		LoadedAt: time.Now().UTC(),
		ByIndex:  map[string]quotaAccountView{},
		ByEmail:  map[string]quotaAccountView{},
		Source:   "usage_sqlite",
	}

	dbPath := detectUsageDBPath(usageDBPath)
	if dbPath == "" {
		idx.Error = "usage.sqlite not found (set usage-db-path or XAI_AUTOBAN_USAGE_DB / CPAMP_USAGE_DB)"
		return idx
	}
	idx.Path = dbPath

	db, err := openUsageDB(dbPath)
	if err != nil {
		idx.Error = "open usage db: " + err.Error()
		return idx
	}
	defer db.Close()

	now := time.Now().UTC()
	usage, err := loadRollingUsage(db, now, defaultUsageWindow)
	if err != nil {
		idx.Error = "rolling usage: " + err.Error()
		return idx
	}
	cooling, err := loadActiveCoolings(db, now, 48*time.Hour)
	if err != nil {
		// Soft: still return token totals even if cooling query fails (schema drift).
		cooling = map[string]coolingAgg{}
	}

	// Optional auth-dir enrichment (email from live files).
	_ = authDir // reserved: membership join uses management API for pie; email from events is enough.
	// Still try to read auth dir emails when present for better ban-row join by email.
	emailByFile := map[string]string{}
	if dir := detectAuthDir(authDir); dir != "" {
		if entries, err := os.ReadDir(dir); err == nil {
			for _, ent := range entries {
				if ent.IsDir() || !strings.HasSuffix(strings.ToLower(ent.Name()), ".json") {
					continue
				}
				name := ent.Name()
				if !strings.HasPrefix(strings.ToLower(name), "xai-") {
					continue
				}
				emailByFile[strings.ToLower(name)] = deriveEmail("", name)
			}
		}
	}

	add := func(a quotaAccountView) {
		if a.Tokens24h == 0 && a.QuotaUsed > 0 {
			a.Tokens24h = a.QuotaUsed
		}
		if a.QuotaUsed == 0 && a.Tokens24h > 0 {
			a.QuotaUsed = a.Tokens24h
		}
		if a.QuotaLimit == 0 {
			a.QuotaLimit = dynamicLimit(a.Tokens24h, defaultReferenceTokens)
		}
		if a.AuthIndex != "" {
			idx.ByIndex[strings.TrimSpace(a.AuthIndex)] = a
		}
		if email := strings.ToLower(strings.TrimSpace(a.Email)); email != "" {
			idx.ByEmail[email] = a
		}
	}

	// Union usage + cooling keys so accounts with only failures still appear.
	keys := map[string]struct{}{}
	for k := range usage {
		keys[k] = struct{}{}
	}
	for k := range cooling {
		keys[k] = struct{}{}
	}

	for authIndex := range keys {
		u := usage[authIndex]
		c, cool := cooling[authIndex]
		email := u.email
		if email == "" {
			email = c.email
		}
		authFile := u.authFile
		if authFile == "" {
			authFile = c.authFile
		}
		if email == "" && authFile != "" {
			if e, ok := emailByFile[strings.ToLower(authFile)]; ok {
				email = e
			}
		}
		tokens := u.tokens24h
		if tokens < 0 {
			tokens = 0
		}
		view := quotaAccountView{
			AuthIndex:  authIndex,
			Email:      email,
			Tokens24h:  tokens,
			QuotaUsed:  tokens,
			QuotaLimit: dynamicLimit(tokens, defaultReferenceTokens),
			Source:     sourceRollingUsage,
			OverRef:    tokens > defaultReferenceTokens,
		}
		if cool {
			view.QuotaHealth = "cooldown"
			view.StatusKind = "quota_issue"
		} else if tokens > defaultReferenceTokens {
			view.QuotaHealth = "ok"
			view.StatusKind = "high_usage"
		} else {
			view.QuotaHealth = "ok"
			view.StatusKind = "active"
		}
		add(view)
	}

	idx.OK = len(idx.ByIndex) > 0 || len(idx.ByEmail) > 0
	if !idx.OK {
		// DB open but no xAI rows in window — still "connected", empty pool.
		idx.OK = true
		idx.Error = ""
	}
	return idx
}
