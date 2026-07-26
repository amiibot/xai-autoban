package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct { void* ptr; size_t len; } cliproxy_buffer;
typedef struct { uint32_t abi_version; void* host_ctx; void* call; void* free_buffer; } cliproxy_host_api;
typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);
typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"xai-autoban/cpasdk/pluginabi"
	"xai-autoban/cpasdk/pluginapi"
)

const (
	pluginName    = "xai-autoban"
	pluginVersion = "1.5.1"
	providerXAI   = "xai"

	managementPrefix   = "/plugins/" + pluginName
	resourceBasePrefix = "/v0/resource/plugins/"
)

var (
	bans    = newBanState()
	autoban = newAutobanController(bans)
)

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(_ *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var rawRequest []byte
	if request != nil && requestLen > 0 {
		rawRequest = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, err := handleMethod(C.GoString(method), rawRequest)
	if err != nil {
		writeResponse(response, errorEnvelope("plugin_error", err.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() { autoban.shutdown() }

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var lifecycle lifecycleRequest
		if len(request) > 0 {
			if err := json.Unmarshal(request, &lifecycle); err != nil {
				return nil, err
			}
		}
		if err := autoban.configure(lifecycle.ConfigYAML); err != nil {
			return nil, err
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodUsageHandle:
		return handleUsage(request)
	case pluginabi.MethodSchedulerPick:
		return handleSchedulerPick(request)
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	UsagePlugin   bool `json:"usage_plugin"`
	Scheduler     bool `json:"scheduler"`
	ManagementAPI bool `json:"management_api"`
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "amiibot (fork of vrxiaojie)",
			GitHubRepository: "https://github.com/amiibot/xai-autoban",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "management-url", Type: pluginapi.ConfigFieldTypeString, Description: "CPA 地址，默认 http://127.0.0.1:8317。"},
				{Name: "management-key", Type: pluginapi.ConfigFieldTypeString, Description: "CPA Management Key；优先于环境变量。"},
				{Name: "management-key-env", Type: pluginapi.ConfigFieldTypeString, Description: "Management Key 环境变量名，默认 CPA_MANAGEMENT_KEY。"},
				{Name: "disable-hours", Type: pluginapi.ConfigFieldTypeInteger, Description: "冷却时长（小时），默认 24；也是 class 上限回退。"},
				{Name: "status-codes", Type: pluginapi.ConfigFieldTypeArray, Description: "失败即冷却的 HTTP 状态码，默认 401、402、403、429。"},
				{Name: "state-file", Type: pluginapi.ConfigFieldTypeString, Description: "状态文件，默认 xai-autoban-state.json。"},
				{Name: "classify-body", Type: pluginapi.ConfigFieldTypeString, Description: "是否解析失败 body 做细分类，默认 true。"},
				{Name: "max-auto-cycles", Type: pluginapi.ConfigFieldTypeInteger, Description: "连续冷却轮次上限，达到后进待清理且不再自动启用，默认 3。"},
				{Name: "cooldown-hours", Type: pluginapi.ConfigFieldTypeArray, Description: "可选按轮次阶梯小时；不配则固定 disable-hours。"},

				{Name: "observe-usage", Type: pluginapi.ConfigFieldTypeString, Description: "是否内嵌读取 CPAMP usage.sqlite 做 24h 用量观测，默认 true（不依赖 grok-quota）。"},
				{Name: "usage-db-path", Type: pluginapi.ConfigFieldTypeString, Description: "usage.sqlite 路径；空则自动探测。"},
				{Name: "auth-dir", Type: pluginapi.ConfigFieldTypeString, Description: "CPA auths 目录（可选，用于邮箱 enrich）。"},
				{Name: "join-quota-state", Type: pluginapi.ConfigFieldTypeString, Description: "sqlite 失败时是否回退读 grok-quota-state.json，默认 true。"},
				{Name: "quota-state-file", Type: pluginapi.ConfigFieldTypeString, Description: "可选外部 state JSON 路径（回退用）。"},
			},
		},
		Capabilities: registrationCapability{UsagePlugin: true, Scheduler: true, ManagementAPI: true},
	}
}

func handleUsage(raw []byte) ([]byte, error) {
	var record pluginapi.UsageRecord
	if len(raw) == 0 || json.Unmarshal(raw, &record) != nil {
		return okEnvelope(map[string]any{})
	}
	// Success events clear consecutive cool-down cycle ledger while healthy.
	if record.Provider != "" && !strings.EqualFold(record.Provider, providerXAI) {
		return okEnvelope(map[string]any{})
	}
	if record.AuthID == "" {
		return okEnvelope(map[string]any{})
	}

	autoban.handleUsage(record)
	return okEnvelope(map[string]any{})
}

func handleSchedulerPick(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	now := time.Now()
	available := make([]pluginapi.SchedulerAuthCandidate, 0, len(req.Candidates))
	for _, candidate := range req.Candidates {
		if strings.EqualFold(candidate.Provider, providerXAI) && bans.active(candidate.ID, now) {
			continue
		}
		available = append(available, candidate)
	}
	if len(available) == len(req.Candidates) || len(available) == 0 {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}
	chosen := available[0]
	for _, candidate := range available[1:] {
		if candidate.Priority > chosen.Priority {
			chosen = candidate
		}
	}
	return okEnvelope(pluginapi.SchedulerPickResponse{AuthID: chosen.ID, Handled: true})
}

func managementRegistration() pluginapi.ManagementRegistrationResponse {
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: managementPrefix + "/bans", Description: "List xAI credentials excluded by xai-autoban."},
			{Method: http.MethodPost, Path: managementPrefix + "/unban", Description: "Release one xAI credential. Body: {\"auth_id\":\"...\"}."},
			{Method: http.MethodPost, Path: managementPrefix + "/unban-all", Description: "Release all credentials held by xai-autoban."},
			{Method: http.MethodPost, Path: managementPrefix + "/delete", Description: "Permanently delete one 403 credential via Management API. Body: {\"auth_id\":\"...\"}."},
			{Method: http.MethodPost, Path: managementPrefix + "/delete-403", Description: "Permanently delete all currently tracked 403 credentials via Management API."},
			{Method: http.MethodPost, Path: managementPrefix + "/delete-classes", Description: "Delete by classes. Body: {\"classes\":[\"permission\"]}."},
			{Method: http.MethodPost, Path: managementPrefix + "/import", Description: "Restore a previously exported ban snapshot."},
			{Method: http.MethodGet, Path: managementPrefix + "/export", Description: "Export full analysis snapshot JSON (same as public /export)."},
		},
		Resources: []pluginapi.ResourceRoute{
			{Path: "/status", Menu: "xAI Autoban", Description: "View and release xAI credentials excluded after classified failures."},
			{Path: "/data", Description: "Public xAI autoban status data."},
			{Path: "/export", Description: "Download full analysis snapshot (JSON). Query format=csv for ban CSV."},
			{Path: "/action", Description: "Public xAI autoban actions."},
		},
	}
}

func handleManagement(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	return okEnvelope(dispatchManagement(req))
}

