package main

import (
	"net/http"
	"testing"
	"time"

	"xai-autoban/cpasdk/pluginapi"
)

func TestClassifyFailureBodyRules(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		class  string
		reason string
	}{
		{"401", 401, "", classAuth, "unauthorized"},
		{"402", 402, "payment required", classPayment, "payment_required"},
		{"402 paid limit", 402, "personal-team-blocked:spending-limit", classQuotaPaid, "quota_paid"},
		{"403 free", 403, `subscription:free-usage-exhausted`, classQuotaFree, "quota_free"},
		{"403 free text", 403, "used all the included free usage for model", classQuotaFree, "quota_free"},
		{"403 permission", 403, "permission-denied", classPermission, "permission_denied"},
		{"403 chat denied", 403, "Access to the chat endpoint is denied", classPermission, "permission_denied"},
		{"403 unknown", 403, "something else", classForbiddenUnknown, "forbidden_unknown"},
		{"429", 429, "rate", classRateLimit, "rate_limited"},
		{"403 auth body", 403, "invalid token expired", classAuth, "auth_forbidden"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyFailure(tc.status, tc.body, true)
			if got.Class != tc.class || got.Reason != tc.reason {
				t.Fatalf("got class=%s reason=%s want class=%s reason=%s", got.Class, got.Reason, tc.class, tc.reason)
			}
		})
	}
}

func TestClassifyBodyDisabledFallsBackToStatus(t *testing.T) {
	got := classifyFailure(403, "permission-denied", false)
	if got.Class != classForbiddenUnknown || got.Reason != "forbidden" {
		t.Fatalf("unexpected: %#v", got)
	}
}

func TestDurationForClassUsesConfig(t *testing.T) {
	cfg := defaultRuntimeConfig()
	cfg.ClassDisableHours[classRateLimit] = 10 * time.Minute
	if d := cfg.durationForClass(classRateLimit); d != 10*time.Minute {
		t.Fatalf("rate_limit duration %v", d)
	}
	if d := cfg.durationForClass(classAuth); d != 24*time.Hour {
		t.Fatalf("auth duration %v", d)
	}
	if !cfg.classDeletable(classPermission) {
		t.Fatal("permission should be deletable by default")
	}
	if cfg.classDeletable(classQuotaFree) {
		t.Fatal("quota_free should not be deletable by default")
	}
}

func TestHandleUsageClassifiesAndSetsDuration(t *testing.T) {
	state := newBanState()
	controller := newAutobanController(state)
	// no management client needed for queueing
	controller.handleUsage(pluginapiUsage(http.StatusForbidden, "permission-denied", "auth-perm"))
	m := state.lookup([]string{"auth-perm"})
	entry, ok := m["auth-perm"]
	if !ok {
		t.Fatal("missing ban entry")
	}
	if entry.Class != classPermission {
		t.Fatalf("class=%s", entry.Class)
	}
	if entry.StatusCode != 403 {
		t.Fatalf("status=%d", entry.StatusCode)
	}
	// one-shot permission uses step-0 ladder (default 6h), not full 24h cap
	if entry.Phase != phaseCooling {
		t.Fatalf("phase=%s", entry.Phase)
	}
	if d := entry.ResetAt.Sub(entry.BannedAt); d < 23*time.Hour || d > 25*time.Hour {
		t.Fatalf("duration %v want ~24h", d)
	}
}

// helper to avoid importing issues with package-level names in test file only
func pluginapiUsage(status int, body, authID string) pluginapi.UsageRecord {
	return pluginapi.UsageRecord{
		Provider: "xai",
		AuthID:   authID,
		Failed:   true,
		Failure:  pluginapi.UsageFailure{StatusCode: status, Body: body},
	}
}
