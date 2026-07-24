package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const stateSchemaVersion = 3

type banEntry struct {
	AuthIndex          string    `json:"auth_index,omitempty"`
	StatusCode         int       `json:"status_code"`
	Class              string    `json:"class,omitempty"`
	Reason             string    `json:"reason"`
	BodyFingerprint    string    `json:"body_fingerprint,omitempty"`
	BannedAt           time.Time `json:"banned_at"`
	ResetAt            time.Time `json:"reset_at"`
	ManagementDisabled bool      `json:"management_disabled"`
	LastAttemptAt      time.Time `json:"last_attempt_at,omitempty"`
	NextAttemptAt      time.Time `json:"next_attempt_at,omitempty"`
	LastError          string    `json:"last_error,omitempty"`

	// Phase: cooling | pending (legacy isolated/trial migrated on load).
	Phase string `json:"phase,omitempty"`
	// Cycle is 1-based cool-down round count for this outage story.
	Cycle int `json:"cycle,omitempty"`
	// Step is legacy JSON only (pre-1.5); migrated into Cycle on load.
	Step int `json:"step,omitempty"`
	// ForceHealthy: manual unban — after re-enable, drop row and clear cycle.
	ForceHealthy bool `json:"force_healthy,omitempty"`
	// UnusableSince: first cool-down of current outage; survives re-cool; cleared on manual unban.
	UnusableSince time.Time `json:"unusable_since,omitempty"`

	// Legacy fields ignored at runtime (still unmarshaled from old state).
	DebtScore      float64   `json:"debt_score,omitempty"`
	Streak         int       `json:"streak,omitempty"`
	TrialSuccesses int       `json:"trial_successes,omitempty"`
	TrialDeadline  time.Time `json:"trial_deadline,omitempty"`
}

type persistedState struct {
	SchemaVersion int                       `json:"schema_version"`
	UpdatedAt     time.Time                 `json:"updated_at"`
	Bans          map[string]banEntry       `json:"bans"`
	Cycles        map[string]cycleLedger    `json:"cycles,omitempty"`
	Evidence      map[string]evidenceLedger `json:"evidence,omitempty"` // legacy load only
}

type banState struct {
	mu        sync.Mutex
	bans      map[string]banEntry
	cycles    map[string]cycleLedger
	stateFile string
}

type banAction struct {
	AuthID    string
	AuthIndex string
	Disabled  bool
}

func newBanState() *banState {
	return &banState{
		bans:   make(map[string]banEntry),
		cycles: make(map[string]cycleLedger),
	}
}

func migrateBanEntry(entry banEntry, maxCycles int) (banEntry, bool) {
	// Returns entry, keep (false = drop row: old trial treated as available with cycle memory elsewhere).
	rawPhase := strings.TrimSpace(strings.ToLower(entry.Phase))
	// Cycle from cycle field or legacy step (step 0 → cycle 1).
	cycle := entry.Cycle
	if cycle <= 0 {
		if entry.Step > 0 {
			cycle = entry.Step
		} else if entry.Step == 0 && (rawPhase == phaseIsolatedLegacy || rawPhase == phaseCooling || rawPhase == "" || rawPhase == phaseTrialLegacy) {
			cycle = 1
		}
	}
	if cycle <= 0 {
		cycle = 1
	}
	entry.Cycle = cycle
	entry.Step = 0
	entry.DebtScore = 0
	entry.Streak = 0
	entry.TrialSuccesses = 0
	entry.TrialDeadline = time.Time{}

	if entry.UnusableSince.IsZero() && !entry.BannedAt.IsZero() {
		entry.UnusableSince = entry.BannedAt
	}

	if maxCycles <= 0 {
		maxCycles = defaultMaxAutoCycles
	}

	switch rawPhase {
	case phaseTrialLegacy:
		// Already re-enabled in half-open; high cycle → pending to avoid stampede; else drop to available.
		if cycle >= 2 || cycle >= maxCycles {
			entry.Phase = phasePending
			entry.ResetAt = time.Time{}
			entry.ManagementDisabled = true
			entry.ForceHealthy = false
			return entry, true
		}
		// Drop row; caller should seed cycle ledger.
		return entry, false
	case phasePending, "pending_cleanup", "pending-cleanup":
		entry.Phase = phasePending
		entry.ResetAt = time.Time{}
		return entry, true
	default:
		// isolated / cooling / empty
		if cycle >= maxCycles {
			entry.Phase = phasePending
			entry.ResetAt = time.Time{}
		} else {
			entry.Phase = phaseCooling
			if entry.ResetAt.IsZero() {
				// Orphan without deadline — treat as pending disable work only if still disabled.
				if !entry.ManagementDisabled {
					return entry, false
				}
				entry.Phase = phasePending
			}
		}
		return entry, true
	}
}

