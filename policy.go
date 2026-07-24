package main

import (
	"strings"
	"time"
)

// Three-state lifecycle (v1.5+):
//
//	available  — not in ban map
//	cooling    — scheduler skip + CPA disable, auto-enable after TTL
//	pending    — scheduler skip + stay disabled, NO auto-enable (manual unban or 403 delete)
//
// Permanent removal is only credential delete (HTTP 403 ops).
const (
	phaseCooling = "cooling"
	phasePending = "pending"

	// Legacy phase names (migrated on load).
	phaseIsolatedLegacy = "isolated"
	phaseTrialLegacy    = "trial"
)

// cycleLedger remembers consecutive cool-down rounds across enable→available
// so MaxAutoCycles can still be reached. Not a debt score.
type cycleLedger struct {
	Cycle         int       `json:"cycle,omitempty"` // last completed/entered cool-down round (1-based)
	UnusableSince time.Time `json:"unusable_since,omitempty"`
	UpdatedAt     time.Time `json:"updated_at,omitempty"`
}

// evidenceLedger is retained only for unmarshaling pre-1.5 state files.
// Runtime uses cycleLedger; debt/streak are ignored.
type evidenceLedger struct {
	DebtScore float64   `json:"debt_score,omitempty"`
	Streak    int       `json:"streak,omitempty"`
	Step      int       `json:"step,omitempty"`
	Cycle     int       `json:"cycle,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

func normalizePhase(phase string) string {
	switch strings.TrimSpace(strings.ToLower(phase)) {
	case phasePending, "pending_cleanup", "pending-cleanup":
		return phasePending
	case phaseCooling, phaseIsolatedLegacy, "":
		return phaseCooling
	case phaseTrialLegacy:
		// Trial accounts were already re-enabled; treat as cooling for migration
		// (caller may promote high-cycle trial → pending).
		return phaseCooling
	default:
		return phaseCooling
	}
}

func (c runtimeConfig) maxAutoCycles() int {
	if c.MaxAutoCycles <= 0 {
		return 3
	}
	return c.MaxAutoCycles
}

// isolationDuration returns cool-down TTL for this cycle (1-based).
// Default: fixed disable-hours (usually 24h). Optional CooldownSteps map cycle→ladder.
func (c runtimeConfig) isolationDuration(class string, cycle int) time.Duration {
	cap := c.durationForClass(class)
	if cap <= 0 {
		cap = 24 * time.Hour
	}
	steps := c.CooldownSteps
	if len(steps) == 0 {
		return cap
	}
	// cycle is 1-based → index 0 for first cool-down
	idx := cycle - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(steps) {
		idx = len(steps) - 1
	}
	d := steps[idx]
	if d <= 0 {
		d = cap
	}
	if d > cap {
		return cap
	}
	return d
}

func defaultOneShotClasses() map[string]struct{} {
	// Kept for config compatibility / docs: permission is fail-immediate like all classes.
	return stringSet([]string{classPermission})
}