func dispatchManagement(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	switch {
	case method == http.MethodGet && strings.HasSuffix(strings.TrimRight(req.Path, "/"), managementPrefix+"/bans"):
		return jsonResponse(http.StatusOK, currentStatus())
	case method == http.MethodPost && strings.HasSuffix(strings.TrimRight(req.Path, "/"), managementPrefix+"/unban"):
		var body struct {
			AuthID string `json:"auth_id"`
		}
		_ = json.Unmarshal(req.Body, &body)
		if body.AuthID == "" {
			body.AuthID = req.Query.Get("auth_id")
		}
		if strings.TrimSpace(body.AuthID) == "" {
			return jsonResponse(http.StatusBadRequest, map[string]any{"error": "missing_auth_id"})
		}
		removed := autoban.requestRelease([]string{strings.TrimSpace(body.AuthID)})
		return jsonResponse(http.StatusOK, map[string]any{"ok": true, "removed": removed, "status": currentStatus()})
	case method == http.MethodPost && strings.HasSuffix(strings.TrimRight(req.Path, "/"), managementPrefix+"/unban-all"):
		return jsonResponse(http.StatusOK, map[string]any{"ok": true, "removed": autoban.requestReleaseAll(), "status": currentStatus()})
	case method == http.MethodPost && strings.HasSuffix(strings.TrimRight(req.Path, "/"), managementPrefix+"/delete"):
		var body struct {
			AuthID string `json:"auth_id"`
		}
		_ = json.Unmarshal(req.Body, &body)
		if body.AuthID == "" {
			body.AuthID = req.Query.Get("auth_id")
		}
		if strings.TrimSpace(body.AuthID) == "" {
			return jsonResponse(http.StatusBadRequest, map[string]any{"error": "missing_auth_id"})
		}
		deleted, err := autoban.deleteCredentials([]string{strings.TrimSpace(body.AuthID)}, 403)
		if err != nil && deleted == 0 {
			return jsonResponse(http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error(), "deleted": deleted, "status": currentStatus()})
		}
		return jsonResponse(http.StatusOK, map[string]any{"ok": true, "deleted": deleted, "status": currentStatus()})
	case method == http.MethodPost && strings.HasSuffix(strings.TrimRight(req.Path, "/"), managementPrefix+"/delete-403"):
		deleted, err := autoban.deleteByStatus(403)
		if err != nil && deleted == 0 {
			return jsonResponse(http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error(), "deleted": deleted, "status": currentStatus()})
		}
		return jsonResponse(http.StatusOK, map[string]any{"ok": true, "deleted": deleted, "status": currentStatus()})

	case method == http.MethodPost && strings.HasSuffix(strings.TrimRight(req.Path, "/"), managementPrefix+"/import"):
		return importSnapshot(req.Body)
	case method == http.MethodGet && matchesResourcePath(req.Path, "data"):
		return jsonResponse(http.StatusOK, currentStatus())
	case method == http.MethodGet && matchesResourcePath(req.Path, "export"):
		return handleExport(req)
	case method == http.MethodGet && strings.HasSuffix(strings.TrimRight(req.Path, "/"), managementPrefix+"/export"):
		return handleExport(req)
	case method == http.MethodGet && matchesResourcePath(req.Path, "action"):
		return publicAction(req)
	case method == http.MethodGet && (matchesResourcePath(req.Path, "status") || strings.HasSuffix(strings.TrimRight(req.Path, "/"), managementPrefix+"/status")):
		return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": {"text/html; charset=utf-8"}}, Body: []byte(statusPage())}
	default:
		return jsonResponse(http.StatusNotFound, map[string]any{"error": "not_found"})
	}
}

func matchesResourcePath(path, resource string) bool {
	cleanPath := strings.TrimRight(strings.TrimSpace(path), "/")
	if !strings.HasPrefix(cleanPath, resourceBasePrefix) {
		return false
	}
	remainder := strings.TrimPrefix(cleanPath, resourceBasePrefix)
	separator := strings.IndexByte(remainder, '/')
	return separator > 0 && remainder[separator+1:] == resource
}

func publicAction(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	op := strings.TrimSpace(req.Query.Get("op"))
	removed := 0
	deleted := 0
	switch op {
	case "unban":
		id := strings.TrimSpace(req.Query.Get("auth_id"))
		if id == "" {
			return jsonResponse(http.StatusBadRequest, map[string]any{"error": "missing_auth_id"})
		}
		removed = autoban.requestRelease([]string{id})
	case "unban-status":
		status, err := strconv.Atoi(req.Query.Get("status"))
		if err != nil {
			return jsonResponse(http.StatusBadRequest, map[string]any{"error": "invalid_status"})
		}
		removed = autoban.requestReleaseStatus(status)
	case "unban-many":
		ids := strings.Split(req.Query.Get("auth_ids"), ",")
		removed = autoban.requestRelease(ids)
	case "unban-all":
		removed = autoban.requestReleaseAll()
	case "delete":
		id := strings.TrimSpace(req.Query.Get("auth_id"))
		if id == "" {
			return jsonResponse(http.StatusBadRequest, map[string]any{"error": "missing_auth_id"})
		}
		var err error
		deleted, err = autoban.deleteCredentials([]string{id}, 403)
		if err != nil && deleted == 0 {
			slog.Error("xai-autoban: public delete action failed", "operation", op, "error", err)
			return jsonResponse(http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error(), "deleted": deleted, "status": currentStatus()})
		}
		slog.Warn("xai-autoban: public delete action", "operation", op, "deleted", deleted)
		return jsonResponse(http.StatusOK, map[string]any{"ok": true, "deleted": deleted, "status": currentStatus()})
	case "delete-many":
		rawIDs := strings.Split(req.Query.Get("auth_ids"), ",")
		var err error
		deleted, err = autoban.deleteCredentials(rawIDs, 403)
		if err != nil && deleted == 0 {
			slog.Error("xai-autoban: public delete action failed", "operation", op, "error", err)
			return jsonResponse(http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error(), "deleted": deleted, "status": currentStatus()})
		}
		slog.Warn("xai-autoban: public delete action", "operation", op, "deleted", deleted)
		return jsonResponse(http.StatusOK, map[string]any{"ok": true, "deleted": deleted, "status": currentStatus()})
	case "delete-403":
		var err error
		deleted, err = autoban.deleteByStatus(403)
		if err != nil && deleted == 0 {
			slog.Error("xai-autoban: public delete action failed", "operation", op, "error", err)
			return jsonResponse(http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error(), "deleted": deleted, "status": currentStatus()})
		}
		slog.Warn("xai-autoban: public delete action", "operation", op, "deleted", deleted)
		return jsonResponse(http.StatusOK, map[string]any{"ok": true, "deleted": deleted, "status": currentStatus()})

	default:
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "invalid_operation"})
	}
	slog.Warn("xai-autoban: public unban action", "operation", op, "removed", removed)
	return jsonResponse(http.StatusOK, map[string]any{"ok": true, "removed": removed, "status": currentStatus()})
}

type exportBundle struct {
	ExportVersion int                       `json:"export_version"`
	Plugin        string                    `json:"plugin"`
	Version       string                    `json:"version"`
	ExportedAt    string                    `json:"exported_at"`
	Status        statusInfo                `json:"status"`
	StateBans     map[string]banEntry       `json:"state_bans"`
	StateCycles   map[string]cycleLedger    `json:"state_cycles,omitempty"`
	StateEvidence map[string]evidenceLedger `json:"state_evidence,omitempty"` // legacy empty
	UsageAccounts []quotaAccountView        `json:"usage_accounts,omitempty"`
	Notes         []string                  `json:"notes,omitempty"`
}

func buildExportBundle() exportBundle {
	now := time.Now().UTC()
	status := currentStatus()
	bansMap, cyclesMap := bans.exportState()
	cfg := autoban.config()
	var usage []quotaAccountView
	if cfg.ObserveUsage || cfg.JoinQuotaState {
		qidx := loadQuotaUsage(cfg)
		if qidx.OK {
			// de-dupe by auth_index preferred
			seen := map[string]struct{}{}
			for _, v := range qidx.ByIndex {
				key := strings.TrimSpace(v.AuthIndex)
				if key == "" {
					key = strings.ToLower(strings.TrimSpace(v.Email))
				}
				if key == "" {
					continue
				}
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				usage = append(usage, v)
			}
			for email, v := range qidx.ByEmail {
				key := strings.TrimSpace(v.AuthIndex)
				if key == "" {
					key = strings.ToLower(strings.TrimSpace(email))
				}
				if key == "" {
					continue
				}
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				usage = append(usage, v)
			}
			sort.Slice(usage, func(i, j int) bool {
				if usage[i].AuthIndex == usage[j].AuthIndex {
					return usage[i].Email < usage[j].Email
				}
				return usage[i].AuthIndex < usage[j].AuthIndex
			})
		}
	}
	notes := []string{
		"status.bans: panel join view (isolation rows + usage fields)",
		"state_bans/state_cycles: raw plugin state for offline analysis",
		"usage_accounts: observed xAI accounts from usage.sqlite / quota state (not only banned)",
		"observe-only fields never trigger ban decisions",
	}
	return exportBundle{
		ExportVersion: 1,
		Plugin:        pluginName,
		Version:       pluginVersion,
		ExportedAt:    now.Format(time.RFC3339),
		Status:        status,
		StateBans:     bansMap,
		StateCycles:   cyclesMap,
		UsageAccounts: usage,
		Notes:         notes,
	}
}

func handleExport(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	format := strings.ToLower(strings.TrimSpace(req.Query.Get("format")))
	if format == "" {
		// allow /export.csv style via path suffix handled as resource name "export" only;
		// clients pass ?format=csv
		format = "json"
	}
	bundle := buildExportBundle()
	if format == "csv" || format == "bans.csv" {
		raw, err := bansCSV(bundle.Status.Bans)
		if err != nil {
			return jsonResponse(http.StatusInternalServerError, map[string]any{"error": "csv_failed", "message": err.Error()})
		}
		name := "xai-autoban-bans-" + time.Now().UTC().Format("20060102-150405") + ".csv"
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Content-Type":        {"text/csv; charset=utf-8"},
				"Content-Disposition": {"attachment; filename=\"" + name + "\""},
			},
			Body: raw,
		}
	}
	// default JSON (pretty for local analysis)
	raw, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return jsonResponse(http.StatusInternalServerError, map[string]any{"error": "marshal_failed", "message": err.Error()})
	}
	name := "xai-autoban-export-" + time.Now().UTC().Format("20060102-150405") + ".json"
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":        {"application/json; charset=utf-8"},
			"Content-Disposition": {"attachment; filename=\"" + name + "\""},
		},
		Body: raw,
	}
}

