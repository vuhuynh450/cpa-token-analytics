package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

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

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unsafe"

	_ "modernc.org/sqlite"
)

//go:embed web/index.html web/assets/*
var webFS embed.FS

const abiVersion uint32 = 1

// version is injected at release time via -ldflags "-X main.version=..."
var version = "0.4.0"

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

var (
	mu       sync.Mutex
	db       *sql.DB
	lastRecv time.Time
	lastErr  string
)

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(abiVersion)
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
	var req []byte
	if request != nil && requestLen > 0 {
		req = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, err := handleMethod(C.GoString(method), req)
	if err != nil {
		writeResponse(response, errorEnvelope("plugin_error", err.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = len
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	mu.Lock()
	defer mu.Unlock()
	if db != nil {
		_ = db.Close()
		db = nil
	}
}

func handleMethod(method string, req []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		applyConfig(req)
		return okEnvelopeJSON(`{"schema_version":1,"metadata":{"Name":"cpa-token-analytics","Version":"` + version + `","Author":"vuhuynh450","GitHubRepository":"https://github.com/vuhuynh450/cpa-token-analytics","Logo":"","ConfigFields":[]},"capabilities":{"usage_plugin":true,"management_api":true}}`)
	case "management.register":
		return okEnvelopeJSON(`{"routes":[
			{"Method":"GET","Path":"/plugins/cpa-token-analytics/api/usage","Menu":"","Description":"usage data api"},
			{"Method":"GET","Path":"/plugins/cpa-token-analytics/api/health","Menu":"","Description":"health"},
			{"Method":"POST","Path":"/plugins/cpa-token-analytics/api/collect","Menu":"","Description":"collect trigger"},
			{"Method":"GET","Path":"/plugins/cpa-token-analytics/api/analytics/overview","Menu":"","Description":"analytics overview"},
			{"Method":"GET","Path":"/plugins/cpa-token-analytics/api/analytics/performance","Menu":"","Description":"analytics performance"},
			{"Method":"GET","Path":"/plugins/cpa-token-analytics/api/analytics/comparison","Menu":"","Description":"analytics comparison"},
			{"Method":"GET","Path":"/plugins/cpa-token-analytics/api/analytics/heatmap","Menu":"","Description":"analytics heatmap"},
			{"Method":"GET","Path":"/plugins/cpa-token-analytics/api/analytics/cost","Menu":"","Description":"analytics cost"},
			{"Method":"GET","Path":"/plugins/cpa-token-analytics/api/pricing/status","Menu":"","Description":"pricing status"},
			{"Method":"GET","Path":"/plugins/cpa-token-analytics/api/pricing/models","Menu":"","Description":"pricing catalog"},
			{"Method":"POST","Path":"/plugins/cpa-token-analytics/api/pricing/sync","Menu":"","Description":"pricing sync"},
			{"Method":"POST","Path":"/plugins/cpa-token-analytics/api/pricing/override","Menu":"","Description":"price override"},
			{"Method":"POST","Path":"/plugins/cpa-token-analytics/api/pricing/settings","Menu":"","Description":"pricing settings"},
			{"Method":"GET","Path":"/plugins/cpa-token-analytics/api/fx/status","Menu":"","Description":"fx rate status"},
			{"Method":"POST","Path":"/plugins/cpa-token-analytics/api/fx/sync","Menu":"","Description":"fx rate sync"},
			{"Method":"POST","Path":"/plugins/cpa-token-analytics/api/fx/settings","Menu":"","Description":"fx settings"},
			{"Method":"GET","Path":"/plugins/cpa-token-analytics/api/changes","Menu":"","Description":"usage revision check"},
			{"Method":"GET","Path":"/plugins/cpa-token-analytics/api/requests","Menu":"","Description":"request inspector list"},
			{"Method":"GET","Path":"/plugins/cpa-token-analytics/api/requests/{id}","Menu":"","Description":"request inspector detail"}
		],"resources":[
			{"Path":"/report","Menu":"CPA Token Analytics","Description":"CPA usage dashboard"},
			{"Path":"/assets/index-C2Dxclol.js","Menu":"","Description":"dashboard js"},
			{"Path":"/assets/index-CF7M4cm3.css","Menu":"","Description":"dashboard css"}
		]}`)
	case "usage.handle":
		handleUsage(req)
		return okEnvelopeJSON(`{}`)
	case "management.handle":
		return handleManagement(req)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// ---- config ----

func applyConfig(req []byte) {
	var parsed struct {
		ConfigYaml string `json:"config_yaml"`
	}
	_ = json.Unmarshal(req, &parsed)
	if parsed.ConfigYaml == "" {
		// Some host versions wrap the payload in a {"result": {...}} envelope.
		var wrapped struct {
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(req, &wrapped); err == nil && wrapped.Result != nil {
			_ = json.Unmarshal(wrapped.Result, &parsed)
		}
	}
	if parsed.ConfigYaml == "" {
		return
	}
	path := ""
	for _, line := range strings.Split(parsed.ConfigYaml, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "db_path:") {
			path = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "db_path:")), `"'`)
			break
		}
	}
	if path == "" {
		return
	}
	if strings.HasPrefix(path, "~") {
		if u, err := user.Current(); err == nil {
			path = filepath.Join(u.HomeDir, path[2:])
		}
	}
	openDB(path)
}

func defaultDBPath() string {
	u, err := user.Current()
	if err != nil {
		return "usage.sqlite"
	}
	return filepath.Join(u.HomeDir, ".cli-proxy-api", "cpa-token-analytics.sqlite")
}

func openDB(path string) {
	mu.Lock()
	defer mu.Unlock()
	if db != nil && path != "" && lastOpened == path {
		return
	}
	if db != nil {
		_ = db.Close()
	}
	conn, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		lastErr = "sqlite open: " + err.Error()
		return
	}
	_, err = conn.Exec("CREATE TABLE IF NOT EXISTS usage_events (id INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL, dedup TEXT UNIQUE, payload TEXT)")
	if err != nil {
		lastErr = "sqlite migrate: " + err.Error()
		_ = conn.Close()
		return
	}
	db = conn
	if err := runMigrations(conn); err != nil {
		// Keep the error so collect_err reports it; the partially-migrated
		// connection stays usable (migrations are re-run on next open).
		// lastOpened is only set after migrations succeed so the next
		// openDB call retries instead of early-returning.
		lastErr = "sqlite migrate: " + err.Error()
		return
	}
	lastOpened = path
	lastErr = ""
}