func (s *banState) configure(stateFile string) error {
	return s.configureWithMaxCycles(stateFile, defaultMaxAutoCycles)
}

func (s *banState) configureWithMaxCycles(stateFile string, maxCycles int) error {
	stateFile = strings.TrimSpace(stateFile)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bans == nil {
		s.bans = make(map[string]banEntry)
	}
	if s.cycles == nil {
		s.cycles = make(map[string]cycleLedger)
	}
	if stateFile == s.stateFile {
		return nil
	}
	s.stateFile = stateFile
	if stateFile == "" {
		return nil
	}
	raw, err := os.ReadFile(stateFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var snapshot persistedState
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return err
	}
	if maxCycles <= 0 {
		maxCycles = defaultMaxAutoCycles
	}
	for authID, entry := range snapshot.Bans {
		authID = strings.TrimSpace(authID)
		if authID == "" {
			continue
		}
		migrated, keep := migrateBanEntry(entry, maxCycles)
		if !keep {
			// Old trial dropped to available — remember cycle.
			cl := s.cycles[authID]
			if migrated.Cycle > cl.Cycle {
				cl.Cycle = migrated.Cycle
			}
			if cl.UnusableSince.IsZero() {
				cl.UnusableSince = migrated.UnusableSince
			}
			cl.UpdatedAt = time.Now()
			s.cycles[authID] = cl
			continue
		}
		// pending may have zero ResetAt
		if normalizePhase(migrated.Phase) != phasePending && migrated.ResetAt.IsZero() {
			continue
		}
		if current, ok := s.bans[authID]; !ok || current.ResetAt.Before(migrated.ResetAt) || migrated.ManagementDisabled {
			s.bans[authID] = migrated
		}
	}
	// Prefer new cycles map; fall back to legacy evidence.Step.
	for authID, cl := range snapshot.Cycles {
		authID = strings.TrimSpace(authID)
		if authID == "" {
			continue
		}
		if current, ok := s.cycles[authID]; !ok || current.UpdatedAt.Before(cl.UpdatedAt) {
			s.cycles[authID] = cl
		}
	}
	for authID, ev := range snapshot.Evidence {
		authID = strings.TrimSpace(authID)
		if authID == "" {
			continue
		}
		if _, ok := s.cycles[authID]; ok {
			continue
		}
		cycle := ev.Cycle
		if cycle <= 0 {
			cycle = ev.Step
		}
		if cycle <= 0 {
			continue
		}
		s.cycles[authID] = cycleLedger{Cycle: cycle, UpdatedAt: ev.UpdatedAt}
	}
	// Align cycle ledger with ban rows.
	for authID, entry := range s.bans {
		cl := s.cycles[authID]
		if entry.Cycle > cl.Cycle {
			cl.Cycle = entry.Cycle
		}
		if cl.UnusableSince.IsZero() {
			cl.UnusableSince = entry.UnusableSince
		}
		cl.UpdatedAt = time.Now()
		s.cycles[authID] = cl
	}
	return nil
}

// set merges a cool-down spell. Prefer applyFailure for policy.
func (s *banState) set(authID string, entry banEntry) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bans == nil {
		s.bans = make(map[string]banEntry)
	}
	phase := normalizePhase(entry.Phase)
	if current, ok := s.bans[authID]; ok {
		curPhase := normalizePhase(current.Phase)
		if curPhase == phaseCooling && phase == phaseCooling && current.ResetAt.After(entry.ResetAt) {
			entry.ResetAt = current.ResetAt
		}
		entry.ManagementDisabled = current.ManagementDisabled
		entry.LastAttemptAt = current.LastAttemptAt
		entry.NextAttemptAt = current.NextAttemptAt
		entry.LastError = current.LastError
		if entry.AuthIndex == "" {
			entry.AuthIndex = current.AuthIndex
		}
		if entry.Phase == "" {
			entry.Phase = current.Phase
		}
		if entry.UnusableSince.IsZero() || (!current.UnusableSince.IsZero() && current.UnusableSince.Before(entry.UnusableSince)) {
			entry.UnusableSince = current.UnusableSince
		}
		if entry.Cycle < current.Cycle {
			entry.Cycle = current.Cycle
		}
	}
	if entry.Phase == "" {
		entry.Phase = phaseCooling
	}
	entry.Phase = normalizePhase(entry.Phase)
	if entry.UnusableSince.IsZero() && !entry.BannedAt.IsZero() {
		entry.UnusableSince = entry.BannedAt
	}
	s.bans[authID] = entry
	s.persistLocked()
}