func bansCSV(items []banInfo) ([]byte, error) {
	var buf bytes.Buffer
	// UTF-8 BOM helps Excel open Chinese headers correctly.
	buf.Write([]byte{0xEF, 0xBB, 0xBF})
	w := csv.NewWriter(&buf)
	header := []string{
		"auth_id", "auth_index", "status_code", "class", "reason", "phase", "cycle",
		"banned_at", "reset_at", "remaining_seconds", "unusable_since", "offline_hours",
		"last_used", "tokens_24h", "tokens_total", "quota_limit", "quota_email", "quota_health",
		"management_disabled", "last_error", "deletable",
	}
	if err := w.Write(header); err != nil {
		return nil, err
	}
	// stable order
	sorted := append([]banInfo(nil), items...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].OfflineHours != sorted[j].OfflineHours {
			return sorted[i].OfflineHours > sorted[j].OfflineHours
		}
		return sorted[i].AuthID < sorted[j].AuthID
	})
	for _, b := range sorted {
		row := []string{
			b.AuthID,
			b.AuthIndex,
			strconv.Itoa(b.StatusCode),
			b.Class,
			b.Reason,
			b.Phase,
			strconv.Itoa(b.Cycle),
			b.BannedAt,
			b.ResetAt,
			strconv.FormatInt(b.RemainingSeconds, 10),
			b.UnusableSince,
			strconv.FormatInt(b.OfflineHours, 10),
			b.LastUsed,
			strconv.FormatInt(b.Tokens24h, 10),
			strconv.FormatInt(b.TokensTotal, 10),
			strconv.FormatInt(b.QuotaLimit, 10),
			b.QuotaEmail,
			b.QuotaHealth,
			strconv.FormatBool(b.ManagementDisabled),
			b.LastError,
			strconv.FormatBool(b.Deletable),
		}
		if err := w.Write(row); err != nil {
			return nil, err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func importSnapshot(raw []byte) pluginapi.ManagementResponse {
	var snapshot statusInfo
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "invalid_snapshot", "message": err.Error()})
	}
	now := time.Now()
	imported := 0
	for _, item := range snapshot.Bans {
		resetAt, errReset := time.Parse(time.RFC3339, item.ResetAt)
		if errReset != nil || !resetAt.After(now) || strings.TrimSpace(item.AuthID) == "" {
			continue
		}
		bannedAt, errBanned := time.Parse(time.RFC3339, item.BannedAt)
		if errBanned != nil {
			bannedAt = now
		}
		bans.set(item.AuthID, banEntry{AuthIndex: item.AuthIndex, StatusCode: item.StatusCode, Class: item.Class, Reason: item.Reason, BodyFingerprint: item.BodyFingerprint, BannedAt: bannedAt, ResetAt: resetAt, ManagementDisabled: item.ManagementDisabled})
		imported++
	}
	if imported > 0 {
		autoban.signal()
	}
	return jsonResponse(http.StatusOK, map[string]any{"ok": true, "imported": imported, "status": currentStatus()})
}

type statusInfo struct {
	Plugin  string `json:"plugin"`
	Version string `json:"version"`
	Count   int    `json:"count"`
	// PoolTotal is xAI credentials known to the pie (banned + normal when available).
	PoolTotal int `json:"pool_total"`
	// NormalCount is currently not banned (and not disabled, when known).
	NormalCount      int      `json:"normal_count"`
	DeletableClasses []string `json:"deletable_classes"`
	// MaxAutoCycles is cool-down rounds before pending cleanup (panel label).
	MaxAutoCycles int                  `json:"max_auto_cycles"`
	Charts        chartBundle          `json:"charts"`
	QuotaJoin     quotaJoinInfo        `json:"quota_join"`
	Management    managementStatusInfo `json:"management"`
	Bans          []banInfo            `json:"bans"`
}

type chartBundle struct {
	ByStatus []chartSlice `json:"by_status"`
	ByClass  []chartSlice `json:"by_class"`
	// IncludesNormal documents whether "ok/normal" slices were estimated from pool.
	IncludesNormal bool   `json:"includes_normal"`
	PoolSource     string `json:"pool_source,omitempty"` // management | quota_state | banned_only
}

type chartSlice struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Count int    `json:"count"`
}

type quotaJoinInfo struct {
	Enabled bool   `json:"enabled"`
	OK      bool   `json:"ok"`
	Path    string `json:"path,omitempty"`
	Source  string `json:"source,omitempty"` // usage_sqlite | state_file
	Error   string `json:"error,omitempty"`
	// Matched is how many ban rows found a quota record.
	Matched int `json:"matched"`
	// PoolAccounts is number of accounts in the usage pool (for pie base).
	PoolAccounts int `json:"pool_accounts,omitempty"`
}

type managementStatusInfo struct {
	URL          string `json:"url"`
	LastError    string `json:"last_error,omitempty"`
	BlockedUntil string `json:"blocked_until,omitempty"`
}

type banInfo struct {
	AuthID             string `json:"auth_id"`
	AuthIndex          string `json:"auth_index,omitempty"`
	StatusCode         int    `json:"status_code"`
	Class              string `json:"class,omitempty"`
	Reason             string `json:"reason"`
	BodyFingerprint    string `json:"body_fingerprint,omitempty"`
	BannedAt           string `json:"banned_at"`
	ResetAt            string `json:"reset_at"`
	RemainingSeconds   int64  `json:"remaining_seconds"`
	ManagementDisabled bool   `json:"management_disabled"`
	Deletable          bool   `json:"deletable"`
	LastError          string `json:"last_error,omitempty"`
	Phase              string `json:"phase,omitempty"` // cooling | pending
	Cycle              int    `json:"cycle,omitempty"`
	// Optional usage join (observe only; never pre-ban).
	Tokens24h     int64  `json:"tokens_24h"`
	TokensTotal   int64  `json:"tokens_total,omitempty"` // lifetime successful tokens
	QuotaLimit    int64  `json:"quota_limit,omitempty"`
	QuotaHealth   string `json:"quota_health,omitempty"`
	QuotaEmail    string `json:"quota_email,omitempty"`
	OverRef       bool   `json:"over_reference,omitempty"`
	LastUsed      string `json:"last_used,omitempty"`
	UnusableSince string `json:"unusable_since,omitempty"`
	OfflineHours  int64  `json:"offline_hours"`
}

const (
	// chart keys for non-HTTP / healthy accounts
	chartKeyOK       = "ok"
	chartKeyDisabled = "disabled"
	classOK          = "ok"
)

