package main

import (
	"net/http"
	"testing"
	"time"
)

func TestFailImmediateIsolates(t *testing.T) {
	state := newBanState()
	c := newAutobanController(state)
	c.handleUsage(pluginapiUsage(http.StatusTooManyRequests, "rate limit", "r1"))
	entry, ok := state.lookup([]string{"r1"})["r1"]
	if !ok {
		t.Fatal("429 should isolate immediately")
	}
	if normalizePhase(entry.Phase) != phaseCooling || entry.Cycle != 1 {
		t.Fatalf("entry=%#v", entry)
	}
	if !state.active("r1", time.Now()) {
		t.Fatal("cooling should block scheduler")
	}
}

func TestPermissionIsolates(t *testing.T) {
	state := newBanState()
	c := newAutobanController(state)
	c.handleUsage(pluginapiUsage(http.StatusForbidden, "permission-denied", "p1"))
	entry := state.lookup([]string{"p1"})["p1"]
	if normalizePhase(entry.Phase) != phaseCooling || entry.Class != classPermission {
		t.Fatalf("entry=%#v", entry)
	}
}

func TestCycleIncrementsAndPending(t *testing.T) {
	state := newBanState()
	cfg := defaultRuntimeConfig()
	cfg.MaxAutoCycles = 3
	now := time.Now()
	cls := classifyFailure(403, "permission-denied", true)

	_, entered := state.applyFailure("u1", "idx", 403, cls, now, cfg)
	if !entered {
		t.Fatal("first fail")
	}
	// simulate enable success (keep cycle)
	state.finishAction(banAction{AuthID: "u1", Disabled: false}, nil, now, time.Minute)
	if _, ok := state.lookup([]string{"u1"})["u1"]; ok {
		t.Fatal("should leave ban table after enable")
	}

	_, entered = state.applyFailure("u1", "idx", 403, cls, now.Add(time.Hour), cfg)
	if !entered {
		t.Fatal("second fail")
	}
	entry := state.lookup([]string{"u1"})["u1"]
	if entry.Cycle != 2 || normalizePhase(entry.Phase) != phaseCooling {
		t.Fatalf("cycle2 entry=%#v", entry)
	}
	state.finishAction(banAction{AuthID: "u1", Disabled: false}, nil, now.Add(2*time.Hour), time.Minute)

	_, entered = state.applyFailure("u1", "idx", 403, cls, now.Add(3*time.Hour), cfg)
	if !entered {
		t.Fatal("third fail")
	}
	entry = state.lookup([]string{"u1"})["u1"]
	if normalizePhase(entry.Phase) != phasePending || entry.Cycle != 3 {
		t.Fatalf("want pending cycle3 got %#v", entry)
	}
	// pending: no enable action
	actions := state.pendingActions(now.Add(100 * time.Hour))
	for _, a := range actions {
		if a.AuthID == "u1" && !a.Disabled {
			t.Fatal("pending must not auto-enable")
		}
	}
}

func TestSuccessClearsCycleWhileHealthy(t *testing.T) {
	state := newBanState()
	cfg := defaultRuntimeConfig()
	now := time.Now()
	cls := classifyFailure(403, "permission-denied", true)
	state.applyFailure("s1", "idx", 403, cls, now, cfg)
	state.finishAction(banAction{AuthID: "s1", Disabled: false}, nil, now, time.Minute)
	if !state.applySuccess("s1", now.Add(time.Second), cfg) {
		t.Fatal("expected cycle clear")
	}
	// next fail should be cycle 1 again
	state.applyFailure("s1", "idx", 403, cls, now.Add(time.Minute), cfg)
	entry := state.lookup([]string{"s1"})["s1"]
	if entry.Cycle != 1 {
		t.Fatalf("cycle=%d", entry.Cycle)
	}
}

func TestManualUnbanClearsPending(t *testing.T) {
	state := newBanState()
	now := time.Now()
	state.set("m1", banEntry{
		StatusCode: 403, Class: classPermission, BannedAt: now,
		ManagementDisabled: true, Phase: phasePending, Cycle: 3,
	})
	if n := state.requestRelease([]string{"m1"}, now); n != 1 {
		t.Fatalf("release n=%d", n)
	}
	entry := state.lookup([]string{"m1"})["m1"]
	if !entry.ForceHealthy {
		t.Fatalf("want force healthy %#v", entry)
	}
	state.finishAction(banAction{AuthID: "m1", Disabled: false}, nil, now, time.Minute)
	if _, ok := state.lookup([]string{"m1"})["m1"]; ok {
		t.Fatal("should drop after manual enable")
	}
}

func TestUnusableSinceSurvivesCycles(t *testing.T) {
	state := newBanState()
	cfg := defaultRuntimeConfig()
	now := time.Now()
	start := now.Add(-30 * time.Hour)
	cls := classifyFailure(403, "permission-denied", true)
	state.applyFailure("u1", "idx", 403, cls, start, cfg)
	entry := state.lookup([]string{"u1"})["u1"]
	if !entry.UnusableSince.Equal(start) {
		t.Fatalf("unusable=%v", entry.UnusableSince)
	}
	state.finishAction(banAction{AuthID: "u1", Disabled: false}, nil, now, time.Minute)
	state.applyFailure("u1", "idx", 403, cls, now, cfg)
	entry = state.lookup([]string{"u1"})["u1"]
	if !entry.UnusableSince.Equal(start) {
		t.Fatalf("unusable must survive: %v vs %v", entry.UnusableSince, start)
	}
	if entry.Cycle != 2 {
		t.Fatalf("cycle=%d", entry.Cycle)
	}
}