// active: scheduler should skip this auth (cooling or pending).
func (s *banState) active(authID string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.bans[authID]
	if !ok {
		return false
	}
	phase := normalizePhase(entry.Phase)
	if phase == phasePending {
		return true
	}
	// cooling
	if !entry.ResetAt.IsZero() && !now.Before(entry.ResetAt) && !entry.ManagementDisabled && !entry.ForceHealthy {
		delete(s.bans, authID)
		s.persistLocked()
		return false
	}
	return true
}

func (s *banState) clear(authID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.bans[authID]
	delete(s.bans, authID)
	delete(s.cycles, authID)
	if ok {
		s.persistLocked()
	}
	return ok
}

func (s *banState) clearAll() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.bans)
	s.bans = make(map[string]banEntry)
	s.cycles = make(map[string]cycleLedger)
	if n > 0 {
		s.persistLocked()
	}
	return n
}

// requestRelease: manual unban — ForceHealthy enable path or immediate drop.
func (s *banState) requestRelease(authIDs []string, now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := 0
	for _, authID := range authIDs {
		authID = strings.TrimSpace(authID)
		entry, ok := s.bans[authID]
		if !ok {
			continue
		}
		changed++
		if !entry.ManagementDisabled {
			delete(s.bans, authID)
			s.clearCycleLocked(authID)
			continue
		}
		entry.ResetAt = now
		entry.NextAttemptAt = time.Time{}
		entry.LastError = ""
		entry.ForceHealthy = true
		entry.Phase = phaseCooling // enable path via pendingActions
		s.bans[authID] = entry
	}
	if changed > 0 {
		s.persistLocked()
	}
	return changed
}

func (s *banState) requestReleaseStatus(status int, now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := 0
	for authID, entry := range s.bans {
		if entry.StatusCode != status {
			continue
		}
		changed++
		if !entry.ManagementDisabled {
			delete(s.bans, authID)
			s.clearCycleLocked(authID)
			continue
		}
		entry.ResetAt = now
		entry.NextAttemptAt = time.Time{}
		entry.LastError = ""
		entry.ForceHealthy = true
		entry.Phase = phaseCooling
		s.bans[authID] = entry
	}
	if changed > 0 {
		s.persistLocked()
	}
	return changed
}

func (s *banState) requestReleaseAll(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := len(s.bans)
	for authID, entry := range s.bans {
		if !entry.ManagementDisabled {
			delete(s.bans, authID)
			s.clearCycleLocked(authID)
			continue
		}
		entry.ResetAt = now
		entry.NextAttemptAt = time.Time{}
		entry.LastError = ""
		entry.ForceHealthy = true
		entry.Phase = phaseCooling
		s.bans[authID] = entry
	}
	if changed > 0 {
		s.persistLocked()
	}
	return changed
}

func (s *banState) snapshot(now time.Time) map[string]banEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]banEntry)
	changed := false
	for authID, entry := range s.bans {
		phase := normalizePhase(entry.Phase)
		if phase == phaseCooling && !entry.ResetAt.IsZero() && !now.Before(entry.ResetAt) && !entry.ManagementDisabled && !entry.ForceHealthy {
			delete(s.bans, authID)
			changed = true
			continue
		}
		out[authID] = entry
	}
	if changed {
		s.persistLocked()
	}
	return out
}

func (s *banState) pendingActions(now time.Time) []banAction {
	s.mu.Lock()
	defer s.mu.Unlock()
	actions := make([]banAction, 0)
	changed := false
	for authID, entry := range s.bans {
		if !entry.NextAttemptAt.IsZero() && now.Before(entry.NextAttemptAt) {
			continue
		}
		phase := normalizePhase(entry.Phase)

		if phase == phasePending {
			// Only ensure disabled; never auto-enable.
			if !entry.ManagementDisabled {
				actions = append(actions, banAction{AuthID: authID, AuthIndex: entry.AuthIndex, Disabled: true})
			}
			continue
		}

		// cooling
		if entry.ForceHealthy || (!entry.ResetAt.IsZero() && !now.Before(entry.ResetAt)) {
			if entry.ManagementDisabled || entry.ForceHealthy {
				actions = append(actions, banAction{AuthID: authID, AuthIndex: entry.AuthIndex, Disabled: false})
			} else {
				delete(s.bans, authID)
				changed = true
			}
			continue
		}
		if !entry.ManagementDisabled {
			actions = append(actions, banAction{AuthID: authID, AuthIndex: entry.AuthIndex, Disabled: true})
		}
	}
	if changed {
		s.persistLocked()
	}
	return actions
}