func currentStatus() statusInfo {
	now := time.Now()
	snapshot := bans.snapshot(now)
	cfg := autoban.config()

	var qidx quotaJoinIndex
	qinfo := quotaJoinInfo{Enabled: cfg.ObserveUsage || cfg.JoinQuotaState}
	if qinfo.Enabled {
		qidx = loadQuotaUsage(cfg)
		qinfo.OK = qidx.OK
		qinfo.Path = qidx.Path
		qinfo.Source = qidx.Source
		qinfo.Error = qidx.Error
		qinfo.PoolAccounts = qidx.accountCount()
	}

	statusCounts := map[int]int{}
	classCounts := map[string]int{}
	items := make([]banInfo, 0, len(snapshot))
	matched := 0
	bannedKeys := map[string]struct{}{}
	for id, entry := range snapshot {
		class := entry.Class
		if class == "" {
			class = classLegacy
		}
		statusCounts[entry.StatusCode]++
		classCounts[class]++
		bannedKeys[id] = struct{}{}
		if entry.AuthIndex != "" {
			bannedKeys[entry.AuthIndex] = struct{}{}
		}
		unusableSince := entry.UnusableSince
		if unusableSince.IsZero() {
			unusableSince = entry.BannedAt
		}
		offlineHours := int64(0)
		if !unusableSince.IsZero() {
			h := int64(now.Sub(unusableSince).Hours())
			if h < 0 {
				h = 0
			}
			offlineHours = h
		}
		remaining := int64(0)
		if !entry.ResetAt.IsZero() {
			remaining = int64(entry.ResetAt.Sub(now).Seconds())
			if remaining < 0 {
				remaining = 0
			}
		}
		info := banInfo{
			AuthID: id, AuthIndex: entry.AuthIndex, StatusCode: entry.StatusCode, Class: class, Reason: entry.Reason,
			BodyFingerprint:    entry.BodyFingerprint,
			RemainingSeconds:   remaining,
			ManagementDisabled: entry.ManagementDisabled, Deletable: entry.StatusCode == 403, LastError: entry.LastError,
			Phase: normalizePhase(entry.Phase), Cycle: entry.Cycle, OfflineHours: offlineHours,
		}
		if !entry.BannedAt.IsZero() {
			info.BannedAt = entry.BannedAt.Format(time.RFC3339)
		}
		if !entry.ResetAt.IsZero() {
			info.ResetAt = entry.ResetAt.Format(time.RFC3339)
		}
		if !unusableSince.IsZero() {
			info.UnusableSince = unusableSince.Format(time.RFC3339)
		}
		if qidx.OK {
			if q, ok := qidx.lookup(id, entry.AuthIndex, ""); ok {
				matched++
				info.Tokens24h = q.Tokens24h
				if info.Tokens24h == 0 {
					info.Tokens24h = q.QuotaUsed
				}
				info.TokensTotal = q.TokensTotal
				info.QuotaLimit = q.QuotaLimit
				info.QuotaHealth = firstNonEmpty(q.QuotaHealth, q.StatusKind)
				info.QuotaEmail = q.Email
				info.OverRef = q.OverRef
				if !q.LastUsed.IsZero() {
					info.LastUsed = q.LastUsed.Format(time.RFC3339)
				}
			}
		}
		items = append(items, info)
	}
	qinfo.Matched = matched
	sort.Slice(items, func(i, j int) bool { return items[i].ResetAt < items[j].ResetAt })

	// Pool size + normal accounts for pies (observe only).
	poolTotal, normalCount, disabledCount, poolSource := estimatePool(cfg, snapshot, bannedKeys, qidx)

	// Inject "ok" / disabled into charts when we know the pool.
	statusSlices := buildStatusChart(statusCounts)
	classSlices := buildClassChart(classCounts)
	if poolTotal > len(items) || normalCount > 0 {
		if normalCount > 0 {
			statusSlices = prependChartSlice(statusSlices, chartSlice{Key: chartKeyOK, Label: "正常", Count: normalCount})
			classSlices = prependChartSlice(classSlices, chartSlice{Key: classOK, Label: "正常", Count: normalCount})
		}
		if disabledCount > 0 {
			statusSlices = append(statusSlices, chartSlice{Key: chartKeyDisabled, Label: "已禁用(非隔离)", Count: disabledCount})
		}
	}
	charts := chartBundle{
		ByStatus:       statusSlices,
		ByClass:        classSlices,
		IncludesNormal: normalCount > 0 || poolSource != "banned_only",
		PoolSource:     poolSource,
	}

	controller := autoban.status()
	management := managementStatusInfo{URL: controller.ManagementURL, LastError: controller.LastError}
	if !controller.BlockedUntil.IsZero() {
		management.BlockedUntil = controller.BlockedUntil.Format(time.RFC3339)
	}
	if poolTotal < len(items) {
		poolTotal = len(items)
	}
	return statusInfo{
		Plugin: pluginName, Version: pluginVersion, Count: len(items),
		PoolTotal: poolTotal, NormalCount: normalCount,
		DeletableClasses: []string{}, MaxAutoCycles: cfg.maxAutoCycles(),
		Charts: charts, QuotaJoin: qinfo,
		Management: management, Bans: items,
	}
}

// estimatePool returns total xAI credentials, normal (enabled & not banned), and
// disabled-but-not-banned counts for pie charts. Prefer Management API list;
// fall back to usage observation pool size; else banned-only.
func estimatePool(cfg runtimeConfig, snapshot map[string]banEntry, bannedKeys map[string]struct{}, qidx quotaJoinIndex) (poolTotal, normal, disabledExtra int, source string) {
	bannedN := len(snapshot)
	// 1) Management API — authoritative pool of xAI auth files.
	client := autoban.clientSnapshot()
	if client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		files, err := client.listXAIAuthFiles(ctx)
		cancel()
		if err == nil && len(files) > 0 {
			source = "management"
			poolTotal = len(files)
			for _, f := range files {
				if isBannedAuth(bannedKeys, f.ID, f.AuthIndex) {
					continue
				}
				if f.Disabled {
					disabledExtra++
					continue
				}
				normal++
			}
			return poolTotal, normal, disabledExtra, source
		}
	}
	// 2) Usage observation account count (sqlite / state file).
	if qidx.OK {
		n := qidx.accountCount()
		if n > 0 {
			if qidx.Source == "usage_sqlite" {
				source = "usage_sqlite"
			} else {
				source = "quota_state"
			}
			poolTotal = n
			// Normal ≈ pool - currently banned (cannot know disabled without management).
			normal = n - bannedN
			if normal < 0 {
				normal = 0
			}
			return poolTotal, normal, 0, source
		}
	}
	// 3) Banned only — pie has no "ok" slice.
	return bannedN, 0, 0, "banned_only"
}

func isBannedAuth(bannedKeys map[string]struct{}, id, authIndex string) bool {
	if _, ok := bannedKeys[strings.TrimSpace(id)]; ok {
		return true
	}
	if authIndex != "" {
		if _, ok := bannedKeys[strings.TrimSpace(authIndex)]; ok {
			return true
		}
	}
	return false
}

func prependChartSlice(slices []chartSlice, head chartSlice) []chartSlice {
	if head.Count <= 0 {
		return slices
	}
	out := make([]chartSlice, 0, len(slices)+1)
	out = append(out, head)
	out = append(out, slices...)
	return out
}

func buildStatusChart(counts map[int]int) []chartSlice {
	order := []int{401, 402, 403, 429}
	labels := map[int]string{401: "401 未授权", 402: "402 付费/额度", 403: "403 禁止", 429: "429 限流"}
	out := make([]chartSlice, 0, len(order)+len(counts))
	seen := map[int]bool{}
	for _, code := range order {
		seen[code] = true
		if n := counts[code]; n > 0 {
			out = append(out, chartSlice{Key: fmt.Sprintf("%d", code), Label: labels[code], Count: n})
		}
	}
	// Any other status codes
	extras := make([]int, 0)
	for code := range counts {
		if !seen[code] {
			extras = append(extras, code)
		}
	}
	sort.Ints(extras)
	for _, code := range extras {
		out = append(out, chartSlice{Key: fmt.Sprintf("%d", code), Label: fmt.Sprintf("HTTP %d", code), Count: counts[code]})
	}
	return out
}

