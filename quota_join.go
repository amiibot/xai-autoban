package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// joinQuotaState enriches ban records with rolling-24h usage from a sibling
// grok-quota (or compatible) state file. Observation only — never mutates auth.
//
// Expected layout (subset of grok-quota-state.json):
//
//	{
//	  "accounts": [ {"auth_index":"...","email":"...","tokens_24h":123,"quota_health":"ok"} ],
//	  "by_auth_index": { "...": { ... } }
//	}
//
// Missing file / parse errors are silent: join is best-effort.

type quotaAccountView struct {
	AuthIndex   string `json:"auth_index"`
	Email       string `json:"email,omitempty"`
	Tokens24h   int64  `json:"tokens_24h"`
	QuotaUsed   int64  `json:"quota_used"`
	QuotaLimit  int64  `json:"quota_limit"`
	QuotaHealth string `json:"quota_health,omitempty"`
	StatusKind  string `json:"status_kind,omitempty"`
	OverRef     bool   `json:"over_reference,omitempty"`
	Source      string `json:"source,omitempty"`
}

type quotaStateFile struct {
	GeneratedAt  string                       `json:"generated_at,omitempty"`
	Accounts     []quotaAccountView           `json:"accounts,omitempty"`
	ByAuthIndex  map[string]quotaAccountView  `json:"by_auth_index,omitempty"`
	Summary      map[string]any               `json:"summary,omitempty"`
	SourcePlugin string                       `json:"plugin,omitempty"`
}

type quotaJoinIndex struct {
	LoadedAt time.Time
	Path     string
	ByIndex  map[string]quotaAccountView
	ByEmail  map[string]quotaAccountView
	OK       bool
	Error    string
}

func defaultQuotaStateCandidates() []string {
	var out []string
	for _, env := range []string{"GROK_QUOTA_STATE_PATH", "XAI_AUTOBAN_QUOTA_STATE"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			out = append(out, v)
		}
	}
	// Common CPA-relative paths (cwd is usually the CPA data root).
	out = append(out,
		filepath.Join("plugins", "grok-quota-state.json"),
		"grok-quota-state.json",
		filepath.Join("data", "grok-quota-state.json"),
	)
	return out
}

func loadQuotaJoin(explicitPath string) quotaJoinIndex {
	idx := quotaJoinIndex{
		LoadedAt: time.Now().UTC(),
		ByIndex:  map[string]quotaAccountView{},
		ByEmail:  map[string]quotaAccountView{},
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