// finishAction completes Management disable/enable.
// On enable success: drop ban row; keep cycle ledger unless ForceHealthy (manual unban clears cycle).
func (s *banState) finishAction(action banAction, err error, now time.Time, retryInterval time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.bans[action.AuthID]
	if !ok {
		return
	}
	entry.LastAttemptAt = now
	if err != nil {
		entry.LastError = err.Error()
		entry.NextAttemptAt = now.Add(retryInterval)
		s.bans[action.AuthID] = entry
		s.persistLocked()
		return
	}
	if action.Disabled {
		entry.ManagementDisabled = true
		entry.LastError = ""
		entry.NextAttemptAt = time.Time{}
		s.bans[action.AuthID] = entry
		s.persistLocked()
		return
	}

	// Re-enable succeeded → available.
	force := entry.ForceHealthy
	cycle := entry.Cycle
	unusable := entry.UnusableSince
	delete(s.bans, action.AuthID)
	if force {
		s.clearCycleLocked(action.AuthID)
	} else if cycle > 0 {
		// Remember cycle across available so next fail increments.
		s.cycles[action.AuthID] = cycleLedger{
			Cycle:         cycle,
			UnusableSince: unusable,
			UpdatedAt:     now,
		}
	}
	s.persistLocked()
}

func (s *banState) lookup(authIDs []string) map[string]banEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]banEntry)
	for _, authID := range authIDs {
		authID = strings.TrimSpace(authID)
		if authID == "" {
			continue
		}
		if entry, ok := s.bans[authID]; ok {
			out[authID] = entry
		}
	}
	return out
}

func (s *banState) authIDsByStatus(status int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0)
	for authID, entry := range s.bans {
		if entry.StatusCode == status {
			out = append(out, authID)
		}
	}
	return out
}

func (s *banState) authIDsByClass(class string) []string {
	class = strings.TrimSpace(class)
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0)
	for authID, entry := range s.bans {
		entryClass := entry.Class
		if entryClass == "" {
			entryClass = classLegacy
		}
		if entryClass == class {
			out = append(out, authID)
		}
	}
	return out
}

func (s *banState) authIDsByClasses(classes []string) []string {
	if len(classes) == 0 {
		return nil
	}
	want := make(map[string]struct{}, len(classes))
	for _, class := range classes {
		class = strings.TrimSpace(class)
		if class != "" {
			want[class] = struct{}{}
		}
	}
	if len(want) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0)
	for authID, entry := range s.bans {
		entryClass := entry.Class
		if entryClass == "" {
			entryClass = classLegacy
		}
		if _, ok := want[entryClass]; ok {
			out = append(out, authID)
		}
	}
	return out
}

