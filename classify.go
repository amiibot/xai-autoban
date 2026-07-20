package main

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strings"
)

// Failure class identifiers used for policy, panel filters, and delete gates.
const (
	classAuth              = "auth"
	classPayment           = "payment"
	classQuotaFree         = "quota_free"
	classQuotaPaid         = "quota_paid"
	classPermission        = "permission"
	classRateLimit         = "rate_limit"
	classForbiddenUnknown  = "forbidden_unknown"
	classOther             = "other"
	classLegacy            = "legacy"
)

// classification is the result of inspecting an upstream failure.
type classification struct {
	Class       string
	Reason      string
	Fingerprint string
}

// classifyFailure maps HTTP status + response body to a stable failure class.
// Body matching is case-insensitive substring; more specific 403 rules win over unknown.
func classifyFailure(status int, body string, classifyBody bool) classification {
	text := strings.ToLower(strings.TrimSpace(body))
	fp := bodyFingerprint(text)

	if !classifyBody || text == "" {
		return classification{
			Class:       classFromStatusOnly(status),
			Reason:      reasonFromStatusOnly(status),
			Fingerprint: fp,
		}
	}

	// Paid spending-limit signals can appear on 402 or 403.
	if status == 402 || status == 403 {
		if containsAny(text,
			"personal-team-blocked:spending-limit",
			"spending-limit",
		) {
			return classification{Class: classQuotaPaid, Reason: "quota_paid", Fingerprint: fp}
		}
	}

	if status == 402 {
		return classification{Class: classPayment, Reason: "payment_required", Fingerprint: fp}
	}

	if status == 429 {
		return classification{Class: classRateLimit, Reason: "rate_limited", Fingerprint: fp}
	}

	if status == 401 {
		return classification{Class: classAuth, Reason: "unauthorized", Fingerprint: fp}
	}

	if status == 403 {
		if containsAny(text,
			"subscription:free-usage-exhausted",
			"free-usage-exhausted",
			"used all the included free usage",
			"included free usage for model",
		) {
			return classification{Class: classQuotaFree, Reason: "quota_free", Fingerprint: fp}
		}
		if containsAny(text,
			"access to the chat endpoint is denied",
			"permission-denied",
			"permission_denied",
			"permission denied",
		) || strings.Trim(text, " .!\t\r\n") == "access denied" {
			return classification{Class: classPermission, Reason: "permission_denied", Fingerprint: fp}
		}
		if containsAny(text,
			"invalid token",
			"token expired",
			"token_expired",
			"unauthorized",
			"authentication",
			"unauthenticated",
		) {
			return classification{Class: classAuth, Reason: "auth_forbidden", Fingerprint: fp}
		}
		return classification{Class: classForbiddenUnknown, Reason: "forbidden_unknown", Fingerprint: fp}
	}

	return classification{
		Class:       classOther,
		Reason:      fmt.Sprintf("http_%d", status),
		Fingerprint: fp,
	}
}

func classFromStatusOnly(status int) string {
	switch status {
	case 401:
		return classAuth
	case 402:
		return classPayment
	case 403:
		return classForbiddenUnknown
	case 429:
		return classRateLimit
	default:
		return classOther
	}
}

func reasonFromStatusOnly(status int) string {
	switch status {
	case 401:
		return "unauthorized"
	case 402:
		return "payment_required"
	case 403:
		return "forbidden"
	case 429:
		return "rate_limited"
	default:
		return fmt.Sprintf("http_%d", status)
	}
}

func containsAny(text string, needles ...string) bool {
	for _, n := range needles {
		if n != "" && strings.Contains(text, n) {
			return true
		}
	}
	return false
}

func bodyFingerprint(text string) string {
	if text == "" {
		return ""
	}
	// Cap input so huge HTML error pages do not bloat state.
	if len(text) > 2048 {
		text = text[:2048]
	}
	sum := sha1.Sum([]byte(text))
	return hex.EncodeToString(sum[:8])
}

func knownClasses() []string {
	return []string{
		classAuth,
		classPayment,
		classQuotaFree,
		classQuotaPaid,
		classPermission,
		classRateLimit,
		classForbiddenUnknown,
		classOther,
		classLegacy,
	}
}

func isKnownClass(class string) bool {
	class = strings.TrimSpace(class)
	for _, c := range knownClasses() {
		if c == class {
			return true
		}
	}
	return false
}
