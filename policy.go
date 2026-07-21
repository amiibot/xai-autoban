package main

import (
	"math"
	"strings"
	"time"
)

// Isolation / trial phases for ban entries.
// hard isolation is time-bounded (TTL + stepped cooldown) — never permanent by itself.
// Permanent removal is only credential delete (HTTP 403 ops).
const (
	phaseIsolated = "isolated"
	phaseTrial    = "trial"
)

// evidenceLedger holds soft counters for accounts that are not (yet) isolated.
type evidenceLedger struct {
	DebtScore float64   `json:"debt_score,omitempty"`
	Streak    int       `json:"streak,omitempty"`
	Step      int       `json:"step,omitempty"` // last isolation step (0-based); survives healthy graduation
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

func normalizePhase(phase string) string {
	switch strings.TrimSpace(strings.ToLower(phase)) {
	case phaseTrial:
		return phaseTrial
	case phaseIsolated, "":
		// Legacy entries without phase are treated as isolated.
		return phaseIsolated
	default:
		return phaseIsolated
	}
}

func (c runtimeConfig) debtWeight(class string) float64 {
	class = strings.TrimSpace(class)
	if c.DebtWeights != nil {
		if w, ok := c.DebtWeights[class]; ok {
			return w
		}
	}
	switch class {
	case classRateLimit:
		return c.DebtFail429
	case classPermission:
		// Weight kept for diagnostics; one-shot classes isolate without needing debt threshold.
		return c.DebtFail401
	default:
		return c.DebtFail401
	}
}

func (c runtimeConfig) isOneShotClass(class string) bool {
	class = strings.TrimSpace(class)
	if class == "" || len(c.OneShotClasses) == 0 {
		return false
	}
	_, ok := c.OneShotClasses[class]
	return ok
}

// shouldIsolate decides whether accumulated evidence crosses the hard-isolation bar.
// Permission (and other one-shot classes) isolate immediately but still with TTL — not permanent ban.
func (c runtimeConfig) shouldIsolate(class string, debt float64, streak int) bool {
	if !c.DebtEnabled {
		return true
	}
	if c.isOneShotClass(class) {
		return true
	}
	if c.StreakThreshold > 0 && streak >= c.StreakThreshold {
		return true
	}
	if c.DebtThreshold > 0 && debt+1e-9 >= c.DebtThreshold {
		return true
	}
	return false
}

func (c runtimeConfig) decayDebt(debt float64) float64 {
	return math.Max(0, debt-c.DebtSuccessDecay)
}

// isolationDuration returns stepped cooldown, capped by class-disable-hours / disable-hours.
// Steps default: 6h → 12h → 24h. step is 0-based and clamped to the last step.
func (c runtimeConfig) isolationDuration(class string, step int) time.Duration {
	steps := c.CooldownSteps
	if len(steps) == 0 {
		steps = defaultCooldownSteps()
	}
	if step < 0 {
		step = 0
	}
	if step >= len(steps) {
		step = len(steps) - 1
	}
	d := steps[step]
	if d <= 0 {
		d = 6 * time.Hour
	}
	cap := c.durationForClass(class)
	if cap > 0 && d > cap {
		return cap
	}
	return d
}

func defaultCooldownSteps() []time.Duration {
	return []time.Duration{6 * time.Hour, 12 * time.Hour, 24 * time.Hour}
}

func defaultOneShotClasses() map[string]struct{} {
	return stringSet([]string{classPermission})
}

func defaultDebtWeights() map[string]float64 {
	return map[string]float64{
		classAuth:             1.5,
		classPayment:          1.5,
		classQuotaFree:        1.5,
		classQuotaPaid:        1.5,
		classPermission:       1.5,
		classRateLimit:        0.5,
		classForbiddenUnknown: 1.0,
		classOther:            1.0,
		classLegacy:           1.5,
	}
}