var lastOpened string

func ensureDB() {
	if db == nil {
		path := defaultDBPath()
		// First-open migration only applies to the default location; an
		// explicit db_path (applyConfig -> openDB) is used exactly as given.
		migrateLegacyDB(filepath.Join(filepath.Dir(path), "usage-report.sqlite"), path)
		openDB(path)
	}
}

// migrateLegacyDB copies the legacy pre-rebrand database file to newPath
// when newPath does not exist yet. Additive and idempotent: the legacy
// file is left untouched and an existing newPath is never overwritten.
func migrateLegacyDB(legacyPath, newPath string) {
	if _, err := os.Stat(newPath); err == nil {
		return
	}
	src, err := os.Open(legacyPath)
	if err != nil {
		return
	}
	defer src.Close()
	if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
		return
	}
	dst, err := os.OpenFile(newPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		_ = os.Remove(newPath)
		return
	}
	_ = dst.Close()
}

// ---- usage ----

func handleUsage(req []byte) {
	var parsed struct {
		Result json.RawMessage `json:"result"`
	}
	if len(req) > 0 {
		_ = json.Unmarshal(req, &parsed)
	}
	rec := parsed.Result
	if rec == nil {
		rec = req
	}
	var u usageRecord
	if err := json.Unmarshal(rec, &u); err != nil {
		return
	}
	lastRecv = time.Now()

	model := u.Model
	if model == "" {
		model = u.Alias
	}
	if model == "" {
		model = "unknown"
	}
	ts := u.RequestedAt
	if ts.IsZero() {
		ts = time.Now()
	}
	tokens := map[string]int64{
		"input_tokens":          u.Detail.InputTokens,
		"output_tokens":         u.Detail.OutputTokens,
		"reasoning_tokens":      u.Detail.ReasoningTokens,
		"cached_tokens":         u.Detail.CachedTokens,
		"cache_read_tokens":     u.Detail.CacheReadTokens,
		"cache_creation_tokens": u.Detail.CacheCreationTokens,
		"total_tokens":          u.Detail.TotalTokens,
	}
	source := u.Source
	if source == "" {
		source = u.APIKey
	}
	event := map[string]interface{}{
		"timestamp":  ts.Format(time.RFC3339Nano),
		"model":      model,
		"source":     source,
		"failed":     u.Failed,
		"latency_ms": float64(u.Latency) / float64(time.Millisecond),
		"ttft_ms":    float64(u.TTFT) / float64(time.Millisecond),
		"tokens":     tokens,
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return
	}
	digest := sha1.Sum(payload)
	// Keep the existing dedup identity when adding optional protocol metadata.
	statusCode := u.StatusCode
	if statusCode == 0 {
		statusCode = u.HTTPStatus
	}
	if statusCode == 0 {
		statusCode = u.Status
	}
	authType := u.AuthType
	if authType == "" {
		authType = u.AccountType
	}
	if authType == "" {
		proto := strings.ToLower(u.ExecutorType + " " + u.Provider)
		for _, k := range []string{"oauth", "subscription", "account"} {
			if strings.Contains(proto, k) {
				authType = k
				break
			}
		}
	}
	if u.Provider != "" || u.ExecutorType != "" || statusCode != 0 || authType != "" {
		if u.Provider != "" {
			event["provider"] = u.Provider
		}
		if u.ExecutorType != "" {
			event["executor_type"] = u.ExecutorType
		}
		if statusCode != 0 {
			event["status_code"] = statusCode
		}
		if authType != "" {
			event["auth_type"] = authType
		}
		payload, err = json.Marshal(event)
		if err != nil {
			return
		}
	}

	ensureDB()
	mu.Lock()
	defer mu.Unlock()
	if db == nil {
		return
	}
	_, _ = db.Exec("INSERT OR IGNORE INTO usage_events (ts, dedup, payload, ts_unix) VALUES (?,?,?,?)",
		ts.Format("2006-01-02 15:04:05"), hex.EncodeToString(digest[:]), string(payload), float64(ts.UnixNano())/1e9)
}

