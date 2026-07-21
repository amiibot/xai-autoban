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

const stateSchemaVersion = 2

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

	// Phase: isolated (hard, scheduler skip) | trial (half-open, may be scheduled).
	// Empty on load → isolated (legacy schema).
	Phase string `json:"phase,omitempty"`
	// Step is the cooldown ladder index used for this isolation spell (0→6h, 1→12h, 2→24h).
	Step int `json:"step,omitempty"`
	// DebtScore / Streak mirrored onto the ban row for diagnostics while isolated/trial.
	DebtScore float64 `json:"debt_score,omitempty"`
	Streak    int     `json:"streak,omitempty"`
	// TrialSuccesses counts attributed successes while phase=trial.
	TrialSuccesses int `json:"trial_successes,omitempty"`
	// TrialDeadline ends a zombie trial (timeout → re-isolate). Zero = no deadline.
	TrialDeadline time.Time `json:"trial_deadline,omitempty"`
	// ForceHealthy: manual unban — after re-enable, drop row without half-open trial.
	ForceHealthy bool `json:"force_healthy,omitempty"`
}

type persistedState struct {
	SchemaVersion int                       `json:"schema_version"`
	UpdatedAt     time.Time                 `json:"updated_at"`
	Bans          map[string]banEntry       `json:"bans"`
	Evidence      map[string]evidenceLedger `json:"evidence,omitempty"`
}

type banState struct {
	mu        sync.Mutex
	bans      map[string]banEntry
	evidence  map[string]evidenceLedger
	stateFile string
}

type banAction struct {
	AuthID    string
	AuthIndex string
	Disabled  bool
}

func newBanState() *banState {
	return &banState{
		bans:     make(map[string]banEntry),
		evidence: make(map[string]evidenceLedger),
	}
}