// applyFailure: fail-immediate cool-down; Cycle++; >= MaxAutoCycles → pending.
func (s *banState) applyFailure(authID, authIndex string, status int, cls classification, now time.Time, cfg runtimeConfig) (isolated bool, entered bool) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return false, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bans == nil {
		s.bans = make(map[string]banEntry)
	}
	if s.cycles == nil {
		s.cycles = make(map[string]cycleLedger)
	}

	entry, hasBan := s.bans[authID]
	phase := ""
	if hasBan {
		phase = normalizePhase(entry.Phase)
	}

	// Already cooling: refresh diagnostics, keep window.
	if hasBan && phase == phaseCooling {
		entry.StatusCode = status
		entry.Class = cls.Class
		entry.Reason = cls.Reason
		entry.BodyFingerprint = cls.Fingerprint
		if authIndex != "" {
			entry.AuthIndex = authIndex
		}
		if entry.UnusableSince.IsZero() {
			if !entry.BannedAt.IsZero() {
				entry.UnusableSince = entry.BannedAt
			} else {
				entry.UnusableSince = now
			}
		}
		s.bans[authID] = entry
		s.persistLocked()
		return true, false
	}

	// Pending: refresh only, stay pending.
	if hasBan && phase == phasePending {
		entry.StatusCode = status
		entry.Class = cls.Class
		entry.Reason = cls.Reason
		entry.BodyFingerprint = cls.Fingerprint
		if authIndex != "" {
			entry.AuthIndex = authIndex
		}
		s.bans[authID] = entry
		s.persistLocked()
		return true, false
	}

	// New cool-down from available (or no ban).
	cl := s.cycles[authID]
	cycle := cl.Cycle + 1
	if cycle < 1 {
		cycle = 1
	}
	unusable := cl.UnusableSince
	if unusable.IsZero() {
		unusable = now
	}
	maxN := cfg.maxAutoCycles()

	entry = banEntry{
		AuthIndex:       authIndex,
		StatusCode:      status,
		Class:           cls.Class,
		Reason:          cls.Reason,
		BodyFingerprint: cls.Fingerprint,
		BannedAt:        now,
		Cycle:           cycle,
		UnusableSince:   unusable,
		// ManagementDisabled false → worker will disable
	}

	if cycle >= maxN {
		entry.Phase = phasePending
		entry.ResetAt = time.Time{}
	} else {
		entry.Phase = phaseCooling
		entry.ResetAt = now.Add(cfg.isolationDuration(cls.Class, cycle))
	}

	s.bans[authID] = entry
	s.cycles[authID] = cycleLedger{Cycle: cycle, UnusableSince: unusable, UpdatedAt: now}
	s.persistLocked()
	return true, true
}

// applySuccess: while available, clear cycle ledger (consecutive-fail story ends).
// Returns whether ledger was cleared (for logging).
func (s *banState) applySuccess(authID string, now time.Time, cfg runtimeConfig) (cleared bool) {
	_ = cfg
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, hasBan := s.bans[authID]; hasBan {
		// Still cooling/pending — ignore success for cycle reset.
		return false
	}
	if s.cycles == nil {
		return false
	}
	if _, ok := s.cycles[authID]; !ok {
		return false
	}
	delete(s.cycles, authID)
	s.persistLocked()
	return true
}

// reisolateTimedOutTrials removed in v1.5 (no trial phase).
func (s *banState) reisolateTimedOutTrials(now time.Time, cfg runtimeConfig) int {
	_ = now
	_ = cfg
	return 0
}

func (s *banState) clearCycleLocked(authID string) {
	if s.cycles == nil {
		return
	}
	delete(s.cycles, authID)
}

func (s *banState) clearEvidenceLocked(authID string, resetStep bool) {
	// Compat name used by older call sites — clear cycle ledger.
	_ = resetStep
	s.clearCycleLocked(authID)
}

// exportState returns copies of bans + cycles for offline analysis.
func (s *banState) exportState() (map[string]banEntry, map[string]cycleLedger) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bansOut := make(map[string]banEntry, len(s.bans))
	for id, e := range s.bans {
		bansOut[id] = e
	}
	cyclesOut := make(map[string]cycleLedger, len(s.cycles))
	for id, e := range s.cycles {
		cyclesOut[id] = e
	}
	return bansOut, cyclesOut
}

func (s *banState) persistLocked() {
	if strings.TrimSpace(s.stateFile) == "" {
		return
	}
	dir := filepath.Dir(s.stateFile)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Error("xai-autoban: failed to create state directory", "error", err)
		return
	}
	// Strip legacy-only fields before write for cleanliness.
	bans := make(map[string]banEntry, len(s.bans))
	for id, e := range s.bans {
		e.Step = 0
		e.DebtScore = 0
		e.Streak = 0
		e.TrialSuccesses = 0
		e.TrialDeadline = time.Time{}
		e.Phase = normalizePhase(e.Phase)
		bans[id] = e
	}
	raw, err := json.MarshalIndent(persistedState{
		SchemaVersion: stateSchemaVersion,
		UpdatedAt:     time.Now().UTC(),
		Bans:          bans,
		Cycles:        s.cycles,
	}, "", "  ")
	if err != nil {
		slog.Error("xai-autoban: failed to marshal state", "error", err)
		return
	}
	tmp := s.stateFile + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		slog.Error("xai-autoban: failed to write state file", "error", err)
		return
	}
	if err := os.Rename(tmp, s.stateFile); err != nil {
		slog.Error("xai-autoban: failed to replace state file", "error", err)
	}
}
