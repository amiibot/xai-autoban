package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultManagementURL       = "http://127.0.0.1:8317"
	defaultManagementKeyEnv    = "CPA_MANAGEMENT_KEY"
	defaultDisableHours        = 24
	defaultRequestTimeout      = 10 * time.Second
	defaultRetryInterval       = time.Minute
	defaultAuthFailureCooldown = 10 * time.Minute
	defaultStateFile           = "xai-autoban-state.json"
)

type runtimeConfig struct {
	Enabled             bool
	ManagementURL       string
	ManagementKey       string
	ManagementKeyEnv    string
	DisableDuration     time.Duration
	StatusCodes         map[int]struct{}
	RequestTimeout      time.Duration
	RetryInterval       time.Duration
	AuthFailureCooldown time.Duration
	StateFile           string
	ClassifyBody        bool
	// ObserveUsage enables embedded rolling-24h usage from CPAMP usage.sqlite.
	// Default true — no separate grok-quota plugin required.
	ObserveUsage bool
	// UsageDBPath optional explicit path to usage.sqlite (else auto-detect).
	UsageDBPath string
	// AuthDir optional CPA auths directory for email enrichment.
	AuthDir string
	// QuotaStateFile is optional path to an external state JSON (legacy / fallback).
	// Empty = auto-detect common CPA paths / env GROK_QUOTA_STATE_PATH.
	QuotaStateFile string
	// JoinQuotaState allows falling back to an external state file when
	// ObserveUsage fails or is disabled. Default true (soft fallback only).
	JoinQuotaState bool
	// ClassDisableHours maps failure class → isolation duration cap.
	// Missing classes fall back to DisableDuration. Stepped cooldowns are min(step, cap).
	ClassDisableHours map[string]time.Duration
	// DeletableClasses are the only classes allowed for permanent Management delete.
	// Note: permanent delete is HTTP 403 ops in the controller; this set is retained for config/API.
	DeletableClasses map[string]struct{}

	// Debt / half-open policy (see policy.go). Hard isolation is never permanent by itself.
	DebtEnabled              bool
	DebtThreshold            float64
	DebtSuccessDecay         float64
	DebtFail401              float64 // default weight for most classes
	DebtFail429              float64 // default weight for rate_limit
	DebtWeights              map[string]float64
	StreakThreshold          int
	OneShotClasses           map[string]struct{} // e.g. permission → isolate on first failure (still with TTL)
	CooldownSteps             []time.Duration    // 6h, 12h, 24h
	HalfOpenEnabled          bool
	HalfOpenSuccessThreshold int
	TrialMaxDuration         time.Duration
}

type rawRuntimeConfig struct {
	Enabled                    *bool              `yaml:"enabled"`
	ManagementURL              string             `yaml:"management-url"`
	ManagementKey              string             `yaml:"management-key"`
	ManagementKeyEnv           string             `yaml:"management-key-env"`
	DisableHours               int                `yaml:"disable-hours"`
	StatusCodes                []int              `yaml:"status-codes"`
	RequestTimeoutSeconds      int                `yaml:"request-timeout-seconds"`
	RetryIntervalSeconds       int                `yaml:"retry-interval-seconds"`
	AuthFailureCooldownSeconds int                `yaml:"auth-failure-cooldown-seconds"`
	StateFile                  string             `yaml:"state-file"`
	ClassifyBody               *bool              `yaml:"classify-body"`
	ObserveUsage               *bool              `yaml:"observe-usage"`
	UsageDBPath                string             `yaml:"usage-db-path"`
	AuthDir                    string             `yaml:"auth-dir"`
	QuotaStateFile             string             `yaml:"quota-state-file"`
	JoinQuotaState             *bool              `yaml:"join-quota-state"`
	ClassDisableHours          map[string]float64 `yaml:"class-disable-hours"`
	DeletableClasses           []string           `yaml:"deletable-classes"`
	DebtEnabled                *bool              `yaml:"debt-enabled"`
	DebtThreshold              *float64           `yaml:"debt-threshold"`
	DebtSuccessDecay           *float64           `yaml:"debt-success-decay"`
	DebtFail401                *float64           `yaml:"debt-fail-401"`
	DebtFail429                *float64           `yaml:"debt-fail-429"`
	StreakThreshold            *int               `yaml:"streak-threshold"`
	OneShotClasses             []string           `yaml:"one-shot-classes"`
	CooldownHours               []float64          `yaml:"cooldown-hours"`
	HalfOpenEnabled            *bool              `yaml:"half-open-enabled"`
	HalfOpenSuccessThreshold   *int               `yaml:"half-open-success-threshold"`
	TrialMaxHours              *float64           `yaml:"trial-max-hours"`
}