func (s *banState) configure(stateFile string) error {
	stateFile = strings.TrimSpace(stateFile)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bans == nil {
		s.bans = make(map[string]banEntry)
	}
	if s.evidence == nil {
		s.evidence = make(map[string]evidenceLedger)
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
	for authID, entry := range snapshot.Bans {
		authID = strings.TrimSpace(authID)
		if authID == "" || entry.ResetAt.IsZero() {
			continue
		}
		entry.Phase = normalizePhase(entry.Phase)
		if current, ok := s.bans[authID]; !ok || current.ResetAt.Before(entry.ResetAt) || entry.ManagementDisabled {
			s.bans[authID] = entry
		}
	}
	for authID, ev := range snapshot.Evidence {
		authID = strings.TrimSpace(authID)
		if authID == "" {
			continue
		}
		if current, ok := s.evidence[authID]; !ok || current.UpdatedAt.Before(ev.UpdatedAt) {
			s.evidence[authID] = ev
		}
	}
	return nil
}

// set merges a new isolation spell onto an auth. Prefer applyFailure/applySuccess for policy.
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
	if current, ok := s.bans[authID]; ok {
		// Keep longer isolation windows when refreshing evidence while already isolated.
		if current.ResetAt.After(entry.ResetAt) && normalizePhase(current.Phase) == phaseIsolated && normalizePhase(entry.Phase) == phaseIsolated {
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
	}
	if entry.Phase == "" {
		entry.Phase = phaseIsolated
	}
	s.bans[authID] = entry
	s.persistLocked()
}

// active reports whether SchedulerPick should skip this auth.
// Only phase=isolated blocks; trial (half-open) is schedulable.
func (s *banState) active(authID string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.bans[authID]
	if !ok {
		return false
	}
	phase := normalizePhase(entry.Phase)
	if phase == phaseTrial {
		// Timed-out trial is still listed until process re-isolates; do not skip scheduling
		// so traffic can keep probing (or fail and re-isolate via usage).
		return false
	}
	// isolated
	if !now.Before(entry.ResetAt) && !entry.ManagementDisabled {
		// Expired isolation without disable still pending enable path — drop if nothing left to do.
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
	delete(s.evidence, authID)
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
	s.evidence = make(map[string]evidenceLedger)
	if n > 0 {
		s.persistLocked()
	}
	return n
}

// requestRelease is a manual unban: skip half-open trial (ForceHealthy).
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
		if !entry.ManagementDisabled && normalizePhase(entry.Phase) != phaseIsolated {
			// Trial or already enabled → drop immediately.
			delete(s.bans, authID)
			s.clearEvidenceLocked(authID, true)
			continue
		}
		if !entry.ManagementDisabled {
			delete(s.bans, authID)
			s.clearEvidenceLocked(authID, true)
			continue
		}
		entry.ResetAt = now
		entry.NextAttemptAt = time.Time{}
		entry.LastError = ""
		entry.ForceHealthy = true
		entry.Phase = phaseIsolated
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
			s.clearEvidenceLocked(authID, true)
			continue
		}
		entry.ResetAt = now
		entry.NextAttemptAt = time.Time{}
		entry.LastError = ""
		entry.ForceHealthy = true
		entry.Phase = phaseIsolated
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
			s.clearEvidenceLocked(authID, true)
			continue
		}
		entry.ResetAt = now
		entry.NextAttemptAt = time.Time{}
		entry.LastError = ""
		entry.ForceHealthy = true
		entry.Phase = phaseIsolated
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
		// Drop orphan trial/isolated rows that are fully idle and past deadline with no management work.
		if phase == phaseIsolated && !now.Before(entry.ResetAt) && !entry.ManagementDisabled && !entry.ForceHealthy {
			// Leave for pendingActions enable path only when ManagementDisabled; else GC.
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

		if phase == phaseTrial {
			// Trial accounts stay enabled; no disable/enable here.
			// Timeout is handled by applySuccess/applyFailure or explicit re-isolate in controller tick.
			continue
		}

		// isolated
		if !now.Before(entry.ResetAt) {
			if entry.ManagementDisabled || entry.ForceHealthy {
				actions = append(actions, banAction{AuthID: authID, AuthIndex: entry.AuthIndex, Disabled: false})
			} else {
				// Isolation window ended without ever disabling (e.g. no management) → drop.
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

// finishAction completes a Management disable/enable. halfOpen enables trial transition.
func (s *banState) finishAction(action banAction, err error, now time.Time, retryInterval time.Duration, halfOpen bool, trialMax time.Duration, successThreshold int) {
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
		entry.Phase = phaseIsolated
		s.bans[action.AuthID] = entry
		s.persistLocked()
		return
	}

	// Re-enable succeeded.
	if entry.ForceHealthy || !halfOpen {
		delete(s.bans, action.AuthID)
		s.clearEvidenceLocked(action.AuthID, true)
		s.persistLocked()
		return
	}

	// Enter half-open trial: schedulable, still listed for panel.
	entry.ManagementDisabled = false
	entry.LastError = ""
	entry.NextAttemptAt = time.Time{}
	entry.ForceHealthy = false
	entry.Phase = phaseTrial
	entry.TrialSuccesses = 0
	if trialMax <= 0 {
		trialMax = 6 * time.Hour
	}
	entry.TrialDeadline = now.Add(trialMax)
	// ResetAt displays trial deadline remaining in the panel.
	entry.ResetAt = entry.TrialDeadline
	if successThreshold <= 0 {
		successThreshold = 2
	}
	_ = successThreshold
	s.bans[action.AuthID] = entry
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

// applyFailure updates debt/streak and may open or refresh isolation / fail trial.
// Returns whether scheduler isolation is active after the call (phase isolated).
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
	if s.evidence == nil {
		s.evidence = make(map[string]evidenceLedger)
	}

	ev := s.evidence[authID]
	weight := cfg.debtWeight(cls.Class)
	if !cfg.DebtEnabled {
		weight = 0
	}
	ev.DebtScore += weight
	ev.Streak++
	ev.UpdatedAt = now

	entry, hasBan := s.bans[authID]
	phase := ""
	if hasBan {
		phase = normalizePhase(entry.Phase)
	}

	// Already hard-isolated: refresh diagnostics, keep window (do not shrink).
	if hasBan && phase == phaseIsolated {
		entry.StatusCode = status
		entry.Class = cls.Class
		entry.Reason = cls.Reason
		entry.BodyFingerprint = cls.Fingerprint
		entry.DebtScore = ev.DebtScore
		entry.Streak = ev.Streak
		if authIndex != "" {
			entry.AuthIndex = authIndex
		}
		s.bans[authID] = entry
		s.evidence[authID] = ev
		s.persistLocked()
		return true, false
	}

	// Trial failure → re-isolate with stepped cooldown.
	if hasBan && phase == phaseTrial {
		step := entry.Step + 1
		if step < 0 {
			step = 0
		}
		ev.Step = step
		duration := cfg.isolationDuration(cls.Class, step)
		entry = banEntry{
			AuthIndex:       firstNonEmpty(authIndex, entry.AuthIndex),
			StatusCode:      status,
			Class:           cls.Class,
			Reason:          cls.Reason,
			BodyFingerprint: cls.Fingerprint,
			BannedAt:        now,
			ResetAt:         now.Add(duration),
			Phase:           phaseIsolated,
			Step:            step,
			DebtScore:       ev.DebtScore,
			Streak:          ev.Streak,
			// Management was enabled during trial; need disable again.
			ManagementDisabled: false,
		}
		s.bans[authID] = entry
		s.evidence[authID] = ev
		s.persistLocked()
		return true, true
	}

	// Healthy: maybe escalate to isolation.
	if !cfg.shouldIsolate(cls.Class, ev.DebtScore, ev.Streak) {
		s.evidence[authID] = ev
		s.persistLocked()
		return false, false
	}

	step := ev.Step
	// First isolation after healthy uses current step (0 after full graduation).
	duration := cfg.isolationDuration(cls.Class, step)
	entry = banEntry{
		AuthIndex:       authIndex,
		StatusCode:      status,
		Class:           cls.Class,
		Reason:          cls.Reason,
		BodyFingerprint: cls.Fingerprint,
		BannedAt:        now,
		ResetAt:         now.Add(duration),
		Phase:           phaseIsolated,
		Step:            step,
		DebtScore:       ev.DebtScore,
		Streak:          ev.Streak,
	}
	s.bans[authID] = entry
	s.evidence[authID] = ev
	s.persistLocked()
	return true, true
}

// applySuccess decays debt, clears streak, and may graduate a trial account.
// Returns whether the row was removed (graduated) and whether trial progress changed.
func (s *banState) applySuccess(authID string, now time.Time, cfg runtimeConfig) (graduated bool) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.evidence == nil {
		s.evidence = make(map[string]evidenceLedger)
	}

	ev := s.evidence[authID]
	ev.DebtScore = cfg.decayDebt(ev.DebtScore)
	ev.Streak = 0
	ev.UpdatedAt = now

	entry, hasBan := s.bans[authID]
	if !hasBan {
		if ev.DebtScore <= 0 && ev.Step == 0 {
			delete(s.evidence, authID)
		} else {
			s.evidence[authID] = ev
		}
		s.persistLocked()
		return false
	}

	phase := normalizePhase(entry.Phase)
	if phase != phaseTrial {
		// Success while isolated (shouldn't schedule) — still decay evidence on the row.
		entry.DebtScore = ev.DebtScore
		entry.Streak = 0
		s.bans[authID] = entry
		s.evidence[authID] = ev
		s.persistLocked()
		return false
	}

	// Half-open trial success.
	if !entry.TrialDeadline.IsZero() && now.After(entry.TrialDeadline) {
		// Zombie trial: treat as failure path externally; keep row, controller will re-isolate.
		entry.DebtScore = ev.DebtScore
		entry.Streak = 0
		s.bans[authID] = entry
		s.evidence[authID] = ev
		s.persistLocked()
		return false
	}

	entry.TrialSuccesses++
	entry.DebtScore = ev.DebtScore
	entry.Streak = 0
	threshold := cfg.HalfOpenSuccessThreshold
	if threshold <= 0 {
		threshold = 2
	}
	if entry.TrialSuccesses >= threshold {
		delete(s.bans, authID)
		// Full graduation: reset ladder step.
		ev.Step = 0
		if ev.DebtScore <= 0 {
			delete(s.evidence, authID)
		} else {
			s.evidence[authID] = ev
		}
		s.persistLocked()
		return true
	}
	s.bans[authID] = entry
	s.evidence[authID] = ev
	s.persistLocked()
	return false
}

// reisolateTimedOutTrials moves expired trials back to hard isolation (step+1).
func (s *banState) reisolateTimedOutTrials(now time.Time, cfg runtimeConfig) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for authID, entry := range s.bans {
		if normalizePhase(entry.Phase) != phaseTrial {
			continue
		}
		if entry.TrialDeadline.IsZero() || !now.After(entry.TrialDeadline) {
			continue
		}
		step := entry.Step + 1
		class := entry.Class
		if class == "" {
			class = classLegacy
		}
		duration := cfg.isolationDuration(class, step)
		ev := s.evidence[authID]
		ev.Step = step
		ev.UpdatedAt = now
		s.evidence[authID] = ev
		entry.Phase = phaseIsolated
		entry.Step = step
		entry.BannedAt = now
		entry.ResetAt = now.Add(duration)
		entry.TrialSuccesses = 0
		entry.TrialDeadline = time.Time{}
		entry.ManagementDisabled = false
		entry.NextAttemptAt = time.Time{}
		entry.LastError = ""
		entry.ForceHealthy = false
		s.bans[authID] = entry
		n++
	}
	if n > 0 {
		s.persistLocked()
	}
	return n
}

func (s *banState) clearEvidenceLocked(authID string, resetStep bool) {
	if s.evidence == nil {
		return
	}
	if resetStep {
		delete(s.evidence, authID)
		return
	}
	ev := s.evidence[authID]
	ev.DebtScore = 0
	ev.Streak = 0
	s.evidence[authID] = ev
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
	raw, err := json.MarshalIndent(persistedState{
		SchemaVersion: stateSchemaVersion,
		UpdatedAt:     time.Now(),
		Bans:          s.bans,
		Evidence:      s.evidence,
	}, "", "  ")
	if err != nil {
		slog.Error("xai-autoban: failed to encode state", "error", err)
		return
	}
	tmp := s.stateFile + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		slog.Error("xai-autoban: failed to write state", "error", err)
		return
	}
	if err := os.Rename(tmp, s.stateFile); err != nil {
		slog.Error("xai-autoban: failed to replace state", "error", err)
	}
}