type usageRecord struct {
	Provider        string
	ExecutorType    string
	StatusCode      int
	HTTPStatus      int
	Status          int
	AuthType        string
	AccountType     string
	Model           string
	Alias           string
	APIKey          string
	Source          string
	ReasoningEffort string
	RequestedAt     time.Time
	Latency         time.Duration
	TTFT            time.Duration
	Failed          bool
	Detail          struct {
		InputTokens         int64
		OutputTokens        int64
		ReasoningTokens     int64
		CachedTokens        int64
		CacheReadTokens     int64
		CacheCreationTokens int64
		TotalTokens         int64
	}
}

// ---- management ----

func handleManagement(req []byte) ([]byte, error) {
	var parsed struct {
		Result json.RawMessage `json:"result"`
	}
	if len(req) > 0 {
		_ = json.Unmarshal(req, &parsed)
	}
	rec := parsed.Result
	if rec == nil {
		rec = req
	}
	var m struct {
		Method string
		Path   string
		Query  map[string][]string
		Body   any
	}
	if err := json.Unmarshal(rec, &m); err != nil {
		return errorEnvelope("bad_request", err.Error()), nil
	}

	if strings.HasSuffix(m.Path, "/report") || m.Path == "" {
		html, err := webFS.ReadFile("web/index.html")
		if err != nil {
			return errorEnvelope("asset_missing", err.Error()), nil
		}
		return managementResponse(200, "text/html; charset=utf-8", html)
	}
	if i := strings.LastIndex(m.Path, "/assets/"); i >= 0 {
		name := m.Path[i+len("/assets/"):]
		if name == "" || strings.Contains(name, "..") || strings.Contains(name, "/") {
			return managementResponse(404, "text/plain; charset=utf-8", []byte("not found"))
		}
		data, err := webFS.ReadFile("web/assets/" + name)
		if err != nil {
			return errorEnvelope("asset_missing", err.Error()), nil
		}
		return managementResponse(200, assetContentType(name), data)
	}
	if strings.HasSuffix(m.Path, "/api/health") {
		body, _ := json.Marshal(map[string]interface{}{"service": "cpa-token-analytics", "version": version, "pid": 0})
		return managementResponse(200, "application/json; charset=utf-8", body)
	}
	if strings.HasSuffix(m.Path, "/api/collect") {
		collectOnce()
		body, _ := json.Marshal(map[string]interface{}{"collected_at": collectedAt(), "collect_err": collectErr()})
		return managementResponse(200, "application/json; charset=utf-8", body)
	}
	if strings.HasSuffix(m.Path, "/api/usage") {
		body, err := usageResponse(m.Query)
		if err != nil {
			b, _ := json.Marshal(map[string]interface{}{"error": err.Error()})
			return managementResponse(400, "application/json; charset=utf-8", b)
		}
		return managementResponse(200, "application/json; charset=utf-8", body)
	}
	if strings.Contains(m.Path, "/api/analytics/") {
		return handleAnalytics(m.Path, m.Query)
	}
	if strings.Contains(m.Path, "/api/pricing/") {
		return handlePricing(m.Path, m.Method, m.Body)
	}
	if strings.Contains(m.Path, "/api/fx/") {
		return handleFx(m.Path, m.Method, m.Body)
	}
	if strings.Contains(m.Path, "/api/changes") {
		return handleChanges(m.Method)
	}
	if strings.Contains(m.Path, "/api/requests") {
		return handleRequests(m.Path, m.Method, m.Query)
	}
	return managementResponse(404, "text/plain; charset=utf-8", []byte("not found"))
}