func defaultRuntimeConfig() runtimeConfig {
	return runtimeConfig{
		Enabled:             true,
		ManagementURL:       defaultManagementURL,
		ManagementKeyEnv:    defaultManagementKeyEnv,
		DisableDuration:     defaultDisableHours * time.Hour,
		StatusCodes:         statusCodeSet([]int{401, 402, 403, 429}),
		RequestTimeout:      defaultRequestTimeout,
		RetryInterval:       defaultRetryInterval,
		AuthFailureCooldown: defaultAuthFailureCooldown,
		StateFile:           defaultStateFile,
		ClassifyBody:        true,
		ObserveUsage:        true,
		JoinQuotaState:      true, // file fallback only when sqlite observe fails
		// Cap for stepped isolation (actual spell = min(step ladder, class cap)).
		ClassDisableHours: defaultClassDisableHours(defaultDisableHours * time.Hour),
		// Config surface only; runtime permanent delete is HTTP 403 (not class-gated).
		DeletableClasses: stringSet([]string{classPermission}),

		DebtEnabled:              true,
		DebtThreshold:            2.0,
		DebtSuccessDecay:         1.0,
		DebtFail401:              1.5,
		DebtFail429:              0.5,
		DebtWeights:              defaultDebtWeights(),
		StreakThreshold:          3,
		OneShotClasses:           defaultOneShotClasses(), // permission: first hit → hard isolate with TTL
		CooldownSteps:             defaultCooldownSteps(),
		HalfOpenEnabled:          true,
		HalfOpenSuccessThreshold: 2,
		TrialMaxDuration:         6 * time.Hour,
	}
}

func defaultClassDisableHours(fallback time.Duration) map[string]time.Duration {
	out := make(map[string]time.Duration, 8)
	for _, c := range []string{
		classAuth,
		classPayment,
		classQuotaFree,
		classQuotaPaid,
		classPermission,
		classRateLimit,
		classForbiddenUnknown,
		classOther,
	} {
		out[c] = fallback
	}
	return out
}

func parseRuntimeConfig(raw []byte) (runtimeConfig, error) {
	cfg := defaultRuntimeConfig()
	if len(strings.TrimSpace(string(raw))) == 0 {
		return cfg, nil
	}

	var input rawRuntimeConfig
	if err := yaml.Unmarshal(raw, &input); err != nil {
		return runtimeConfig{}, fmt.Errorf("解析插件配置失败: %w", err)
	}
	if input.Enabled != nil {
		cfg.Enabled = *input.Enabled
	}
	if value := strings.TrimSpace(input.ManagementURL); value != "" {
		cfg.ManagementURL = strings.TrimRight(value, "/")
	}
	parsedURL, err := url.Parse(cfg.ManagementURL)
	if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || parsedURL.Host == "" {
		return runtimeConfig{}, fmt.Errorf("management-url 必须是有效的 http/https 地址")
	}
	cfg.ManagementKey = strings.TrimSpace(input.ManagementKey)
	if value := strings.TrimSpace(input.ManagementKeyEnv); value != "" {
		cfg.ManagementKeyEnv = value
	}
	if input.DisableHours > 0 {
		cfg.DisableDuration = time.Duration(input.DisableHours) * time.Hour
		// When only disable-hours is set, keep all class defaults aligned unless overridden below.
		cfg.ClassDisableHours = defaultClassDisableHours(cfg.DisableDuration)
	}
	if len(input.StatusCodes) > 0 {
		cfg.StatusCodes = statusCodeSet(input.StatusCodes)
	}
	if input.RequestTimeoutSeconds > 0 {
		cfg.RequestTimeout = time.Duration(input.RequestTimeoutSeconds) * time.Second
	}
	if input.RetryIntervalSeconds > 0 {
		cfg.RetryInterval = time.Duration(input.RetryIntervalSeconds) * time.Second
	}
	if input.AuthFailureCooldownSeconds > 0 {
		cfg.AuthFailureCooldown = time.Duration(input.AuthFailureCooldownSeconds) * time.Second
	}
	if value := strings.TrimSpace(input.StateFile); value != "" {
		cfg.StateFile = filepath.Clean(value)
	}
	if input.ClassifyBody != nil {
		cfg.ClassifyBody = *input.ClassifyBody
	}
	if input.ObserveUsage != nil {
		cfg.ObserveUsage = *input.ObserveUsage
	}
	if value := strings.TrimSpace(input.UsageDBPath); value != "" {
		cfg.UsageDBPath = filepath.Clean(value)
	}
	if value := strings.TrimSpace(input.AuthDir); value != "" {
		cfg.AuthDir = filepath.Clean(value)
	}
	if value := strings.TrimSpace(input.QuotaStateFile); value != "" {
		cfg.QuotaStateFile = filepath.Clean(value)
	}
	if input.JoinQuotaState != nil {
		cfg.JoinQuotaState = *input.JoinQuotaState
	}
	if len(input.ClassDisableHours) > 0 {
		merged := defaultClassDisableHours(cfg.DisableDuration)
		for class, hours := range input.ClassDisableHours {
			class = strings.TrimSpace(strings.ToLower(class))
			if class == "" || hours <= 0 {
				continue
			}
			merged[class] = time.Duration(hours * float64(time.Hour))
		}
		cfg.ClassDisableHours = merged
	}
	if input.DeletableClasses != nil {
		// Explicit empty list means nothing is permanently deletable.
		cfg.DeletableClasses = stringSet(input.DeletableClasses)
	}
	if input.DebtEnabled != nil {
		cfg.DebtEnabled = *input.DebtEnabled
	}
	if input.DebtThreshold != nil && *input.DebtThreshold > 0 {
		cfg.DebtThreshold = *input.DebtThreshold
	}
	if input.DebtSuccessDecay != nil && *input.DebtSuccessDecay >= 0 {
		cfg.DebtSuccessDecay = *input.DebtSuccessDecay
	}
	if input.DebtFail401 != nil && *input.DebtFail401 >= 0 {
		cfg.DebtFail401 = *input.DebtFail401
	}
	if input.DebtFail429 != nil && *input.DebtFail429 >= 0 {
		cfg.DebtFail429 = *input.DebtFail429
	}
	// Keep class weight map coherent with scalar defaults / overrides.
	cfg.DebtWeights = defaultDebtWeights()
	cfg.DebtWeights[classRateLimit] = cfg.DebtFail429
	for _, cname := range []string{classAuth, classPayment, classQuotaFree, classQuotaPaid, classPermission, classLegacy} {
		cfg.DebtWeights[cname] = cfg.DebtFail401
	}
	if input.StreakThreshold != nil && *input.StreakThreshold > 0 {
		cfg.StreakThreshold = *input.StreakThreshold
	}
	if input.OneShotClasses != nil {
		cfg.OneShotClasses = stringSet(input.OneShotClasses)
	}
	if len(input.CooldownHours) > 0 {
		steps := make([]time.Duration, 0, len(input.CooldownHours))
		for _, h := range input.CooldownHours {
			if h > 0 {
				steps = append(steps, time.Duration(h*float64(time.Hour)))
			}
		}
		if len(steps) > 0 {
			cfg.CooldownSteps = steps
		}
	}
	if input.HalfOpenEnabled != nil {
		cfg.HalfOpenEnabled = *input.HalfOpenEnabled
	}
	if input.HalfOpenSuccessThreshold != nil && *input.HalfOpenSuccessThreshold > 0 {
		cfg.HalfOpenSuccessThreshold = *input.HalfOpenSuccessThreshold
	}
	if input.TrialMaxHours != nil && *input.TrialMaxHours > 0 {
		cfg.TrialMaxDuration = time.Duration(*input.TrialMaxHours * float64(time.Hour))
	}
	return cfg, nil
}

