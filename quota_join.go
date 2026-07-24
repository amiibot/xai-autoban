package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// quotaAccountView is the lean per-account observation row used by the panel.
// Built either from embedded usage.sqlite observer or a compatible state file.
type quotaAccountView struct {
	AuthIndex   string `json:"auth_index"`
	Email       string `json:"email,omitempty"`
	Tokens24h   int64  `json:"tokens_24h"`
	TokensTotal int64  `json:"tokens_total,omitempty"` // lifetime successful tokens
	QuotaUsed   int64  `json:"quota_used"`
	QuotaLimit  int64  `json:"quota_limit"`
	QuotaHealth string `json:"quota_health,omitempty"`
	StatusKind  string `json:"status_kind,omitempty"`
	OverRef     bool   `json:"over_reference,omitempty"`
	Source      string `json:"source,omitempty"`
	// LastUsed is the most recent successful xAI usage (UTC). Zero if unknown.
	LastUsed time.Time `json:"last_used,omitempty"`
}

type quotaStateFile struct {
	GeneratedAt  string                      `json:"generated_at,omitempty"`
	Accounts     []quotaAccountView          `json:"accounts,omitempty"`
	ByAuthIndex  map[string]quotaAccountView `json:"by_auth_index,omitempty"`
	Summary      map[string]any              `json:"summary,omitempty"`
	SourcePlugin string                      `json:"plugin,omitempty"`
}

type quotaJoinIndex struct {
	LoadedAt time.Time
	Path     string
	Source   string // usage_sqlite | state_file
	ByIndex  map[string]quotaAccountView
	ByEmail  map[string]quotaAccountView
	OK       bool
	Error    string
}

// loadQuotaUsage is the single entry for observation join.
// Prefer embedded CPAMP usage.sqlite; optionally fall back to an external state file.
func loadQuotaUsage(cfg runtimeConfig) quotaJoinIndex {
	if cfg.ObserveUsage {
		idx := loadQuotaObserve(cfg.UsageDBPath, cfg.AuthDir)
		if idx.OK {
			return idx
		}
		// Soft-fail: try external file if enabled.
		if cfg.JoinQuotaState {
			fileIdx := loadQuotaJoinFile(cfg.QuotaStateFile)
			if fileIdx.OK {
				return fileIdx
			}
			// Keep the more specific sqlite error if file also missing.
			if fileIdx.Error != "" && idx.Error != "" {
				idx.Error = idx.Error + "; fallback: " + fileIdx.Error
			} else if idx.Error == "" {
				idx.Error = fileIdx.Error
			}
		}
		return idx
	}
	if cfg.JoinQuotaState {
		return loadQuotaJoinFile(cfg.QuotaStateFile)
	}
	return quotaJoinIndex{Error: "usage observation disabled"}
}

func defaultQuotaStateCandidates() []string {
	var out []string
	for _, env := range []string{"GROK_QUOTA_STATE_PATH", "XAI_AUTOBAN_QUOTA_STATE"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			out = append(out, v)
		}
	}
	out = append(out,
		filepath.Join("plugins", "grok-quota-state.json"),
		"grok-quota-state.json",
		filepath.Join("data", "grok-quota-state.json"),
	)
	return out
}

// loadQuotaJoinFile reads a sibling/compatible state JSON (optional fallback).
func loadQuotaJoinFile(explicitPath string) quotaJoinIndex {
	idx := quotaJoinIndex{
		LoadedAt: time.Now().UTC(),
		ByIndex:  map[string]quotaAccountView{},
		ByEmail:  map[string]quotaAccountView{},
		Source:   "state_file",
	}
	candidates := defaultQuotaStateCandidates()
	if p := strings.TrimSpace(explicitPath); p != "" {
		candidates = append([]string{p}, candidates...)
	}
	var raw []byte
	var path string
	var err error
	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		raw, err = os.ReadFile(c)
		if err == nil && len(raw) > 0 {
			path = c
			break
		}
	}
	if path == "" {
		idx.Error = "quota state file not found"
		return idx
	}
	idx.Path = path
	var file quotaStateFile
	if err := json.Unmarshal(raw, &file); err != nil {
		idx.Error = "quota state parse failed: " + err.Error()
		return idx
	}
	add := func(a quotaAccountView) {
		if a.Tokens24h == 0 && a.QuotaUsed > 0 {
			a.Tokens24h = a.QuotaUsed
		}
		if a.AuthIndex != "" {
			idx.ByIndex[strings.TrimSpace(a.AuthIndex)] = a
		}
		if email := strings.ToLower(strings.TrimSpace(a.Email)); email != "" {
			idx.ByEmail[email] = a
		}
	}
	for _, a := range file.Accounts {
		add(a)
	}
	for k, a := range file.ByAuthIndex {
		if strings.TrimSpace(a.AuthIndex) == "" {
			a.AuthIndex = k
		}
		add(a)
	}
	idx.OK = len(idx.ByIndex) > 0 || len(idx.ByEmail) > 0
	if !idx.OK {
		idx.Error = "quota state empty"
	}
	return idx
}

// Deprecated name kept for tests that call loadQuotaJoin directly (file path).
func loadQuotaJoin(explicitPath string) quotaJoinIndex {
	return loadQuotaJoinFile(explicitPath)
}

func (idx quotaJoinIndex) lookup(authID, authIndex, email string) (quotaAccountView, bool) {
	if idx.ByIndex != nil {
		if a, ok := idx.ByIndex[strings.TrimSpace(authIndex)]; ok {
			return a, true
		}
		// Some hosts put auth_index into AuthID.
		if a, ok := idx.ByIndex[strings.TrimSpace(authID)]; ok {
			return a, true
		}
	}
	if email = strings.ToLower(strings.TrimSpace(email)); email != "" && idx.ByEmail != nil {
		if a, ok := idx.ByEmail[email]; ok {
			return a, true
		}
	}
	return quotaAccountView{}, false
}

// accountCount is the unique account base used for pie "normal" estimation.
func (idx quotaJoinIndex) accountCount() int {
	if len(idx.ByIndex) > 0 {
		return len(idx.ByIndex)
	}
	return len(idx.ByEmail)
}
