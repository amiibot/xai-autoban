package main

import (
	"net/http"
	"testing"
	"time"

	"xai-autoban/cpasdk/pluginapi"
)

func TestPermissionOneShotIsolatesWithSteppedTTL(t *testing.T) {
	state := newBanState()
	c := newAutobanController(state)
	c.handleUsage(pluginapiUsage(http.StatusForbidden, "permission-denied", "p1"))
	entry, ok := state.lookup([]string{"p1"})["p1"]
	if !ok {
		t.Fatal("permission should isolate on first failure")
	}
	if entry.Phase != phaseIsolated || entry.Step != 0 {
		t.Fatalf("entry=%#v", entry)
	}
	d := entry.ResetAt.Sub(entry.BannedAt)
	if d < 5*time.Hour || d > 7*time.Hour {
		t.Fatalf("step0 duration %v", d)
	}
	// hard isolate is not permanent: still scheduled for eventual enable
	if !state.active("p1", time.Now()) {
		t.Fatal("isolated should block scheduler")
	}
}

func TestDebtRequiresThresholdForQuota(t *testing.T) {
	state := newBanState()
	c := newAutobanController(state)
	// quota_free weight 1.5; threshold 2.0 → first hit only evidence
	c.handleUsage(pluginapiUsage(http.StatusForbidden, "subscription:free-usage-exhausted", "q1"))
	if _, ok := state.lookup([]string{"q1"})["q1"]; ok {
		t.Fatal("first quota_free should not isolate")
	}
	c.handleUsage(pluginapiUsage(http.StatusForbidden, "subscription:free-usage-exhausted", "q1"))
	entry, ok := state.lookup([]string{"q1"})["q1"]
	if !ok {
		t.Fatal("second quota_free should isolate via debt")
	}
	if entry.Class != classQuotaFree {
		t.Fatalf("class=%s", entry.Class)
	}
}

func TestHalfOpenTrialGraduateAndFail(t *testing.T) {
	state := newBanState()
	cfg := defaultRuntimeConfig()
	now := time.Now()
	// Seed isolated entry that was management-disabled.
	state.set("t1", banEntry{
		AuthIndex: "idx", StatusCode: 403, Class: classPermission, Reason: "permission_denied",
		BannedAt: now.Add(-time.Hour), ResetAt: now.Add(-time.Second),
		ManagementDisabled: true, Phase: phaseIsolated, Step: 0,
	})
	// Re-enable → trial
	state.finishAction(banAction{AuthID: "t1", AuthIndex: "idx", Disabled: false}, nil, now, time.Minute, true, 6*time.Hour, 2)
	entry := state.lookup([]string{"t1"})["t1"]
	if entry.Phase != phaseTrial {
		t.Fatalf("want trial got %#v", entry)
	}
	if state.active("t1", now) {
		t.Fatal("trial must be schedulable")
	}

	// One success — still trial
	if state.applySuccess("t1", now, cfg) {
		t.Fatal("should not graduate on first success")
	}
	// Second success — graduate
	if !state.applySuccess("t1", now.Add(time.Second), cfg) {
		t.Fatal("expected graduation")
	}
	if _, ok := state.lookup([]string{"t1"})["t1"]; ok {
		t.Fatal("graduated account should leave ban table")
	}

	// Fail path: trial → re-isolate step+1
	state.set("t2", banEntry{
		AuthIndex: "idx2", StatusCode: 403, Class: classPermission, Reason: "permission_denied",
		BannedAt: now, ResetAt: now.Add(time.Hour), ManagementDisabled: false,
		Phase: phaseTrial, Step: 0, TrialDeadline: now.Add(6 * time.Hour),
	})
	cls := classifyFailure(403, "permission-denied", true)
	isolated, entered := state.applyFailure("t2", "idx2", 403, cls, now, cfg)
	if !isolated || !entered {
		t.Fatal("trial failure should re-isolate")
	}
	entry = state.lookup([]string{"t2"})["t2"]
	if entry.Phase != phaseIsolated || entry.Step != 1 {
		t.Fatalf("re-isolate entry %#v", entry)
	}
	d := entry.ResetAt.Sub(entry.BannedAt)
	if d < 11*time.Hour || d > 13*time.Hour {
		t.Fatalf("step1 duration %v want ~12h", d)
	}
}

func TestManualUnbanSkipsTrial(t *testing.T) {
	state := newBanState()
	now := time.Now()
	state.set("m1", banEntry{
		StatusCode: 403, Class: classPermission, BannedAt: now, ResetAt: now.Add(time.Hour),
		ManagementDisabled: true, Phase: phaseIsolated,
	})
	if n := state.requestRelease([]string{"m1"}, now); n != 1 {
		t.Fatalf("release n=%d", n)
	}
	entry := state.lookup([]string{"m1"})["m1"]
	if !entry.ForceHealthy {
		t.Fatalf("manual release should force healthy: %#v", entry)
	}
	// enable without half-open
	state.finishAction(banAction{AuthID: "m1", Disabled: false}, nil, now, time.Minute, true, 6*time.Hour, 2)
	if _, ok := state.lookup([]string{"m1"})["m1"]; ok {
		t.Fatal("force healthy should drop after re-enable even if half-open enabled")
	}
}

func TestSuccessDecaysDebt(t *testing.T) {
	state := newBanState()
	c := newAutobanController(state)
	// one rate_limit → debt 0.5
	c.handleUsage(pluginapi.UsageRecord{
		Provider: "xai", AuthID: "s1", Failed: true,
		Failure: pluginapi.UsageFailure{StatusCode: 429},
	})
	// success decays by 1.0 → 0
	c.handleUsage(pluginapi.UsageRecord{Provider: "xai", AuthID: "s1", Failed: false})
	state.mu.Lock()
	ev := state.evidence["s1"]
	state.mu.Unlock()
	if ev.DebtScore > 0.01 || ev.Streak != 0 {
		t.Fatalf("after success evidence=%#v", ev)
	}
}