func (c runtimeConfig) managementKey() string {
	if c.ManagementKey != "" {
		return c.ManagementKey
	}
	if c.ManagementKeyEnv == "" {
		return ""
	}
	return strings.TrimSpace(os.Getenv(c.ManagementKeyEnv))
}

func (c runtimeConfig) handlesStatus(status int) bool {
	_, ok := c.StatusCodes[status]
	return ok
}

func (c runtimeConfig) statusCodeList() []int {
	out := make([]int, 0, len(c.StatusCodes))
	for status := range c.StatusCodes {
		out = append(out, status)
	}
	sort.Ints(out)
	return out
}

func (c runtimeConfig) durationForClass(class string) time.Duration {
	class = strings.TrimSpace(class)
	if class != "" && c.ClassDisableHours != nil {
		if d, ok := c.ClassDisableHours[class]; ok && d > 0 {
			return d
		}
	}
	if c.DisableDuration > 0 {
		return c.DisableDuration
	}
	return defaultDisableHours * time.Hour
}

func (c runtimeConfig) classDeletable(class string) bool {
	if len(c.DeletableClasses) == 0 {
		return false
	}
	class = strings.TrimSpace(class)
	if class == "" {
		_, ok := c.DeletableClasses[classLegacy]
		return ok
	}
	_, ok := c.DeletableClasses[class]
	return ok
}

func (c runtimeConfig) deletableClassList() []string {
	out := make([]string, 0, len(c.DeletableClasses))
	for class := range c.DeletableClasses {
		out = append(out, class)
	}
	sort.Strings(out)
	return out
}

func statusCodeSet(statuses []int) map[int]struct{} {
	out := make(map[int]struct{}, len(statuses))
	for _, status := range statuses {
		if status >= 100 && status <= 599 {
			out[status] = struct{}{}
		}
	}
	return out
}

func stringSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(strings.ToLower(value))
		if value == "" {
			continue
		}
		out[value] = struct{}{}
	}
	return out
}