func buildClassChart(counts map[string]int) []chartSlice {
	order := []string{
		classAuth, classPayment, classQuotaPaid, classQuotaFree,
		classPermission, classRateLimit, classForbiddenUnknown, classOther, classLegacy,
	}
	labels := map[string]string{
		classAuth: "auth", classPayment: "payment", classQuotaPaid: "quota_paid",
		classQuotaFree: "quota_free", classPermission: "permission", classRateLimit: "rate_limit",
		classForbiddenUnknown: "forbidden_unknown", classOther: "other", classLegacy: "legacy",
	}
	out := make([]chartSlice, 0, len(order))
	seen := map[string]bool{}
	for _, c := range order {
		seen[c] = true
		if n := counts[c]; n > 0 {
			out = append(out, chartSlice{Key: c, Label: labels[c], Count: n})
		}
	}
	extras := make([]string, 0)
	for c := range counts {
		if !seen[c] {
			extras = append(extras, c)
		}
	}
	sort.Strings(extras)
	for _, c := range extras {
		out = append(out, chartSlice{Key: c, Label: c, Count: counts[c]})
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func statusPage() string {
	name := html.EscapeString(pluginName)
	return `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>` + name + `</title>
  <style>
    :root{color-scheme:light;--bg:#f5f7f9;--surface:#fff;--text:#17202a;--muted:#66727f;--line:#dce2e8;--red:#b42318;--red-bg:#fff1f0;--amber:#9a6700;--amber-bg:#fff8db;--blue:#175cd3;--blue-bg:#eff6ff;--green:#067647;--green-bg:#ecfdf3;--shadow:0 1px 2px rgba(16,24,40,.06)}
    *{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font-family:Inter,ui-sans-serif,system-ui,-apple-system,"Segoe UI",sans-serif;font-size:14px;letter-spacing:0}
    header{background:#182230;color:#fff;border-bottom:1px solid #253347}.header-inner{max-width:1440px;margin:auto;padding:18px 24px;display:flex;align-items:center;justify-content:space-between;gap:20px}.brand h1{font-size:21px;line-height:1.2;margin:0}.brand p{margin:5px 0 0;color:#b9c3cf;font-size:13px}.live{display:flex;align-items:center;gap:8px;color:#d5f5e8;font-size:13px}.live-dot{width:8px;height:8px;border-radius:50%;background:#32d583;box-shadow:0 0 0 3px rgba(50,213,131,.18)}
    .charts{display:grid;grid-template-columns:1fr 1fr;gap:14px;margin-bottom:16px}
    .chart-card{background:var(--surface);border:1px solid var(--line);border-radius:7px;box-shadow:var(--shadow);padding:16px}
    .chart-card h2{margin:0 0 4px;font-size:14px}.chart-sub{color:var(--muted);font-size:12px;margin:0 0 12px}
    .chart-body{display:flex;align-items:center;gap:18px;flex-wrap:wrap}
    .pie{width:148px;height:148px;border-radius:50%;background:#e8eef3;flex:0 0 auto}
    .pie.empty{display:flex;align-items:center;justify-content:center;color:var(--muted);font-size:12px;font-weight:600}
    .legend{flex:1;min-width:180px;display:flex;flex-direction:column;gap:6px}
    .legend-row{display:flex;align-items:center;gap:8px;font-size:13px}
    .swatch{width:10px;height:10px;border-radius:2px;flex:0 0 auto}
    .legend-label{flex:1;color:#344054}.legend-count{font-variant-numeric:tabular-nums;font-weight:700}
    .quota-note{font-size:12px;color:var(--muted);margin:0 0 12px}
    .quota-note.ok{color:var(--green)}.quota-note.warn{color:var(--amber)}
    @media(max-width:860px){.charts{grid-template-columns:1fr}}
    main{max-width:1440px;margin:auto;padding:20px 24px 36px}.stats{display:grid;grid-template-columns:repeat(5,minmax(140px,1fr));gap:12px;margin-bottom:16px}.stat{background:var(--surface);border:1px solid var(--line);border-radius:7px;padding:16px;box-shadow:var(--shadow)}.stat-label{font-size:12px;color:var(--muted);font-weight:600}.stat-value{font-size:28px;line-height:1.1;font-weight:750;margin-top:7px}.stat-total{border-left:4px solid var(--blue)}.stat-401{border-left:4px solid #1570ef}.stat-402{border-left:4px solid #f79009}.stat-403{border-left:4px solid #d92d20}.stat-429{border-left:4px solid #7f56d9}
    .toolbar{background:var(--surface);border:1px solid var(--line);border-radius:7px;box-shadow:var(--shadow);margin-bottom:14px}.toolbar-row{display:flex;align-items:center;gap:10px;padding:12px;flex-wrap:wrap}.toolbar-row+.toolbar-row{border-top:1px solid var(--line)}input[type=search]{height:36px;min-width:260px;flex:1;border:1px solid #bfc8d2;border-radius:6px;padding:0 11px;background:#fff;color:var(--text);font-size:14px}.segments{display:flex;border:1px solid #bfc8d2;border-radius:6px;overflow:hidden}.segments button{border:0;border-right:1px solid #bfc8d2;border-radius:0;background:#fff;color:#344054}.segments button:last-child{border-right:0}.segments button.active{background:#e8eef6;color:#101828;font-weight:700}
    button{height:36px;border:1px solid #bfc8d2;border-radius:6px;background:#fff;color:#273240;padding:0 12px;font:inherit;font-weight:600;cursor:pointer;white-space:nowrap}button:hover{background:#f2f4f7}button:disabled{opacity:.45;cursor:not-allowed}.primary{background:#175cd3;color:#fff;border-color:#175cd3}.primary:hover{background:#164ca7}.danger{color:#b42318;border-color:#f1a39b;background:#fff}.danger:hover{background:var(--red-bg)}.quiet-danger{color:#b42318}.spacer{flex:1}.auto{display:flex;align-items:center;gap:7px;color:var(--muted);white-space:nowrap}.auto input{width:16px;height:16px}.message{min-height:20px;color:var(--muted);font-size:13px}.message.error{color:var(--red)}
    .table-shell{background:var(--surface);border:1px solid var(--line);border-radius:7px;box-shadow:var(--shadow);overflow:hidden}.table-head{padding:11px 14px;border-bottom:1px solid var(--line);display:flex;align-items:center;gap:12px}.table-head strong{font-size:14px}.table-head span{color:var(--muted);font-size:13px}.table-wrap{overflow:auto;max-height:64vh}table{border-collapse:collapse;width:100%;min-width:1280px}th,td{padding:10px 12px;text-align:left;border-bottom:1px solid #edf0f3;vertical-align:middle}th{position:sticky;top:0;background:#f8fafb;color:#475467;font-size:12px;font-weight:700;z-index:1}th.sortable{cursor:pointer;user-select:none}th.sortable:hover{color:#101828}th.sortable.sorted-asc::after{content:" \25B2";font-size:10px;opacity:.7}th.sortable.sorted-desc::after{content:" \25BC";font-size:10px;opacity:.7}tbody tr:hover{background:#f9fbfc}td code{font-family:"SFMono-Regular",Consolas,monospace;font-size:12px;color:#344054}.check{width:38px;text-align:center}.badge{display:inline-flex;align-items:center;justify-content:center;min-width:45px;height:24px;border-radius:12px;font-weight:750;font-size:12px}.b401{color:#175cd3;background:#eff8ff}.b402{color:var(--amber);background:var(--amber-bg)}.b403{color:var(--red);background:var(--red-bg)}.b429{color:#6941c6;background:#f4f3ff}.reason{color:#475467}.time{white-space:nowrap}.remaining{font-variant-numeric:tabular-nums;font-weight:650}.management-ok{color:var(--green);font-weight:650}.management-pending{color:var(--amber);font-weight:650}.management-error{color:var(--red);font-weight:650}.row-action{height:30px;padding:0 9px;font-size:12px}.actions{display:flex;gap:6px;align-items:center;flex-wrap:wrap}.empty{padding:52px;text-align:center;color:var(--muted)}
    .tip{position:relative;cursor:help;border-bottom:1px dotted rgba(102,114,127,.45)}
    .tip[data-tip]:hover::after,.tip[data-tip]:focus::after{content:attr(data-tip);position:absolute;z-index:20;left:0;bottom:calc(100% + 8px);min-width:180px;max-width:320px;padding:8px 10px;border-radius:6px;background:#182230;color:#fff;font-size:12px;font-weight:500;line-height:1.45;white-space:pre-wrap;box-shadow:0 8px 24px rgba(16,24,40,.18);pointer-events:none}
    .tip[data-tip]:hover::before,.tip[data-tip]:focus::before{content:"";position:absolute;z-index:20;left:12px;bottom:calc(100% + 2px);border:6px solid transparent;border-top-color:#182230;pointer-events:none}
    th.tip{border-bottom-color:transparent}
    .pager{display:flex;align-items:center;justify-content:space-between;gap:12px;padding:11px 14px}.pager-info{color:var(--muted);font-size:13px}.pager-buttons{display:flex;align-items:center;gap:7px}.pager button{height:32px}.page-number{min-width:72px;text-align:center;font-variant-numeric:tabular-nums}.footer-note{color:#7b8794;font-size:12px;margin:12px 2px 0}
    @media(max-width:860px){.header-inner,main{padding-left:14px;padding-right:14px}.stats{grid-template-columns:repeat(2,minmax(130px,1fr))}.toolbar-row{align-items:stretch}input[type=search]{min-width:100%;order:-1}.segments{width:100%}.segments button{flex:1}.spacer{display:none}.table-wrap{max-height:58vh}}
    @media(max-width:480px){.stats{grid-template-columns:1fr 1fr}.stat{padding:13px}.stat-value{font-size:23px}.brand p{display:none}.toolbar-row button{flex:1}.segments button{padding:0 7px}.auto{width:100%}}
  </style>
</head>
<body>
  <header><div class="header-inner"><div class="brand"><h1>xAI Autoban</h1><p>隔离 + 观测（用量只读） · v` + pluginVersion + `</p></div><div class="live"><span class="live-dot"></span><span id="syncState">正在连接</span></div></div></header>
  <main>
    <section class="stats" aria-label="隔离统计">
      <div class="stat stat-total"><div class="stat-label">当前隔离</div><div class="stat-value" id="total">-</div></div>
      <div class="stat stat-total" style="border-left-color:#12b76a"><div class="stat-label">正常(估)</div><div class="stat-value" id="normalCount">-</div></div>
      <div class="stat stat-total" style="border-left-color:#667085"><div class="stat-label">池规模</div><div class="stat-value" id="poolTotal">-</div></div>
      <div class="stat stat-401"><div class="stat-label">401 未授权</div><div class="stat-value" id="count401">-</div></div>
      <div class="stat stat-402"><div class="stat-label">402 无额度</div><div class="stat-value" id="count402">-</div></div>
      <div class="stat stat-403"><div class="stat-label">403 禁止访问</div><div class="stat-value" id="count403">-</div></div>
      <div class="stat stat-429"><div class="stat-label">429 限流</div><div class="stat-value" id="count429">-</div></div>
    </section>
    <section class="charts" aria-label="status charts">
      <div class="chart-card">
        <h2>HTTP 状态分布</h2>
        <p class="chart-sub">账号池占比：正常 + 401/402/403/429（能拿到池规模时含正常号）</p>
        <div class="chart-body"><div id="pieStatus" class="pie empty">无数据</div><div id="legendStatus" class="legend"></div></div>
      </div>
      <div class="chart-card">
        <h2>失败类型分布</h2>
        <p class="chart-sub">账号池占比：正常 + 失败类型（permission / quota_free / …）</p>
        <div class="chart-body"><div id="pieClass" class="pie empty">无数据</div><div id="legendClass" class="legend"></div></div>
      </div>
    </section>
    <p id="quotaNote" class="quota-note">用量观测：初始化中…</p>

    <section class="toolbar">
      <div class="toolbar-row">
        <input id="search" type="search" placeholder="搜索 Auth ID 或原因" autocomplete="off">
        <div class="segments" id="filters">
          <button data-status="all" class="active">全部</button><button data-status="401">401</button><button data-status="402">402</button><button data-status="403">403</button><button data-status="429">429</button>
        </div>
        <button class="primary" onclick="loadData()">刷新</button>
        <label class="auto"><input id="autoRefresh" type="checkbox" checked>30 秒自动刷新</label>
      </div>
      <div class="toolbar-row">
        <button id="unbanSelected" onclick="unbanSelected()" disabled>解禁已选</button>
        <button onclick="copyVisible()">复制当前 ID</button>
        <button class="tip" data-tip="下载完整 JSON：隔离状态 + 原始 state + 用量观测账号，便于本地分析" onclick="exportJSON()">导出 JSON</button>
        <button class="tip" data-tip="下载当前隔离行 CSV（Excel 可直接打开）" onclick="exportCSV()">导出 CSV</button>
        <span class="spacer"></span>
        <button class="quiet-danger" onclick="unbanStatus(401)">清除全部 401</button>
        <button class="quiet-danger" onclick="unbanStatus(402)">清除全部 402</button>
        <button class="quiet-danger" onclick="unbanStatus(403)">清除全部 403</button>
        <button class="danger" onclick="deleteSelected403()">删除已选 403</button>
        <button class="danger" onclick="deleteAll403()">删除全部 403 账号</button>
        <button class="quiet-danger" onclick="unbanStatus(429)">清除全部 429</button>
        <button class="danger" onclick="unbanAll()">全部解禁</button>
      </div>
      <div class="toolbar-row"><div id="message" class="message">准备加载数据</div></div>
    </section>

    <section class="table-shell">
      <div class="table-head"><strong>隔离凭据</strong><span id="resultCount">0 条</span></div>
      <div class="table-wrap">
        <table><thead><tr><th class="check"><input id="selectPage" type="checkbox" title="选择当前页"></th><th class="sortable" data-sort="auth_id">Auth ID</th><th class="sortable tip" data-sort="status_code" data-tip="触发冷却的 HTTP 状态码">状态码</th><th class="sortable tip" data-sort="class" data-tip="失败 body 粗分类 / class TTL">失败类型</th><th class="sortable tip" data-sort="tokens_total" data-tip="近24h / 参考上限 · Σ累计成功 token（只读）">用量 24h·累计</th><th>原因</th><th class="sortable tip" data-sort="phase" data-tip="冷却中 / 待清理 / 解禁进度">当前处置</th><th class="sortable tip" data-sort="last_used" data-tip="最近一次成功请求">最后使用</th><th class="sortable tip" data-sort="offline_hours" data-tip="连续不可用整小时">已下线(h)</th><th class="sortable tip" data-sort="remaining_seconds" data-tip="本轮冷却剩余">剩余时间</th><th class="sortable tip" data-sort="cycle" data-tip="冷却轮次">轮次</th><th>操作</th></tr></thead><tbody id="rows"></tbody></table>
        <div id="empty" class="empty" hidden>当前筛选条件下没有隔离凭据</div>
      </div>
      <div class="pager"><div class="pager-info" id="range">0-0 / 0</div><div class="pager-buttons"><button id="prev" onclick="changePage(-1)">上一页</button><span class="page-number" id="pageNumber">1 / 1</span><button id="next" onclick="changePage(1)">下一页</button></div></div>
    </section>
    <p class="footer-note">此页面无需管理密钥。硬隔离有期限（阶梯冷却 + 半开试用），不是永 ban。三态：冷却中 → 到期自动启用 → 再失败再冷却；达 max-auto-cycles 进「待清理」（不再自动启用）。「用量」含 24h 与累计 Σ。「已下线(h)」自首次冷却起算。表头可排序。永久删除仅 HTTP 403。</p>
  </main>
  <script>
    const base=window.location.pathname.replace(/\/status\/?$/,'');
    const state={bans:[],filter:'all',query:'',page:1,pageSize:50,selected:new Set(),timer:null,max_auto_cycles:2};
    const $=id=>document.getElementById(id);
    const esc=value=>String(value??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));

    async function api(path){const response=await fetch(base+path,{cache:'no-store'});const text=await response.text();let data;try{data=JSON.parse(text)}catch(_){throw new Error(text||('HTTP '+response.status))}if(!response.ok)throw new Error(data.error||('HTTP '+response.status));return data}
    function setMessage(text,error=false){$('message').textContent=text;$('message').className='message'+(error?' error':'')}
    function counts(){const out={401:0,402:0,403:0,429:0};for(const ban of state.bans)if(out[ban.status_code]!==undefined)out[ban.status_code]++;return out}
    function filtered(){const q=state.query.toLowerCase();let list=state.bans.filter(ban=>(state.filter==='all'||String(ban.status_code)===state.filter)&&(!q||ban.auth_id.toLowerCase().includes(q)||(ban.reason||'').toLowerCase().includes(q)||(ban.class||'').toLowerCase().includes(q)||(ban.phase||'').toLowerCase().includes(q)));const key=state.sortKey||'offline_hours';const dir=state.sortDir==='asc'?1:-1;const numKeys={status_code:1,tokens_24h:1,tokens_total:1,offline_hours:1,remaining_seconds:1,cycle:1};list=list.slice().sort((a,b)=>{let va=a[key],vb=b[key];if(key==='tokens_total'){va=Number(a.tokens_total||0);vb=Number(b.tokens_total||0)}else if(numKeys[key]){va=Number(va||0);vb=Number(vb||0)}else{va=String(va??'');vb=String(vb??'')}if(va<vb)return -1*dir;if(va>vb)return 1*dir;return String(a.auth_id).localeCompare(String(b.auth_id))});return list}
    function setSort(key){if(state.sortKey===key){state.sortDir=state.sortDir==='asc'?'desc':'asc'}else{state.sortKey=key;state.sortDir=(key==='auth_id'||key==='class'||key==='phase'||key==='last_used')?'asc':'desc'}state.page=1;document.querySelectorAll('th.sortable').forEach(th=>{th.classList.remove('sorted-asc','sorted-desc');if(th.dataset.sort===state.sortKey)th.classList.add(state.sortDir==='asc'?'sorted-asc':'sorted-desc')});render()}
    function formatDate(value){const d=new Date(value);return Number.isNaN(d.getTime())?value:d.toLocaleString('zh-CN',{hour12:false})}
    function formatRemaining(seconds){seconds=Math.max(0,Number(seconds||0));const d=Math.floor(seconds/86400),h=Math.floor(seconds%86400/3600),m=Math.floor(seconds%3600/60);if(d)return d+'天 '+h+'小时';if(h)return h+'小时 '+m+'分';return m+'分钟'}
    function formatLastUsed(value){if(!value)return '—';const d=new Date(value);return Number.isNaN(d.getTime())?String(value):d.toLocaleString('zh-CN',{hour12:false})}
    function formatOfflineHours(h){h=Number(h);if(!Number.isFinite(h)||h<0)return '—';return String(Math.floor(h))}
    function tipAttr(text){const s=String(text??'').trim();return s?' class="tip" data-tip="'+esc(s)+'"':'';}
    function lastUsedTip(ban){if(!ban||!ban.last_used)return 'usage.sqlite 中暂无成功请求记录（可能从未成功，或库未接入）。';return '最近一次成功请求：'+formatLastUsed(ban.last_used)+'\n来源：CPAMP usage.sqlite（只读观测）';}
    function offlineTip(ban){const h=formatOfflineHours(ban.offline_hours);const since=ban.unusable_since||ban.banned_at;if(!since&&h==='—')return '尚无连续不可用起点。';return '已下线 '+h+' 小时\n起点：'+(since?formatDate(since):'—')+'\n含义：本轮首次隔离时间；阶梯再隔离/试用失败不重置。';}
    function remainTip(ban){return '本轮冷却剩余：'+formatRemaining(ban.remaining_seconds)+'\n计划解禁：'+(ban.reset_at?formatDate(ban.reset_at):'—')+'\n（不等于已下线时长）';}
    function reasonLabel(reason){return {payment_required:'无额度或无订阅',forbidden:'上游拒绝访问',unauthorized:'凭据未授权',rate_limited:'请求频率受限',rate_limited_fallback:'限流（默认冷却）'}[reason]||reason}
    function managementState(ban){
      const cycle=Number(ban.cycle||0);
      const maxN=Number(state.max_auto_cycles||3)||3;
      const rem=formatRemaining(ban.remaining_seconds);
      const extras=[];
      if(cycle>0)extras.push('冷却轮次 '+cycle+' / 上限 '+maxN);
      if(ban.unusable_since)extras.push('连续不可用自 '+formatDate(ban.unusable_since));
      const tail=extras.length?('\n'+extras.join('\n')):'';
      const phase=String(ban.phase||'cooling');
      if(ban.last_error){
        const retryingDisable=!ban.management_disabled;
        return {
          label:retryingDisable?'停用失败·重试':'解禁失败·重试',
          className:'management-error',
          title:(retryingDisable?'Management 停用失败，将自动重试。':'Management 启用失败，将自动重试。')+'\n错误：'+String(ban.last_error||'')+tail
        };
      }
      if(phase==='pending'||phase==='pending_cleanup'){
        return {
          label:'待清理',
          className:'management-error',
          title:'已达自动冷却上限（'+cycle+'/'+maxN+'）。插件不再自动启用。\n可手动解禁，或对 403 永久删除。'+tail
        };
      }
      if(ban.management_disabled){
        if(Number(ban.remaining_seconds)>0){
          return {label:'冷却中',className:'management-ok',title:'CPA 已 disabled，调度跳过。\n本轮剩余：'+rem+'\n计划解禁：'+(ban.reset_at?formatDate(ban.reset_at):'—')+tail};
        }
        return {label:'解禁中',className:'management-pending',title:'冷却已到，正在调用 Management 重新启用。'+tail};
      }
      return {label:'冷却中·待停用',className:'management-pending',title:'本地已跳过调度，等待 CPA disable。\n本轮剩余：'+rem+tail};
    }

    async function loadData(silent=false){try{if(!silent){$('syncState').textContent='同步中';setMessage('正在加载实时状态...')}const data=await api('/data');state.bans=data.bans||[];state.deletable_classes=data.deletable_classes||['permission'];state.max_auto_cycles=Number(data.max_auto_cycles||2)||2;state.charts=data.charts||{by_status:[],by_class:[]};state.quota_join=data.quota_join||{};state.pool_total=data.pool_total||0;state.normal_count=data.normal_count||0;if($('poolTotal'))$('poolTotal').textContent=Number(state.pool_total||0).toLocaleString();if($('normalCount'))$('normalCount').textContent=Number(state.normal_count||0).toLocaleString();updateQuotaNote();drawCharts();for(const id of [...state.selected])if(!state.bans.some(x=>x.auth_id===id))state.selected.delete(id);const c=counts();$('total').textContent=data.count.toLocaleString();$('count401').textContent=c[401].toLocaleString();$('count402').textContent=c[402].toLocaleString();$('count403').textContent=c[403].toLocaleString();$('count429').textContent=c[429].toLocaleString();const managementError=data.management&&data.management.last_error;$('syncState').textContent=managementError?'管理接口异常':'已连接';setMessage(managementError||('最后更新：'+new Date().toLocaleTimeString('zh-CN',{hour12:false})),Boolean(managementError));render()}catch(error){$('syncState').textContent='连接异常';setMessage(error.message,true)}}

    const STATUS_COLORS={'ok':'#12b76a','disabled':'#98a2b3','401':'#1570ef','402':'#f79009','403':'#d92d20','429':'#7f56d9'};
    const CLASS_COLORS={ok:'#12b76a',auth:'#1570ef',payment:'#f79009',quota_paid:'#dc6803',quota_free:'#e04f16',permission:'#d92d20',rate_limit:'#7f56d9',forbidden_unknown:'#667085',other:'#98a2b3',legacy:'#d0d5dd'};
    function colorForStatus(key){return STATUS_COLORS[String(key)]||'#98a2b3'}
    function colorForClass(key){return CLASS_COLORS[key]||'#98a2b3'}
    function formatTokens(ban){if(!ban)return '—';const tokens=Number(ban.tokens_24h||0);const total=Number(ban.tokens_total||0);const lim=Number(ban.quota_limit||0);const hasUsage=lim>0||tokens>0||total>0||ban.over_reference;const joined=hasUsage||Boolean(ban.quota_health)||total>0;if(!joined)return '—';const fmt=n=>(Number(n)/1e6).toFixed(2)+'M';let s=fmt(tokens);if(lim>0)s+=' / '+fmt(lim);else if(tokens>0||total>0)s+=' / 2.00M';if(total>0)s+=' · Σ'+fmt(total);if(ban.over_reference)s+=' ↑';return s}
    function tokensTip(ban){const t24=Number(ban.tokens_24h||0),tot=Number(ban.tokens_total||0);return '近24h：'+(t24/1e6).toFixed(3)+'M token\n累计成功：'+(tot/1e6).toFixed(3)+'M token\n来源：usage.sqlite（只读）';}
    function updateQuotaNote(){const q=state.quota_join||{},el=$('quotaNote');if(!el)return;if(!q.enabled){el.className='quota-note';el.textContent='用量观测：已关闭（observe-usage / join-quota-state）';return}if(q.ok){el.className='quota-note ok';const src=q.source==='usage_sqlite'?'内嵌 usage.sqlite':(q.source||'state');el.textContent='用量观测：已连接 ['+src+'] '+(q.path||'')+' · 命中隔离行 '+Number(q.matched||0)+' · 池约 '+Number(state.pool_total||q.pool_accounts||0)+'（只读，不预 ban）';return}el.className='quota-note warn';el.textContent='用量观测：未连接（'+(q.error||'找不到 usage.sqlite')+'）。可配置 usage-db-path / XAI_AUTOBAN_USAGE_DB 或 CPAMP_USAGE_DB'}
    function drawPie(pieId,legendId,slices,colorFn){const pie=$(pieId),legend=$(legendId);if(!pie||!legend)return;const data=(slices||[]).filter(s=>Number(s.count)>0);const total=data.reduce((a,s)=>a+Number(s.count),0);legend.innerHTML='';if(!total){pie.className='pie empty';pie.style.background='';pie.textContent='无数据';return}pie.className='pie';pie.textContent='';let angle=0;const parts=[];for(const s of data){const c=colorFn(s.key);const deg=Number(s.count)/total*360;parts.push(c+' '+angle+'deg '+(angle+deg)+'deg');angle+=deg;const row=document.createElement('div');row.className='legend-row';row.innerHTML='<span class="swatch" style="background:'+c+'"></span><span class="legend-label">'+esc(s.label||s.key)+'</span><span class="legend-count">'+Number(s.count).toLocaleString()+' · '+(100*Number(s.count)/total).toFixed(1)+'%</span>';legend.appendChild(row)}pie.style.background='conic-gradient('+parts.join(',')+')'}
    function drawCharts(){const c=state.charts||{};drawPie('pieStatus','legendStatus',c.by_status||[],colorForStatus);drawPie('pieClass','legendClass',c.by_class||[],colorForClass);const sub=document.querySelectorAll('.chart-sub');if(sub[0]&&c.pool_source){sub[0].textContent='池来源: '+(c.pool_source||'')+(c.includes_normal?' · 含正常号':' · 仅隔离号')}}
    function render(){const list=filtered();const pages=Math.max(1,Math.ceil(list.length/state.pageSize));state.page=Math.min(state.page,pages);const start=(state.page-1)*state.pageSize;const pageRows=list.slice(start,start+state.pageSize);$('rows').innerHTML=pageRows.map(ban=>{const management=managementState(ban);const actions='<button class="row-action" data-unban="'+esc(ban.auth_id)+'">解禁</button>'+(ban.status_code===403?' <button class="row-action danger" data-delete="'+esc(ban.auth_id)+'">删除账号</button>':'');return '<tr><td class="check"><input type="checkbox" data-id="'+esc(ban.auth_id)+'" '+(state.selected.has(ban.auth_id)?'checked':'')+'></td><td><code>'+esc(ban.auth_id)+'</code></td><td><span class="badge b'+ban.status_code+'">'+ban.status_code+'</span></td><td class="reason"><code>'+esc(ban.class||'legacy')+'</code></td><td class="remaining tip" data-tip="'+esc(tokensTip(ban))+'">'+esc(formatTokens(ban))+'</td><td class="reason">'+esc(reasonLabel(ban.reason))+'</td><td class="'+management.className+' tip" data-tip="'+esc(management.title)+'">'+esc(management.label)+'</td><td class="time tip" data-tip="'+esc(lastUsedTip(ban))+'">'+esc(formatLastUsed(ban.last_used))+'</td><td class="remaining tip" data-tip="'+esc(offlineTip(ban))+'">'+esc(formatOfflineHours(ban.offline_hours))+'</td><td class="remaining tip" data-tip="'+esc(remainTip(ban))+'">'+esc(formatRemaining(ban.remaining_seconds))+'</td><td class="remaining">'+esc(String(ban.cycle||0))+'</td><td class="actions">'+actions+'</td></tr>'}).join('');$('empty').hidden=pageRows.length>0;$('resultCount').textContent=list.length.toLocaleString()+' 条';$('range').textContent=(list.length?start+1:0)+'-'+Math.min(start+state.pageSize,list.length)+' / '+list.length;$('pageNumber').textContent=state.page+' / '+pages;$('prev').disabled=state.page<=1;$('next').disabled=state.page>=pages;$('unbanSelected').disabled=state.selected.size===0;$('unbanSelected').textContent='解禁已选 ('+state.selected.size+')';$('selectPage').checked=pageRows.length>0&&pageRows.every(x=>state.selected.has(x.auth_id));document.querySelectorAll('#rows input[type=checkbox]').forEach(input=>input.addEventListener('change',()=>{input.checked?state.selected.add(input.dataset.id):state.selected.delete(input.dataset.id);render()}));document.querySelectorAll('#rows [data-unban]').forEach(button=>button.addEventListener('click',()=>unbanOne(encodeURIComponent(button.dataset.unban))));document.querySelectorAll('#rows [data-delete]').forEach(button=>button.addEventListener('click',()=>deleteOne(encodeURIComponent(button.dataset.delete))))}
    function changePage(delta){state.page+=delta;render();document.querySelector('.table-wrap').scrollTop=0}
    async function runAction(params,question,successText){if(question&&!confirm(question))return;try{setMessage('正在执行操作...');const result=await api('/action?'+new URLSearchParams(params));state.selected.clear();const done=successText?successText(result):('操作完成，已解禁 '+(result.removed||0)+' 个凭据');setMessage(done);await loadData(true)}catch(error){setMessage(error.message,true)}}
    function unbanOne(encoded){const id=decodeURIComponent(encoded);runAction({op:'unban',auth_id:id},'确认解禁该凭据？\n'+id)}
    function unbanStatus(status){const n=state.bans.filter(x=>x.status_code===status).length;runAction({op:'unban-status',status},'确认解禁全部 '+n+' 个 '+status+' 凭据？')}
    function unbanAll(){runAction({op:'unban-all'},'确认解禁全部 '+state.bans.length+' 个凭据？此操作会立即改变调度池。')}
    function unbanSelected(){const ids=[...state.selected];runAction({op:'unban-many',auth_ids:ids.join(',')},'确认解禁已选择的 '+ids.length+' 个凭据？')}
    function deleteOne(encoded){const id=decodeURIComponent(encoded);runAction({op:'delete',auth_id:id},'确认永久删除该 403 账号？\n'+id+'\n\n此操作会从 CPA 删除凭据文件，不是解除禁用。',r=>'操作完成，已永久删除 '+(r.deleted||0)+' 个 403 账号')}
    function deleteSelected403(){const selected=state.bans.filter(x=>state.selected.has(x.auth_id)&&x.status_code===403);if(!selected.length){setMessage('没有已选的 403 账号',true);return}const ids=selected.map(x=>x.auth_id);runAction({op:'delete-many',auth_ids:ids.join(',')},'确认永久删除已选 '+ids.length+' 个 403 账号？\n此操作会从 CPA 删除凭据文件，不是解除禁用。',r=>'操作完成，已永久删除 '+(r.deleted||0)+' 个 403 账号')}
    function deleteAll403(){const n=state.bans.filter(x=>x.status_code===403).length;runAction({op:'delete-403'},'确认永久删除全部 '+n+' 个 403 账号？\n此操作会从 CPA 删除凭据文件，不是解除禁用，且不可恢复。',r=>'操作完成，已永久删除 '+(r.deleted||0)+' 个 403 账号')}
    async function copyVisible(){const ids=filtered().map(x=>x.auth_id).join('\n');try{await navigator.clipboard.writeText(ids);setMessage('已复制 '+filtered().length+' 个 Auth ID')}catch(_){setMessage('浏览器拒绝访问剪贴板',true)}}
    function stamp(){const d=new Date();const p=n=>String(n).padStart(2,'0');return d.getFullYear()+p(d.getMonth()+1)+p(d.getDate())+'-'+p(d.getHours())+p(d.getMinutes())+p(d.getSeconds())}
    function downloadBlob(blob,filename){const url=URL.createObjectURL(blob);const a=document.createElement('a');a.href=url;a.download=filename;document.body.appendChild(a);a.click();a.remove();setTimeout(()=>URL.revokeObjectURL(url),1500)}
    async function exportJSON(){try{setMessage('正在导出 JSON…');const data=await api('/export');const text=JSON.stringify(data,null,2);downloadBlob(new Blob([text],{type:'application/json;charset=utf-8'}), 'xai-autoban-export-'+stamp()+'.json');setMessage('已导出 JSON（隔离 '+Number((data.status&&data.status.count)||(data.status&&data.status.bans&&data.status.bans.length)||0)+' 行 · 用量账号 '+Number((data.usage_accounts||[]).length)+'）')}catch(error){setMessage(error.message,true)}}
    async function exportCSV(){try{setMessage('正在导出 CSV…');const response=await fetch(base+'/export?format=csv',{cache:'no-store'});const text=await response.text();if(!response.ok){let msg=text;try{msg=(JSON.parse(text).error||text)}catch(_){ }throw new Error(msg||('HTTP '+response.status))}downloadBlob(new Blob([text],{type:'text/csv;charset=utf-8'}), 'xai-autoban-bans-'+stamp()+'.csv');setMessage('已导出 CSV')}catch(error){setMessage(error.message,true)}}

    $('search').addEventListener('input',event=>{state.query=event.target.value.trim();state.page=1;render()});
    $('filters').addEventListener('click',event=>{const button=event.target.closest('button');if(!button)return;state.filter=button.dataset.status;state.page=1;document.querySelectorAll('#filters button').forEach(x=>x.classList.toggle('active',x===button));render()});
    $('selectPage').addEventListener('change',event=>{const list=filtered(),start=(state.page-1)*state.pageSize;for(const ban of list.slice(start,start+state.pageSize))event.target.checked?state.selected.add(ban.auth_id):state.selected.delete(ban.auth_id);render()});
    $('autoRefresh').addEventListener('change',configureAutoRefresh);
    function configureAutoRefresh(){if(state.timer)clearInterval(state.timer);state.timer=$('autoRefresh').checked?setInterval(()=>loadData(true),30000):null}
    configureAutoRefresh();
    document.querySelectorAll('th.sortable').forEach(th=>th.addEventListener('click',()=>setSort(th.dataset.sort)));
    // default sort indicator
    document.querySelectorAll('th.sortable').forEach(th=>{if(th.dataset.sort===state.sortKey)th.classList.add(state.sortDir==='asc'?'sorted-asc':'sorted-desc')});
    loadData();
  </script>
</body>
</html>`
}

func jsonResponse(status int, value any) pluginapi.ManagementResponse {
	raw, _ := json.MarshalIndent(value, "", "  ")
	return pluginapi.ManagementResponse{StatusCode: status, Headers: http.Header{"Content-Type": {"application/json; charset=utf-8"}}, Body: raw}
}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func okEnvelope(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