func assetContentType(name string) string {
	switch {
	case strings.HasSuffix(name, ".js"):
		return "application/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".map"):
		return "application/json"
	default:
		return "application/octet-stream"
	}
}

func managementResponse(status int, contentType string, body []byte) ([]byte, error) {
	obj, err := json.Marshal(map[string]interface{}{
		"StatusCode": status,
		"Headers":    map[string][]string{"content-type": {contentType}},
		"Body":       body,
	})
	if err != nil {
		return errorEnvelope("encode_error", err.Error()), nil
	}
	return json.Marshal(envelope{OK: true, Result: obj})
}

// ---- usage api (compatible with cpa-usage frontend contract) ----

type dbEvent struct {
	Timestamp    string         `json:"timestamp"`
	Model        string         `json:"model"`
	Provider     string         `json:"provider"`
	ExecutorType string         `json:"executor_type"`
	Alias        string         `json:"alias"`
	Source       string         `json:"source"`
	APIKey       string         `json:"api_key"`
	Failed       bool           `json:"failed"`
	LatencyMs    float64        `json:"latency_ms"`
	TTFTMs       float64        `json:"ttft_ms"`
	StatusCode   int            `json:"status_code"`
	AuthType     string         `json:"auth_type"`
	Tokens       map[string]any `json:"tokens"`
}

type outEvent struct {
	Ts         float64 `json:"ts"`
	Model      string  `json:"model"`
	Key        string  `json:"key"`
	Input      int64   `json:"input"`
	Output     int64   `json:"output"`
	Cache      int64   `json:"cache"`
	CacheWrite int64   `json:"cache_write"`
	Reasoning  int64   `json:"reasoning"`
	Failed     bool    `json:"failed"`
	LatencyMs  float64 `json:"latency_ms"`
	TTFTMs     float64 `json:"ttft_ms"`
	Provider   string  `json:"provider"`
	AuthType   string  `json:"auth_type,omitempty"`
	StatusCode int     `json:"status_code,omitempty"`
}

type outBucket struct {
	Label    string `json:"label"`
	Input    int64  `json:"input"`
	Output   int64  `json:"output"`
	Cache    int64  `json:"cache"`
	Requests int64  `json:"requests"`
}

func anonKey(source string) string {
	if source == "" {
		return "未知 Key"
	}
	sum := sha256.Sum256([]byte(source))
	return "key-" + hex.EncodeToString(sum[:])[:10]
}

