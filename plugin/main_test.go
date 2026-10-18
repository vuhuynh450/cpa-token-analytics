package main

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegister(t *testing.T) {
	raw, err := handleMethod("plugin.register", nil)
	if err != nil {
		t.Fatal(err)
	}
	var e envelope
	if err := json.Unmarshal(raw, &e); err != nil || !e.OK {
		t.Fatalf("register: %s %v", raw, err)
	}
}

func mgmtReq(path, method string) []byte {
	req, _ := json.Marshal(map[string]interface{}{
		"result": map[string]interface{}{
			"Method":  method,
			"Path":    path,
			"Query":   map[string][]string{"start": {"2026-09-09"}, "end": {"2026-09-18"}, "bucket": {"day"}},
			"Headers": map[string][]string{},
			"Body":    nil,
		},
	})
	return req
}

func callMgmt(t *testing.T, path, method string) (int, string, []byte) {
	t.Helper()
	raw, err := handleMethod("management.handle", mgmtReq(path, method))
	if err != nil {
		t.Fatal(err)
	}
	var e envelope
	if err := json.Unmarshal(raw, &e); err != nil || !e.OK {
		t.Fatalf("handle %s: %s %v", path, raw, err)
	}
	var resp struct {
		StatusCode int
		Headers    map[string][]string
		Body       []byte
	}
	if err := json.Unmarshal(e.Result, &resp); err != nil {
		t.Fatal(err)
	}
	ct := ""
	if v := resp.Headers["content-type"]; len(v) > 0 {
		ct = v[0]
	}
	return resp.StatusCode, ct, resp.Body
}

func TestManagementReport(t *testing.T) {
	status, ct, body := callMgmt(t, "/v0/resource/plugins/cpa-token-analytics/report", "GET")
	if status != 200 || len(body) < 300 {
		t.Fatalf("page: %d %s %d bytes", status, ct, len(body))
	}
	t.Logf("page ok: %d bytes, %s", len(body), ct)

	entries, err := fs.ReadDir(webFS, "web/assets")
	if err != nil {
		t.Fatalf("read embedded assets: %v", err)
	}
	jsName := ""
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".js") {
			jsName = e.Name()
			break
		}
	}
	if jsName == "" {
		t.Fatal("no .js asset embedded under web/assets")
	}

	status, ct, js := callMgmt(t, "/v0/resource/plugins/cpa-token-analytics/assets/"+jsName, "GET")
	if status != 200 || len(js) < 100000 {
		t.Fatalf("js: %d %s %d bytes", status, ct, len(js))
	}
	t.Logf("js ok: %s %d bytes, %s", jsName, len(js), ct)

	cssName := ""
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".css") {
			cssName = e.Name()
			break
		}
	}
	if cssName == "" {
		t.Fatal("no .css asset embedded under web/assets")
	}
	status, ct, css := callMgmt(t, "/v0/resource/plugins/cpa-token-analytics/assets/"+cssName, "GET")
	if status != 200 || !strings.HasPrefix(ct, "text/css") || len(css) < 1000 {
		t.Fatalf("css: %d %s %d bytes", status, ct, len(css))
	}
	t.Logf("css ok: %s %d bytes, %s", cssName, len(css), ct)

	status, _, _ = callMgmt(t, "/v0/resource/plugins/cpa-token-analytics/assets/../main.go", "GET")
	if status != 404 {
		t.Fatalf("path traversal must return 404, got %d", status)
	}
	raw, err := handleMethod("management.handle",
		mgmtReq("/v0/resource/plugins/cpa-token-analytics/assets/missing.js", "GET"))
	if err != nil {
		t.Fatal(err)
	}
	var miss envelope
	if err := json.Unmarshal(raw, &miss); err != nil {
		t.Fatal(err)
	}
	if miss.OK || miss.Error == nil || miss.Error.Code != "asset_missing" {
		t.Fatalf("missing asset must return asset_missing envelope: %s", raw)
	}
}

func TestManagementAPI(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "test.sqlite"))
	seed := []byte(`{"result":{"Provider":"codex","Model":"gpt-ci-test","Source":"sk-ci","RequestedAt":"2026-09-10T11:30:00+08:00","Detail":{"InputTokens":100,"OutputTokens":20,"CacheReadTokens":80,"TotalTokens":120},"Failed":false}}`)
	if _, err := handleMethod("usage.handle", seed); err != nil {
		t.Fatal(err)
	}
	status, _, body := callMgmt(t, "/v0/management/plugins/cpa-token-analytics/api/usage", "GET")
	if status != 200 {
		t.Fatalf("usage api: %d %s", status, body)
	}
	var data struct {
		Events  []map[string]interface{} `json:"events"`
		Buckets []map[string]interface{} `json:"buckets"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		t.Fatalf("decode: %v body=%s", err, body[:200])
	}
	if len(data.Events) != 1 {
		t.Fatalf("expect 1 event, got %d", len(data.Events))
	}
	if got := data.Events[0]["model"]; got != "gpt-ci-test" {
		t.Fatalf("unexpected model: %v", got)
	}
	t.Logf("usage api ok: %d events, %d buckets", len(data.Events), len(data.Buckets))
}

func TestUsageHandle(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "test.sqlite"))
	req := []byte(`{"result":{"Provider":"codex","Model":"gpt-6-astra","Source":"sk-test","RequestedAt":"2026-09-18T11:30:00+08:00","Latency":1500000000,"TTFT":250000000,"Detail":{"InputTokens":100,"OutputTokens":20,"ReasoningTokens":5,"CachedTokens":80,"CacheReadTokens":80,"TotalTokens":120},"Failed":false}}`)
	raw, err := handleMethod("usage.handle", req)
	if err != nil {
		t.Fatal(err)
	}
	var e envelope
	if err := json.Unmarshal(raw, &e); err != nil || !e.OK {
		t.Fatalf("usage.handle: %s %v", raw, err)
	}
	var n int
	mu.Lock()
	_ = db.QueryRow("SELECT COUNT(*) FROM usage_events").Scan(&n)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("expect 1 row, got %d", n)
	}
	t.Logf("usage.handle wrote %d row", n)
}

// The rebrand renamed the default DB file to cpa-token-analytics.sqlite;
// ensureDB copies a legacy usage-report.sqlite on first open without
// touching or deleting the original.
func TestLegacyDBMigration(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "usage-report.sqlite")
	fresh := filepath.Join(dir, "cpa-token-analytics.sqlite")
	want := []byte("sqlite-legacy-payload")
	if err := os.WriteFile(legacy, want, 0o600); err != nil {
		t.Fatal(err)
	}
	migrateLegacyDB(legacy, fresh)
	got, err := os.ReadFile(fresh)
	if err != nil || string(got) != string(want) {
		t.Fatalf("migrated db: %q err=%v", got, err)
	}
	if orig, _ := os.ReadFile(legacy); string(orig) != string(want) {
		t.Fatal("legacy db must be left untouched")
	}
	// Second run must not overwrite an existing new file.
	other := []byte("sqlite-new-payload")
	if err := os.WriteFile(fresh, other, 0o600); err != nil {
		t.Fatal(err)
	}
	migrateLegacyDB(legacy, fresh)
	if got, _ := os.ReadFile(fresh); string(got) != string(other) {
		t.Fatal("migration must not overwrite an existing new db")
	}
}

func TestApplyConfig(t *testing.T) {
	tmp := t.TempDir()
	direct := []byte(`{"config_yaml":"enabled: true\ndb_path: \"` + tmp + `/direct.sqlite\"\n"}`)
	applyConfig(direct)
	if lastOpened != tmp+"/direct.sqlite" {
		t.Fatalf("direct shape: got %q", lastOpened)
	}
	wrapped := []byte(`{"result":{"config_yaml":"db_path: ` + tmp + `/wrapped.sqlite"}}`)
	applyConfig(wrapped)
	if lastOpened != tmp+"/wrapped.sqlite" {
		t.Fatalf("wrapped shape: got %q", lastOpened)
	}
}

func TestManagementRegisterRoutes(t *testing.T) {
	raw, err := handleMethod("management.register", nil)
	if err != nil {
		t.Fatal(err)
	}
	var e envelope
	if err := json.Unmarshal(raw, &e); err != nil || !e.OK {
		t.Fatalf("register: %s %v", raw, err)
	}
	var reg struct {
		Routes []struct {
			Method string
			Path   string
		}
		Resources []struct {
			Path string
		}
	}
	if err := json.Unmarshal(e.Result, &reg); err != nil {
		t.Fatal(err)
	}
	if len(reg.Routes) != 19 {
		t.Fatalf("expect 19 authenticated routes, got %d", len(reg.Routes))
	}
	for _, r := range reg.Routes {
		if r.Method != "GET" && r.Method != "POST" {
			t.Fatalf("route %s missing explicit method", r.Path)
		}
		if !strings.HasPrefix(r.Path, "/plugins/cpa-token-analytics/api/") {
			t.Fatalf("route outside plugin namespace: %s", r.Path)
		}
	}
	for _, r := range reg.Resources {
		if strings.Contains(r.Path, "/api/") {
			t.Fatalf("dynamic api must not be registered as resource: %s", r.Path)
		}
	}
}