func usageResponse(q map[string][]string) ([]byte, error) {
	get := func(k string) string {
		if v, ok := q[k]; ok && len(v) > 0 {
			return v[0]
		}
		return ""
	}
	startStr := get("start")
	endStr := get("end")
	bucket := get("bucket")
	if bucket == "" {
		bucket = "day"
	}
	if bucket != "hour" && bucket != "day" && bucket != "month" {
		return nil, fmt.Errorf("无效时间粒度")
	}
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	var a, b time.Time
	if startStr != "" {
		var err error
		a, err = time.ParseInLocation("2006-01-02", startStr, time.Local)
		if err != nil {
			return nil, fmt.Errorf("无效开始日期")
		}
	} else {
		a = today.AddDate(0, 0, -6)
	}
	if endStr != "" {
		var err error
		b, err = time.ParseInLocation("2006-01-02", endStr, time.Local)
		if err != nil {
			return nil, fmt.Errorf("无效结束日期")
		}
	} else {
		b = today
	}
	if a.After(b) {
		return nil, fmt.Errorf("开始日期不能晚于结束日期")
	}
	lo := float64(a.Unix())
	hi := float64(b.AddDate(0, 0, 1).Unix())

	events := []outEvent{}
	bins := map[string]*outBucket{}
	order := []string{}
	skipped := 0
	authSet := map[string]bool{}

	ensureDB()
	mu.Lock()
	var rows *sql.Rows
	var err error
	if db != nil {
		rows, err = queryUsageRows(lo, hi)
	}
	mu.Unlock()
	if err != nil {
		return nil, err
	}
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var id int64
			var storedTs, payload string
			if err := rows.Scan(&id, &storedTs, &payload); err != nil {
				continue
			}
			var e dbEvent
			if err := json.Unmarshal([]byte(payload), &e); err != nil {
				skipped++
				continue
			}
			row, ok := normalizeRow(e, storedTs)
			if !ok {
				skipped++
				continue
			}
			row.id = id
			if row.ts < lo || row.ts >= hi {
				continue
			}
			if row.authType != "" {
				authSet[row.authType] = true
			}
			events = append(events, outEvent{
				Ts: row.ts, Model: row.model, Key: row.key,
				Input: row.input, Output: row.output, Cache: row.cache, CacheWrite: row.cacheWrite,
				Reasoning: row.reasoning, Failed: row.failed, LatencyMs: row.latency, TTFTMs: row.ttft,
				Provider: row.provider, AuthType: row.authType, StatusCode: row.statusCode,
			})
			label := bucketLabel(time.Unix(int64(row.ts), 0), bucket)
			bk := bins[label]
			if bk == nil {
				bk = &outBucket{Label: label}
				bins[label] = bk
				order = append(order, label)
			}
			bk.Input += row.input
			bk.Output += row.output
			bk.Cache += row.cache
			bk.Requests++
		}
	}
	sort.Strings(order)
	buckets := make([]*outBucket, 0, len(order))
	for _, l := range order {
		buckets = append(buckets, bins[l])
	}

	resp := map[string]interface{}{
		"events":       events,
		"buckets":      buckets,
		"collected_at": collectedAt(),
		"collect_err":  collectErr(),
		"skipped":      skipped,
		"version":      version,
		"configured":   true,
		"timezone":     time.Now().Format("MST"),
		"auth_type":    authTypeSummary(authSet),
	}
	return json.Marshal(resp)
}

func addInputCache(e dbEvent, input, output, cache, creation, reasoning int64) bool {
	cachedInput := cache + creation
	if cachedInput <= 0 {
		return false
	}
	total := toI64(e.Tokens["total_tokens"])
	// An inclusive total must never receive cache a second time.
	if total > 0 && total == input+output {
		return false
	}
	provider := strings.ToLower(strings.TrimSpace(e.Provider))
	executor := strings.ToLower(strings.TrimSpace(e.ExecutorType))
	if executor == "openaicompatexecutor" || provider == "openai-compatibility" || strings.HasPrefix(provider, "openai-compatible-") {
		return false
	}
	protocol := provider + " " + executor
	if strings.Contains(protocol, "claude") || strings.Contains(protocol, "anthropic") {
		return total == 0 || total == input+output+cachedInput
	}
	if provider != "" || executor != "" {
		return false
	}
	// Old rows have no protocol identity. Only repair counts that cannot be
	// an input subset; total equality alone also matches Gemini reasoning.
	if cachedInput <= input {
		return false
	}
	return (total == 0 && input == 0) ||
		(total == input+output+cachedInput && total != input+output+reasoning)
}

func parseAnyTime(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty time")
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return float64(t.UnixNano()) / 1e9, nil
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.Local); err == nil {
		return float64(t.UnixNano()) / 1e9, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return float64(t.UnixNano()) / 1e9, nil
	}
	return 0, fmt.Errorf("unsupported time %q", s)
}

func collectedAt() interface{} {
	// Report the data freshness timestamp as of this response: with real-time
	// collection this means "the dashboard data is current as of now".
	return time.Now().Format(time.RFC3339Nano)
}

func collectErr() interface{} {
	mu.Lock()
	defer mu.Unlock()
	if lastErr == "" {
		return nil
	}
	return lastErr
}

func collectOnce() {
	ensureDB()
	mu.Lock()
	lastRecv = time.Now()
	mu.Unlock()
}

func toI64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}

// ---- envelope helpers ----

func okEnvelopeJSON(result string) ([]byte, error) {
	return json.Marshal(envelope{OK: true, Result: json.RawMessage(result)})
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

func callHost(method string, payload []byte) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var response C.cliproxy_buffer
	var req *C.uint8_t
	if len(payload) > 0 {
		req = (*C.uint8_t)(C.CBytes(payload))
		defer C.free(unsafe.Pointer(req))
	}
	if C.call_host_api(cMethod, req, C.size_t(len(payload)), &response) == 0 && response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
}

var _ = os.Getenv
