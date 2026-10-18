package main

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedRecord stores one event through the real usage.handle path.
// Keep timestamps distinct per test (or vary fields) — dedup drops identical payloads.
func seedRecord(t *testing.T, ts time.Time, fields map[string]any) {
	t.Helper()
	rec := map[string]any{
		"Model":       "m",
		"Source":      "sk-fixture",
		"RequestedAt": ts.Format(time.RFC3339Nano),
	}
	for k, v := range fields {
		rec[k] = v
	}
	body, err := json.Marshal(map[string]any{"result": rec})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handleMethod("usage.handle", body); err != nil {
		t.Fatal(err)
	}
}

func callMgmtQuery(t *testing.T, path, method string, q map[string][]string) (int, []byte) {
	t.Helper()
	req, _ := json.Marshal(map[string]interface{}{
		"result": map[string]interface{}{
			"Method":  method,
			"Path":    path,
			"Query":   q,
			"Headers": map[string][]string{},
			"Body":    nil,
		},
	})
	raw, err := handleMethod("management.handle", req)
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
	return resp.StatusCode, resp.Body
}

func getAnalytics(t *testing.T, name string, q map[string][]string) map[string]any {
	t.Helper()
	status, body := callMgmtQuery(t, "/v0/management/plugins/cpa-token-analytics/api/analytics/"+name, "GET", q)
	if status != 200 {
		t.Fatalf("analytics/%s: %d %s", name, status, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return out
}

func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

func TestAnalyticsOverview(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "ov.sqlite"))
	now := time.Now()
	seedRecord(t, now.Add(-2*time.Hour), map[string]any{
		"Model": "m-a", "Latency": int64(1_000_000_000), "TTFT": int64(100_000_000),
		"Detail": map[string]int64{"InputTokens": 50, "OutputTokens": 100, "TotalTokens": 150},
	})
	seedRecord(t, now.Add(-1*time.Hour), map[string]any{
		"Model": "m-a", "Latency": int64(3_000_000_000), // TTFT absent → excluded from ttft.n
		"Detail": map[string]int64{"InputTokens": 100, "OutputTokens": 300, "TotalTokens": 400},
	})
	seedRecord(t, now.Add(-30*time.Minute), map[string]any{
		"Model": "m-b", "Failed": true, "TTFT": int64(200_000_000), // no latency
		"Detail": map[string]int64{"InputTokens": 10, "OutputTokens": 5},
	})

	resp := getAnalytics(t, "overview", nil)
	totals := resp["totals"].(map[string]any)
	if num(totals["requests"]) != 3 || num(totals["failed"]) != 1 || num(totals["succeeded"]) != 2 {
		t.Fatalf("totals: %v", totals)
	}
	if got := num(totals["success_rate"]); got < 0.66 || got > 0.67 {
		t.Fatalf("success_rate=%v", got)
	}
	if num(totals["input"]) != 160 || num(totals["output"]) != 405 || num(totals["total"]) != 565 {
		t.Fatalf("token totals: %v", totals)
	}
	lat := resp["latency"].(map[string]any)
	if num(lat["n"]) != 2 || num(lat["avg"]) != 2000 || num(lat["p50"]) != 1000 || num(lat["p95"]) != 3000 {
		t.Fatalf("latency: %v", lat)
	}
	ttft := resp["ttft"].(map[string]any)
	if num(ttft["n"]) != 2 || num(ttft["avg"]) != 150 || num(ttft["p95"]) != 200 {
		t.Fatalf("ttft must ignore absent/zero samples: %v", ttft)
	}
	cov := resp["coverage"].(map[string]any)
	if num(cov["events_with_latency"]) != 2 || num(cov["events_with_ttft"]) != 2 {
		t.Fatalf("coverage: %v", cov)
	}
	tps := resp["tok_per_sec"].(map[string]any)
	if num(tps["n"]) != 2 || num(tps["avg"]) != 100 {
		t.Fatalf("tok_per_sec: %v (expect avg 100 over 2 valid events)", tps)
	}
	if num(resp["skipped"]) != 0 {
		t.Fatalf("skipped: %v", resp["skipped"])
	}
	if resp["auth_type"] != "unknown" {
		t.Fatalf("auth_type: %v", resp["auth_type"])
	}
	if resp["timezone"] == "" || resp["collected_at"] == nil {
		t.Fatal("missing timezone/collected_at")
	}
}

func TestAnalyticsPerformance(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "perf.sqlite"))
	now := time.Now()
	for i := 0; i < 2; i++ {
		seedRecord(t, now.Add(time.Duration(-2-i)*time.Hour), map[string]any{
			"Model": "m-fast", "Latency": int64(500_000_000), "TTFT": int64(50_000_000),
			"Detail": map[string]int64{"OutputTokens": 50},
		})
	}
	seedRecord(t, now.Add(-30*time.Minute), map[string]any{
		"Model": "m-slow", "Latency": int64(4_000_000_000), "Failed": true,
		"Detail": map[string]int64{"OutputTokens": 80},
	})

	resp := getAnalytics(t, "performance", map[string][]string{"bucket": {"day"}})
	perModel := resp["per_model"].([]any)
	if len(perModel) != 2 {
		t.Fatalf("per_model: %v", perModel)
	}
	first := perModel[0].(map[string]any)
	if first["model"] != "m-fast" || num(first["requests"]) != 2 {
		t.Fatalf("per_model ordering (requests desc): %v", perModel)
	}
	second := perModel[1].(map[string]any)
	if second["model"] != "m-slow" || num(second["failed"]) != 1 || num(second["success_rate"]) != 0 {
		t.Fatalf("per_model m-slow: %v", second)
	}
	if num(second["avg_output"]) != 80 {
		t.Fatalf("avg_output: %v", second)
	}
	trend := resp["trend"].([]any)
	if len(trend) == 0 {
		t.Fatal("empty trend")
	}
	tr := trend[len(trend)-1].(map[string]any)
	if tr["label"] == nil || num(tr["requests"]) == 0 {
		t.Fatalf("trend entry: %v", tr)
	}
	if bad, _ := callMgmtQuery(t, "/v0/management/plugins/cpa-token-analytics/api/analytics/nope", "GET", nil); bad != 404 {
		t.Fatalf("unknown analytics route: %d", bad)
	}
}

func TestAnalyticsComparison(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "cmp.sqlite"))
	now := time.Now()
	y, m, d := now.Date()
	today0 := time.Date(y, m, d, 0, 0, 0, 0, time.Local)
	elapsed := now.Sub(today0)
	seedRecord(t, now, map[string]any{
		"Detail": map[string]int64{"InputTokens": 10, "OutputTokens": 5},
	})
	seedRecord(t, today0.AddDate(0, 0, -1), map[string]any{ // yesterday 00:00 — inside elapsed iff elapsed>0
		"Detail": map[string]int64{"InputTokens": 20, "OutputTokens": 10},
	})
	seedRecord(t, today0.Add(-12*time.Hour), map[string]any{ // yesterday noon — elapsed only after noon
		"Detail": map[string]int64{"InputTokens": 4, "OutputTokens": 2},
	})
	// Event at yesterday midnight is inside elapsed iff elapsed>0; the
	// yesterday-noon event joins only when elapsed>12h.
	wantPrevElapsed := 1.0
	if elapsed > 12*time.Hour {
		wantPrevElapsed = 2
	}
	el := getAnalytics(t, "comparison", map[string][]string{"preset": {"today"}, "mode": {"elapsed"}})
	metrics := el["metrics"].(map[string]any)
	req := metrics["requests"].(map[string]any)
	if num(req["current"]) != 1 || num(req["previous"]) != wantPrevElapsed {
		t.Fatalf("elapsed requests: %v (elapsed=%v)", req, elapsed)
	}
	if el["preset"] != "today" || el["mode"] != "elapsed" {
		t.Fatalf("echo: %v %v", el["preset"], el["mode"])
	}

	full := getAnalytics(t, "comparison", map[string][]string{"preset": {"today"}, "mode": {"full"}})
	freq := full["metrics"].(map[string]any)["requests"].(map[string]any)
	if num(freq["previous"]) != 2 {
		t.Fatalf("full previous must include both events: %v", freq)
	}
	if num(freq["delta"]) != -1 || num(freq["pct_change"]) != -50 {
		t.Fatalf("full delta/pct: %v", freq)
	}
	prev := full["previous"].(map[string]any)
	if prev["start"] == nil || prev["end"] == nil || prev["label"] == nil {
		t.Fatalf("previous block: %v", prev)
	}

	// previous=0 → pct_change null (even when current>0)
	empty := getAnalytics(t, "comparison", map[string][]string{
		"preset": {"custom"}, "custom_start": {"2020-01-01"}, "custom_end": {"2020-01-02"},
	})
	em := empty["metrics"].(map[string]any)["requests"].(map[string]any)
	if em["pct_change"] != nil || num(em["previous"]) != 0 {
		t.Fatalf("empty period must yield null pct_change: %v", em)
	}
	cost := empty["metrics"].(map[string]any)["cost_usd"].(map[string]any)
	if cost["current"] != nil {
		t.Fatalf("cost_usd must be null until pricing lane lands: %v", cost)
	}
	sr := empty["metrics"].(map[string]any)["success_rate"].(map[string]any)
	if sr["current"] != nil || sr["delta"] != nil {
		t.Fatalf("undefined rate must be null: %v", sr)
	}

	status, body := callMgmtQuery(t, "/v0/management/plugins/cpa-token-analytics/api/analytics/comparison", "GET",
		map[string][]string{"preset": {"bogus"}})
	if status != 400 {
		t.Fatalf("bad preset: %d %s", status, body)
	}
}

// TestComparisonAvgLatency is the regression for a panic: avg_latency went
// through a *float64 type assertion on a plain float64 map value. One event
// with latency inside the current period must produce a numeric avg_latency.
func TestComparisonAvgLatency(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "cmplat.sqlite"))
	now := time.Now()
	y, m, d := now.Date()
	today0 := time.Date(y, m, d, 0, 0, 0, 0, time.Local)
	seedRecord(t, today0.Add(time.Hour), map[string]any{
		"Latency": int64(2_000_000_000), // 2000 ms
		"Detail":  map[string]int64{"InputTokens": 10, "OutputTokens": 5},
	})
	seedRecord(t, today0.AddDate(0, 0, -1).Add(time.Hour), map[string]any{
		"Latency": int64(4_000_000_000), // yesterday 4000 ms
		"Detail":  map[string]int64{"InputTokens": 20, "OutputTokens": 10},
	})

	resp := getAnalytics(t, "comparison", map[string][]string{"preset": {"today"}, "mode": {"full"}})
	lat := resp["metrics"].(map[string]any)["avg_latency"].(map[string]any)
	if num(lat["current"]) != 2000 || num(lat["previous"]) != 4000 {
		t.Fatalf("avg_latency: %v", lat)
	}
	if num(lat["delta"]) != -2000 {
		t.Fatalf("avg_latency delta: %v", lat)
	}
}

// TestComparisonPeriodsElapsed checks the start-aligned elapsed previous
// window: preset=today at 09:00 → prev = [yesterday 00:00, yesterday 09:00),
// and a fully-past period keeps the complete previous span (mode=full shape).
func TestComparisonPeriodsElapsed(t *testing.T) {
	now := time.Date(2026, 10, 17, 9, 0, 0, 0, time.Local)
	cur, prev, err := comparisonPeriods("today", "elapsed", "", "", now)
	if err != nil {
		t.Fatal(err)
	}
	today0 := time.Date(2026, 10, 17, 0, 0, 0, 0, time.Local)
	wantPS, wantPE := today0.AddDate(0, 0, -1), today0.AddDate(0, 0, -1).Add(9*time.Hour)
	if !cur.start.Equal(today0) || !cur.end.Equal(today0.AddDate(0, 0, 1)) {
		t.Fatalf("current: %v..%v", cur.start, cur.end)
	}
	if !prev.start.Equal(wantPS) || !prev.end.Equal(wantPE) {
		t.Fatalf("elapsed prev must start-align: %v..%v want %v..%v", prev.start, prev.end, wantPS, wantPE)
	}
	if prev.end.Sub(prev.start) != 9*time.Hour {
		t.Fatalf("elapsed prev length: %v", prev.end.Sub(prev.start))
	}

	// Fully-past period → previous is the complete preceding span.
	y0 := today0.AddDate(0, 0, -1)
	_, prev2, err := comparisonPeriods("yesterday", "elapsed", "", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if !prev2.start.Equal(y0.AddDate(0, 0, -1)) || !prev2.end.Equal(y0) {
		t.Fatalf("past period elapsed must equal full prev: %v..%v", prev2.start, prev2.end)
	}

	// Future custom range → empty (non-negative) previous window.
	_, prev3, err := comparisonPeriods("custom", "elapsed", "2027-01-01", "2027-01-02", now)
	if err != nil {
		t.Fatal(err)
	}
	if prev3.end.Before(prev3.start) {
		t.Fatalf("future range must not yield negative prev: %v..%v", prev3.start, prev3.end)
	}
}

func TestAnalyticsHeatmap(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "hm.sqlite"))
	// 2026-09-10 is a Thursday → dow = 3.
	ts := time.Date(2026, 9, 10, 15, 30, 0, 0, time.Local)
	seedRecord(t, ts, map[string]any{
		"Detail": map[string]int64{"InputTokens": 7, "OutputTokens": 3},
	})
	seedRecord(t, ts.Add(15*time.Minute), map[string]any{
		"Detail": map[string]int64{"InputTokens": 1, "OutputTokens": 1},
	})
	// Outside the queried range: ignored.
	seedRecord(t, ts.AddDate(0, 0, 2), map[string]any{
		"Detail": map[string]int64{"InputTokens": 9, "OutputTokens": 9},
	})

	resp := getAnalytics(t, "heatmap", map[string][]string{
		"start": {"2026-09-10"}, "end": {"2026-09-10"},
	})
	dow := resp["dow_hours"].(map[string]any)
	reqs := dow["requests"].([]any)
	toks := dow["tokens"].([]any)
	costs := dow["cost_usd"].([]any)
	if len(reqs) != 7 || len(reqs[3].([]any)) != 24 {
		t.Fatalf("heatmap shape: %d days", len(reqs))
	}
	if num(reqs[3].([]any)[15]) != 2 {
		t.Fatalf("requests[dow=3][hour=15]: %v", reqs[3])
	}
	if num(toks[3].([]any)[15]) != 12 {
		t.Fatalf("tokens cell: %v", toks[3])
	}
	zeroCost := true
	for _, day := range costs {
		for _, c := range day.([]any) {
			if num(c) != 0 {
				zeroCost = false
			}
		}
	}
	if !zeroCost {
		t.Fatal("cost_usd must be all zeros before pricing lane")
	}
	max := resp["max"].(map[string]any)
	if num(max["requests"]) != 2 || num(max["tokens"]) != 12 {
		t.Fatalf("max: %v", max)
	}
	if resp["timezone"] == "" {
		t.Fatal("missing timezone")
	}
}

func TestAnalyticsAuthType(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "auth.sqlite"))
	now := time.Now()
	seedRecord(t, now.Add(-time.Hour), map[string]any{"ExecutorType": "GeminiOAuthExecutor"})
	seedRecord(t, now.Add(-30*time.Minute), map[string]any{"AuthType": "api-key"})
	seedRecord(t, now.Add(-10*time.Minute), map[string]any{"StatusCode": 429})

	today := time.Now().Format("2006-01-02")
	_, body := callMgmtQuery(t, "/v0/management/plugins/cpa-token-analytics/api/usage", "GET",
		map[string][]string{"start": {today}, "end": {today}})
	var usage struct {
		AuthType string                   `json:"auth_type"`
		Events   []map[string]interface{} `json:"events"`
	}
	if err := json.Unmarshal(body, &usage); err != nil {
		t.Fatal(err)
	}
	if usage.AuthType != "mixed" {
		t.Fatalf("top-level auth_type: %v", usage.AuthType)
	}
	seen := map[string]bool{}
	sawStatus := false
	for _, ev := range usage.Events {
		if v, ok := ev["auth_type"].(string); ok {
			seen[v] = true
		}
		if num(ev["status_code"]) == 429 {
			sawStatus = true
		}
		if _, hasCW := ev["cache_write"]; !hasCW {
			t.Fatal("event missing cache_write")
		}
	}
	if !seen["oauth"] || !seen["api-key"] {
		t.Fatalf("event auth_type values: %v", seen)
	}
	if !sawStatus {
		t.Fatal("status_code not surfaced")
	}
	ov := getAnalytics(t, "overview", nil)
	if ov["auth_type"] != "mixed" {
		t.Fatalf("overview auth_type: %v", ov["auth_type"])
	}

	// Single-value collapses to that value; none → unknown.
	openDB(filepath.Join(t.TempDir(), "auth2.sqlite"))
	seedRecord(t, now, map[string]any{"AccountType": "subscription"})
	ov = getAnalytics(t, "overview", nil)
	if ov["auth_type"] != "subscription" {
		t.Fatalf("single auth_type: %v", ov["auth_type"])
	}
	openDB(filepath.Join(t.TempDir(), "auth3.sqlite"))
	ov = getAnalytics(t, "overview", nil)
	if ov["auth_type"] != "unknown" {
		t.Fatalf("empty auth_type: %v", ov["auth_type"])
	}
}

func TestOptionalFieldsKeepDedup(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "dedup.sqlite"))
	seedRecord(t, time.Now(), map[string]any{
		"Provider": "codex", "StatusCode": 200, "AuthType": "oauth",
	})
	var payload, dedup string
	if err := db.QueryRow("SELECT payload, dedup FROM usage_events").Scan(&payload, &dedup); err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal([]byte(payload), &stored); err != nil {
		t.Fatal(err)
	}
	if stored["status_code"] != float64(200) || stored["auth_type"] != "oauth" {
		t.Fatalf("optional fields missing: %s", payload)
	}
	for _, k := range []string{"provider", "executor_type", "status_code", "auth_type"} {
		delete(stored, k)
	}
	legacy, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha1.Sum(legacy)
	if hex.EncodeToString(digest[:]) != dedup {
		t.Fatal("optional fields changed dedup identity")
	}
}

func TestTsUnixBackfill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec("CREATE TABLE usage_events (id INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL, dedup TEXT UNIQUE, payload TEXT)"); err != nil {
		t.Fatal(err)
	}
	withTs := `{"timestamp":"2026-09-10T11:30:00+08:00","model":"m"}`
	withoutTs := `{"model":"m"}`
	if _, err := conn.Exec("INSERT INTO usage_events (ts,dedup,payload) VALUES (?,?,?)",
		"2026-09-10 03:30:00", "d1", withTs); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec("INSERT INTO usage_events (ts,dedup,payload) VALUES (?,?,?)",
		"2026-09-11 08:00:00", "d2", withoutTs); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()

	openDB(path) // runs runMigrations incl. ALTER + backfill

	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM usage_events WHERE ts_unix IS NULL").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d rows left unbackfilled", n)
	}
	var ts1 float64
	if err := db.QueryRow("SELECT ts_unix FROM usage_events WHERE dedup='d1'").Scan(&ts1); err != nil {
		t.Fatal(err)
	}
	want1, _ := parseAnyTime("2026-09-10T11:30:00+08:00")
	if ts1 != want1 {
		t.Fatalf("d1 ts_unix=%v want %v (payload timestamp must win)", ts1, want1)
	}
	var ts2 float64
	if err := db.QueryRow("SELECT ts_unix FROM usage_events WHERE dedup='d2'").Scan(&ts2); err != nil {
		t.Fatal(err)
	}
	want2, _ := parseAnyTime("2026-09-11 08:00:00")
	if ts2 != want2 {
		t.Fatalf("d2 ts_unix=%v want %v (stored ts fallback)", ts2, want2)
	}
	// Pricing tables exist after migration.
	for _, tbl := range []string{"plugin_meta", "price_catalog", "price_snapshots", "price_overrides"} {
		if err := db.QueryRow("SELECT COUNT(*) FROM " + tbl).Scan(&n); err != nil {
			t.Fatalf("missing table %s: %v", tbl, err)
		}
	}
	// Idempotent: second run must not fail.
	if err := runMigrations(db); err != nil {
		t.Fatalf("migrations not idempotent: %v", err)
	}
}

// TestTsUnixBackfillUnparseable: a batch where every row fails to parse must
// terminate the backfill loop (previously it committed an empty tx, re-read
// the same rows and looped forever). Per contract B2/R3 unparseable rows now
// get the ts_unix=-1 sentinel (excluded from ranges, never rescanned) —
// NULL is reserved for "not yet processed".
func TestTsUnixBackfillUnparseable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "junk.sqlite")
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec("CREATE TABLE usage_events (id INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL, dedup TEXT UNIQUE, payload TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec("INSERT INTO usage_events (ts,dedup,payload) VALUES (?,?,?)",
		"not-a-time", "j1", "{not json"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec("INSERT INTO usage_events (ts,dedup,payload) VALUES (?,?,?)",
		"2026-13-40 99:99:99", "j2", `{"model":"m"}`); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- runMigrations(conn) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runMigrations: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runMigrations hung on an all-unparseable batch")
	}
	var n int
	if err := conn.QueryRow("SELECT COUNT(*) FROM usage_events WHERE ts_unix = -1").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("unparseable rows must get the -1 sentinel: %d", n)
	}
	var nulls int
	if err := conn.QueryRow("SELECT COUNT(*) FROM usage_events WHERE ts_unix IS NULL").Scan(&nulls); err != nil {
		t.Fatal(err)
	}
	if nulls != 0 {
		t.Fatalf("no row may remain unprocessed after migration: %d NULL", nulls)
	}
	_ = conn.Close()
}

func TestUsageTsUnixPrefilter(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "pre.sqlite"))
	seedRecord(t, time.Date(2026, 9, 10, 12, 0, 0, 0, time.Local), map[string]any{"Model": "in-range"})
	seedRecord(t, time.Date(2026, 9, 12, 12, 0, 0, 0, time.Local), map[string]any{"Model": "out-range"})
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM usage_events WHERE ts_unix IS NOT NULL").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("handleUsage must populate ts_unix, got %d", n)
	}
	status, body := callMgmtQuery(t, "/v0/management/plugins/cpa-token-analytics/api/usage", "GET",
		map[string][]string{"start": {"2026-09-10"}, "end": {"2026-09-10"}})
	if status != 200 {
		t.Fatalf("usage: %d %s", status, body)
	}
	var resp struct {
		Events []map[string]interface{} `json:"events"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Events) != 1 || resp.Events[0]["model"] != "in-range" {
		t.Fatalf("ts_unix prefilter: %v", resp.Events)
	}
}

// ============================================================
// Audit-fix P2-BE tests (contract B2, issue 5): calendar
// today/month blocks on /analytics/cost + calendar-month
// comparison periods.
// ============================================================

// withUTC pins time.Local so calendar-boundary logic is deterministic.
// (time.Local is a package var — t.Setenv("TZ") would be unreliable once
// the local zone has already been initialized by an earlier test.)
func withUTC(t *testing.T) {
	t.Helper()
	old := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = old })
}

// TestComparisonPeriodsMonth covers contract B2 calendar semantics for
// preset=month: previous is the previous calendar month (full), or
// [prev-month-start, prev-month-start+elapsed) capped at the month
// boundary (elapsed) — across 28/29/31-day months and year rollover.
func TestComparisonPeriodsMonth(t *testing.T) {
	withUTC(t)
	d := func(y int, m time.Month, day, h int) time.Time {
		return time.Date(y, m, day, h, 0, 0, 0, time.Local)
	}

	// Mid-month: elapsed shorter than the previous month → ps+elapsed.
	now := d(2025, 3, 15, 12)
	cur, prev, err := comparisonPeriods("month", "elapsed", "", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if !cur.start.Equal(d(2025, 3, 1, 0)) || !cur.end.Equal(d(2025, 3, 16, 0)) {
		t.Fatalf("month current: %v..%v", cur.start, cur.end)
	}
	elapsed := now.Sub(d(2025, 3, 1, 0)) // 14d12h
	if !prev.start.Equal(d(2025, 2, 1, 0)) || !prev.end.Equal(d(2025, 2, 1, 0).Add(elapsed)) {
		t.Fatalf("elapsed prev: %v..%v (elapsed=%v)", prev.start, prev.end, elapsed)
	}
	_, prevF, err := comparisonPeriods("month", "full", "", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if !prevF.start.Equal(d(2025, 2, 1, 0)) || !prevF.end.Equal(d(2025, 3, 1, 0)) {
		t.Fatalf("full prev must be the whole previous month: %v..%v", prevF.start, prevF.end)
	}

	// Leap February (2024): ps+elapsed lands inside the 29-day month.
	now = d(2024, 3, 5, 6)
	cur, prev, err = comparisonPeriods("month", "elapsed", "", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if !cur.start.Equal(d(2024, 3, 1, 0)) {
		t.Fatalf("leap current: %v", cur.start)
	}
	elapsed = now.Sub(d(2024, 3, 1, 0))     // 4d6h
	wantPE := d(2024, 2, 1, 0).Add(elapsed) // Feb 05 06:00 — no cap needed
	if !prev.start.Equal(d(2024, 2, 1, 0)) || !prev.end.Equal(wantPE) {
		t.Fatalf("leap elapsed prev: %v..%v want ..%v", prev.start, prev.end, wantPE)
	}
	_, prevF, err = comparisonPeriods("month", "full", "", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if !prevF.start.Equal(d(2024, 2, 1, 0)) || !prevF.end.Equal(d(2024, 3, 1, 0)) {
		t.Fatalf("leap full prev: %v..%v", prevF.start, prevF.end)
	}

	// Year rollover: January's previous month is December of last year.
	now = d(2025, 1, 10, 12)
	cur, prev, err = comparisonPeriods("month", "elapsed", "", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if !cur.start.Equal(d(2025, 1, 1, 0)) {
		t.Fatalf("jan current: %v", cur.start)
	}
	elapsed = now.Sub(d(2025, 1, 1, 0)) // 9d12h < 31d
	if !prev.start.Equal(d(2024, 12, 1, 0)) || !prev.end.Equal(d(2024, 12, 1, 0).Add(elapsed)) {
		t.Fatalf("year rollover prev: %v..%v", prev.start, prev.end)
	}
	_, prevF, err = comparisonPeriods("month", "full", "", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if !prevF.start.Equal(d(2024, 12, 1, 0)) || !prevF.end.Equal(d(2025, 1, 1, 0)) {
		t.Fatalf("year rollover full prev: %v..%v", prevF.start, prevF.end)
	}

	// elapsed longer than the previous month → cap at its end (current
	// start). Mar 31 12:00: elapsed 30d12h > Feb's 28d → prev = whole Feb.
	now = d(2025, 3, 31, 12)
	cur, prev, err = comparisonPeriods("month", "elapsed", "", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if !prev.start.Equal(d(2025, 2, 1, 0)) || !prev.end.Equal(cur.start) {
		t.Fatalf("elapsed must cap at prev month end: %v..%v", prev.start, prev.end)
	}

	// First instant of a month → zero elapsed → empty previous window.
	now = d(2025, 4, 1, 0)
	_, prev, err = comparisonPeriods("month", "elapsed", "", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if !prev.start.Equal(d(2025, 3, 1, 0)) || !prev.end.Equal(prev.start) {
		t.Fatalf("zero elapsed must yield empty prev: %v..%v", prev.start, prev.end)
	}

	// 7d stays duration-based (unchanged semantics): previous is the 7-day
	// window ending at the current start, truncated to the same elapsed
	// length (now - cs = 6d9h → prev ends 10-10 09:00).
	now = d(2026, 10, 17, 9)
	cur, prev, err = comparisonPeriods("7d", "elapsed", "", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if !cur.start.Equal(d(2026, 10, 11, 0)) || !cur.end.Equal(d(2026, 10, 18, 0)) {
		t.Fatalf("7d current: %v..%v", cur.start, cur.end)
	}
	wantPrevE := d(2026, 10, 4, 0).Add(now.Sub(d(2026, 10, 11, 0)))
	if !prev.start.Equal(d(2026, 10, 4, 0)) || !prev.end.Equal(wantPrevE) {
		t.Fatalf("7d elapsed prev: %v..%v want ..%v", prev.start, prev.end, wantPrevE)
	}
}

// TestAnalyticsComparisonMonthEndpoint: the endpoint emits a calendar
// previous month (day-1 aligned, ending at the current month start).
func TestAnalyticsComparisonMonthEndpoint(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "cmpmonth.sqlite"))
	now := time.Now()
	y, m, _ := now.Date()
	thisMonth := time.Date(y, m, 1, 0, 0, 0, 0, time.Local)
	prevMonth := thisMonth.AddDate(0, -1, 0)
	seedRecord(t, prevMonth.Add(6*time.Hour), map[string]any{
		"Detail": map[string]int64{"InputTokens": 10, "OutputTokens": 5},
	})

	resp := getAnalytics(t, "comparison", map[string][]string{
		"preset": {"month"}, "mode": {"full"},
	})
	cur := resp["current"].(map[string]any)
	prv := resp["previous"].(map[string]any)
	ps, err := time.Parse(time.RFC3339, prv["start"].(string))
	if err != nil {
		t.Fatal(err)
	}
	pe, err := time.Parse(time.RFC3339, prv["end"].(string))
	if err != nil {
		t.Fatal(err)
	}
	cs, err := time.Parse(time.RFC3339, cur["start"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if !ps.Equal(prevMonth) || !pe.Equal(thisMonth) || !cs.Equal(thisMonth) {
		t.Fatalf("calendar month bounds: prev %v..%v cur %v", ps, pe, cs)
	}
	reqs := resp["metrics"].(map[string]any)["requests"].(map[string]any)
	if num(reqs["previous"]) != 1 {
		t.Fatalf("prev month must see the seeded event: %v", reqs)
	}
}

// TestAnalyticsCostTodayMonthBlocks: today/month must be computed from
// independent calendar ranges (contract B2), not from the selected range.
// Events outside the selected range but inside today/month must count in
// the blocks; events inside the selected range but outside today/month
// must not. usd is null when nothing priced; coverage counts are always
// present; the model filter applies to the blocks.
func TestAnalyticsCostTodayMonthBlocks(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "costtm.sqlite"))
	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		return nil, fmt.Errorf("no network")
	})
	mu.Lock()
	seedCatalog(t, [][6]string{
		{"m-tm", "codex", "models.dev", "1", "10", "2026-10-01T00:00:00Z"},
	})
	mu.Unlock()

	now := time.Now()
	y, m, d := now.Date()
	today0 := time.Date(y, m, d, 0, 0, 0, 0, time.Local)
	month0 := time.Date(y, m, 1, 0, 0, 0, 0, time.Local)
	todayEnd := today0.AddDate(0, 0, 1)
	monthEnd := month0.AddDate(0, 1, 0)
	prevMonth1 := month0.AddDate(0, -1, 0)

	price := func(in, out int64) float64 { return (float64(in)*1 + float64(out)*10) / 1e6 }
	seed := func(ts time.Time, model string, in, out int64) time.Time {
		seedRecord(t, ts, map[string]any{
			"Provider": "codex", "Model": model,
			"Detail": map[string]int64{"InputTokens": in, "OutputTokens": out},
		})
		return ts
	}
	sToday := seed(today0.Add(time.Hour), "m-tm", 1000, 100)
	sMonth := seed(month0.Add(2*time.Hour), "m-tm", 2000, 50)
	seed(today0.Add(3*time.Hour), "m-none", 5, 5)              // unpriced, today
	sOut := seed(prevMonth1.Add(6*time.Hour), "m-tm", 500, 50) // in selected range only

	inSpan := func(ts, lo, hi time.Time) bool { return !ts.Before(lo) && ts.Before(hi) }

	// Selected range covers ONLY sOut (a single day in the previous month):
	// today/month blocks must still see the events outside it.
	resp := getAnalytics(t, "cost", map[string][]string{
		"start": {prevMonth1.Format("2006-01-02")},
		"end":   {prevMonth1.Format("2006-01-02")},
	})

	totals := resp["totals"].(map[string]any)
	if math.Abs(num(totals["priced_usd"])-price(500, 50)) > 1e-12 ||
		num(totals["priced_events"]) != 1 {
		t.Fatalf("selected-range totals must cover only sOut: %v", totals)
	}

	today := resp["today"].(map[string]any)
	month := resp["month"].(map[string]any)
	wantTodayUSD := 0.0
	wantTodayTotal := 0.0
	for _, s := range []struct {
		ts      time.Time
		usd     float64
		matched bool
	}{{sToday, price(1000, 100), true}, {sMonth, price(2000, 50), true}, {sOut, price(500, 50), true}} {
		if inSpan(s.ts, today0, todayEnd) {
			wantTodayTotal++
			if s.matched {
				wantTodayUSD += s.usd
			}
		}
	}
	wantTodayTotal++ // the m-none event sits inside today
	// sMonth lands inside today only on the 1st of the month.
	wantTodayPriced := 0.0
	if inSpan(sToday, today0, todayEnd) {
		wantTodayPriced++
	}
	if d == 1 {
		wantTodayPriced++ // sMonth is also a today event on day 1
	}
	if math.Abs(num(today["usd"])-wantTodayUSD) > 1e-12 {
		t.Fatalf("today.usd: %v want %v", today["usd"], wantTodayUSD)
	}
	if num(today["total_events"]) != wantTodayTotal || num(today["priced_events"]) != wantTodayPriced {
		t.Fatalf("today coverage: %v (total=%v priced=%v)", today, wantTodayTotal, wantTodayPriced)
	}
	wantMonthUSD := price(1000, 100) + price(2000, 50)
	wantMonthTotal := 2.0
	if inSpan(today0.Add(3*time.Hour), month0, monthEnd) {
		wantMonthTotal++ // unpriced event belongs to the month too
	}
	if math.Abs(num(month["usd"])-wantMonthUSD) > 1e-12 {
		t.Fatalf("month.usd: %v want %v", month["usd"], wantMonthUSD)
	}
	if num(month["priced_events"]) != 2 || num(month["total_events"]) != wantMonthTotal {
		t.Fatalf("month coverage: %v", month)
	}
	// Legacy scalars mirror the blocks (null-safe back-compat).
	if math.Abs(num(resp["today_usd"])-wantTodayUSD) > 1e-12 ||
		math.Abs(num(resp["month_usd"])-wantMonthUSD) > 1e-12 {
		t.Fatalf("legacy scalars: %v %v", resp["today_usd"], resp["month_usd"])
	}

	// model filter applies to the blocks: m-tm keeps only priced events.
	resp = getAnalytics(t, "cost", map[string][]string{
		"start": {prevMonth1.Format("2006-01-02")},
		"end":   {prevMonth1.Format("2006-01-02")},
		"model": {"m-tm"},
	})
	today = resp["today"].(map[string]any)
	month = resp["month"].(map[string]any)
	if num(today["total_events"]) != wantTodayTotal-1 ||
		num(month["total_events"]) != wantMonthTotal-1 {
		t.Fatalf("model filter must drop m-none from blocks: %v %v", today, month)
	}
	if math.Abs(num(month["usd"])-wantMonthUSD) > 1e-12 {
		t.Fatalf("filtered month.usd: %v", month["usd"])
	}

	// model filter to the unpriced model → blocks exist, usd null.
	resp = getAnalytics(t, "cost", map[string][]string{"model": {"m-none"}})
	today = resp["today"].(map[string]any)
	if today["usd"] != nil || num(today["total_events"]) != 1 || num(today["priced_events"]) != 0 {
		t.Fatalf("unpriced-only today must carry null usd: %v", today)
	}
	if resp["today_usd"] != nil {
		t.Fatalf("legacy today_usd must mirror null: %v", resp["today_usd"])
	}
	month = resp["month"].(map[string]any)
	if month["usd"] != nil || num(month["total_events"]) != 1 {
		t.Fatalf("unpriced-only month must carry null usd: %v", month)
	}
}

// ============================================================
// Audit-fix P3-A tests (contract B2 issue 6): -1 sentinel
// backfill + NULL safety-net prefilter.
// ============================================================

// seedLegacyRow inserts one usage_events row directly, bypassing
// handleUsage: stored ts text, dedup key, payload, and an explicit ts_unix
// (nil = NULL). Returns the row id.
func seedLegacyRow(t *testing.T, conn *sql.DB, ts, dedup, payload string, tsUnix any) int64 {
	t.Helper()
	res, err := conn.Exec("INSERT INTO usage_events (ts,dedup,payload,ts_unix) VALUES (?,?,?,?)",
		ts, dedup, payload, tsUnix)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestTsUnixBackfillSentinel: junk rows scattered in front of valid rows
// (more than one batch worth) must get ts_unix=-1 while every valid row —
// including those sitting after a full junk batch — is backfilled. The old
// loop returned early on an all-junk batch, leaving valid rows NULL
// forever; the new loop terminates only on an empty/scan-failed batch.
func TestTsUnixBackfillSentinel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sentinel.sqlite")
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec("CREATE TABLE usage_events (id INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL, dedup TEXT UNIQUE, payload TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec("ALTER TABLE usage_events ADD COLUMN ts_unix REAL"); err != nil {
		t.Fatal(err)
	}
	// 501 junk rows before any valid row: the first batch is all-junk even
	// after ORDER BY id, which is exactly what broke the old early return.
	const junkN = 501
	junkTs := []string{"not-a-time", "", "2026-13-40 99:99:99"}
	junkPayloads := []string{"{not json", `{"timestamp":"bogus","model":"m"}`, `{"model":"m"}`}
	tx, err := conn.Begin()
	if err != nil {
		t.Fatal(err)
	}
	st, err := tx.Prepare("INSERT INTO usage_events (ts,dedup,payload) VALUES (?,?,?)")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < junkN; i++ {
		if _, err := st.Exec(junkTs[i%len(junkTs)], fmt.Sprintf("j%d", i), junkPayloads[i%len(junkPayloads)]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.Exec("not-a-time", "interleaved", `{"model":"m"}`); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// Valid rows AFTER the junk batch: payload ts and stored-ts fallback.
	seedLegacyRow(t, conn, "2026-09-10 03:30:00", "v1", `{"timestamp":"2026-09-10T11:30:00+08:00","model":"m"}`, nil)
	seedLegacyRow(t, conn, "2026-09-11 08:00:00", "v2", `{"model":"m"}`, nil)
	// Rows in BETWEEN junk (same batch region): still must be reached.
	seedLegacyRow(t, conn, "2026-09-12 00:00:00", "v3", `{"timestamp":"2026-09-12T12:00:00Z","model":"m"}`, nil)

	if err := runMigrations(conn); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}
	var junk, nulls, valid int
	if err := conn.QueryRow("SELECT COUNT(*) FROM usage_events WHERE ts_unix = -1").Scan(&junk); err != nil {
		t.Fatal(err)
	}
	if junk != junkN+1 {
		t.Fatalf("sentinel count: %d want %d", junk, junkN+1)
	}
	if err := conn.QueryRow("SELECT COUNT(*) FROM usage_events WHERE ts_unix IS NULL").Scan(&nulls); err != nil {
		t.Fatal(err)
	}
	if nulls != 0 {
		t.Fatalf("%d rows left unprocessed (old code stopped at the all-junk batch)", nulls)
	}
	if err := conn.QueryRow("SELECT COUNT(*) FROM usage_events WHERE ts_unix > 0").Scan(&valid); err != nil {
		t.Fatal(err)
	}
	if valid != 3 {
		t.Fatalf("valid rows backfilled: %d want 3", valid)
	}
	// Re-run is a no-op (idempotent + resumable; sentinels never rescanned).
	if err := runMigrations(conn); err != nil {
		t.Fatalf("migration rerun: %v", err)
	}
	if err := conn.QueryRow("SELECT COUNT(*) FROM usage_events WHERE ts_unix = -1").Scan(&junk); err != nil {
		t.Fatal(err)
	}
	if junk != junkN+1 {
		t.Fatalf("rerun must not touch sentinels: %d", junk)
	}
	_ = conn.Close()
}

// TestTsUnixBackfillResumable: a partially-migrated DB (one row already
// migrated to a real ts, junk to -1) only processes remaining NULL rows.
func TestTsUnixBackfillResumable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resume.sqlite")
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec("CREATE TABLE usage_events (id INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL, dedup TEXT UNIQUE, payload TEXT, ts_unix REAL)"); err != nil {
		t.Fatal(err)
	}
	migrated := seedLegacyRow(t, conn, "2026-09-01 00:00:00", "m1", `{"model":"m"}`, 1111.0)
	seedLegacyRow(t, conn, "garbage", "s1", `{"model":"m"}`, -1.0)
	seedLegacyRow(t, conn, "2026-09-02 00:00:00", "u1", `{"model":"m"}`, nil)

	if err := runMigrations(conn); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}
	var got float64
	if err := conn.QueryRow("SELECT ts_unix FROM usage_events WHERE id = ?", migrated).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 1111.0 {
		t.Fatalf("already-migrated row must not be rewritten: %v", got)
	}
	var nulls, sent, parsed int
	if err := conn.QueryRow("SELECT COUNT(*) FROM usage_events WHERE ts_unix IS NULL").Scan(&nulls); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow("SELECT COUNT(*) FROM usage_events WHERE ts_unix = -1").Scan(&sent); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow("SELECT COUNT(*) FROM usage_events WHERE ts_unix > 0").Scan(&parsed); err != nil {
		t.Fatal(err)
	}
	if nulls != 0 || sent != 1 || parsed != 2 {
		t.Fatalf("resume: nulls=%d sentinel=%d parsed=%d", nulls, sent, parsed)
	}
	_ = conn.Close()
}

// TestQueryUsageRowsNullSafetyNet: with the index usable, range queries
// must return migrated in-range rows AND legacy NULL rows (payload-parse
// safety net) while dropping sentinel/out-of-range rows — all ordered by id
// with no duplicates (the UNION-ALL sets are disjoint on ts_unix IS NULL).
func TestQueryUsageRowsNullSafetyNet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nullnet.sqlite")
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec("CREATE TABLE usage_events (id INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL, dedup TEXT UNIQUE, payload TEXT)"); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	openDB(path) // migrations create ts_unix + index; table stays empty

	// All instants are built in time.Local so the [local Sep10, Sep11)
	// window holds in every TZ. Insertion order interleaves the classes so
	// ORDER BY id is exercised: legacy NULL (in-range via payload),
	// migrated out-of-range, sentinel, migrated in-range, legacy NULL
	// (out-of-range via payload).
	day10 := func(h int) time.Time { return time.Date(2026, 9, 10, h, 0, 0, 0, time.Local) }
	rfc := func(tm time.Time) string { return tm.Format(time.RFC3339Nano) }
	mu.Lock()
	legacyIn := seedLegacyRow(t, db, "2026-09-10 00:00:00", "ln1",
		fmt.Sprintf(`{"timestamp":%q,"model":"legacy"}`, rfc(day10(10))), nil)
	migratedOut := seedLegacyRow(t, db, "2026-09-12 00:00:00", "mo1",
		fmt.Sprintf(`{"timestamp":%q,"model":"m"}`, rfc(time.Date(2026, 9, 12, 10, 0, 0, 0, time.Local))),
		float64(time.Date(2026, 9, 12, 10, 0, 0, 0, time.Local).Unix()))
	sentinel := seedLegacyRow(t, db, "2026-09-10 01:00:00", "s1",
		fmt.Sprintf(`{"timestamp":%q,"model":"m"}`, rfc(day10(11))), -1.0)
	migratedIn := seedLegacyRow(t, db, "2026-09-10 02:00:00", "mi1",
		fmt.Sprintf(`{"timestamp":%q,"model":"m"}`, rfc(day10(12))), float64(day10(12).Unix()))
	legacyOut := seedLegacyRow(t, db, "2026-09-20 00:00:00", "lo1",
		fmt.Sprintf(`{"timestamp":%q,"model":"legacy"}`, rfc(time.Date(2026, 9, 20, 10, 0, 0, 0, time.Local))), nil)
	mu.Unlock()

	lo := float64(day10(0).Unix())
	hi := float64(time.Date(2026, 9, 11, 0, 0, 0, 0, time.Local).Unix())
	mu.Lock()
	rows, err := queryUsageRows(lo, hi)
	mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		var ts, payload string
		if err := rows.Scan(&id, &ts, &payload); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	// Index branch returns: legacy NULL rows (payload still to be checked by
	// the caller) + migrated in-range. Sentinel -1 and migrated out-of-range
	// must NOT appear. Order must be strictly ascending id and no id may
	// repeat (UNION ALL sets are disjoint on ts_unix IS NULL).
	want := []int64{legacyIn, migratedIn, legacyOut}
	if len(ids) != len(want) {
		t.Fatalf("ids=%v want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids=%v want %v (order/dup edge broken)", ids, want)
		}
	}
	for _, banned := range []int64{migratedOut, sentinel} {
		for _, got := range ids {
			if got == banned {
				t.Fatalf("row %d must not be returned by the prefilter: %v", banned, ids)
			}
		}
	}

	// The safety net: the legacy in-range row survives the payload filter in
	// loadEventsRange; the legacy out-of-range one is dropped there, and the
	// sentinel row never re-enters payload-parse territory (it was selected
	// by neither branch — its id must be absent above).
	events, skipped, err := loadEventsRange(lo, hi, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 0 {
		t.Fatalf("skipped=%d", skipped)
	}
	var gotIds []int64
	for _, e := range events {
		gotIds = append(gotIds, e.id)
	}
	wantIds := []int64{legacyIn, migratedIn}
	if len(gotIds) != len(wantIds) || gotIds[0] != wantIds[0] || gotIds[1] != wantIds[1] {
		t.Fatalf("loadEventsRange ids=%v want %v", gotIds, wantIds)
	}

	// After the backfill converts NULL → real ts, the same rows arrive via
	// the index path only.
	mu.Lock()
	if err := backfillTsUnix(db); err != nil {
		mu.Unlock()
		t.Fatal(err)
	}
	mu.Unlock()
	events, _, err = loadEventsRange(lo, hi, "", "")
	if err != nil {
		t.Fatal(err)
	}
	gotIds = gotIds[:0]
	for _, e := range events {
		gotIds = append(gotIds, e.id)
	}
	if len(gotIds) != len(wantIds) || gotIds[0] != wantIds[0] || gotIds[1] != wantIds[1] {
		t.Fatalf("post-backfill ids=%v want %v", gotIds, wantIds)
	}
}

// ============================================================
// Final-gate rework tests. Add-only.
// ============================================================

// Final-gate F2: a period containing ONLY partial-priced events must report
// cost_usd (the sum of the priced components), not null — consistent with
// totals.priced_usd / heatmap cost_usd / costRangeBlock.usd which all sum
// every matched estimate.
func TestComparisonCostPartial(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "fg2.sqlite"))
	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		return nil, fmt.Errorf("no network")
	})
	mu.Lock()
	// input price only → every event of this model is partial
	if _, err := db.Exec(`INSERT INTO price_catalog (model,provider,source,input_per_m,output_per_m,synced_at)
		VALUES ('m-part','p1','models.dev',1,NULL,'2026-10-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()
	now := time.Now()
	today0 := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	seedRecord(t, today0.Add(time.Hour), map[string]any{
		"Provider": "p1", "Model": "m-part",
		"Detail": map[string]int64{"InputTokens": 1000, "OutputTokens": 100},
	})
	seedRecord(t, today0.Add(2*time.Hour), map[string]any{
		"Provider": "p1", "Model": "m-part",
		"Detail": map[string]int64{"InputTokens": 500, "OutputTokens": 50},
	})

	resp := getAnalytics(t, "comparison", map[string][]string{
		"preset": {"today"}, "mode": {"full"},
	})
	cm := resp["metrics"].(map[string]any)["cost_usd"].(map[string]any)
	want := (1000*1 + 500*1) / 1e6 // input components only; output unpriced
	if cm["current"] == nil || math.Abs(num(cm["current"])-want) > 1e-12 {
		t.Fatalf("partial-only period must report cost_usd: %v want %v", cm, want)
	}
	if cm["previous"] != nil {
		t.Fatalf("empty previous period must stay null: %v", cm)
	}
	// The cost endpoint agrees on the same total (both already counted
	// matched-but-partial USD — the assertion guards parity, not a change).
	cost := getAnalytics(t, "cost", nil)
	if math.Abs(num(cost["totals"].(map[string]any)["priced_usd"])-want) > 1e-12 {
		t.Fatalf("totals.priced_usd must equal comparison cost_usd: %v", cost["totals"])
	}
}

// Final-gate F4: the analytics/cost wrapper must round-trip integer fields
// larger than float64 can hold exactly — decodePreservingNumbers decodes via
// UseNumber so digits survive the unmarshal→patch→marshal round-trip.
func TestCostResponsePreservesLargeInts(t *testing.T) {
	base := []byte(`{"currency":"USD","totals":{"priced_events":9007199254740993,"priced_usd":0.001},"big":18446744073709551615,"usd":1.5}`)
	m, err := decodePreservingNumbers(base)
	if err != nil {
		t.Fatal(err)
	}
	m["today_usd"] = nil // the wrapper's patch is irrelevant to number fidelity
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, frag := range []string{`"priced_events":9007199254740993`, `"big":18446744073709551615`, `"usd":1.5`} {
		if !strings.Contains(string(out), frag) {
			t.Fatalf("round-trip lost %s: %s", frag, out)
		}
	}
	// Numbers must be json.Number inside the decoded map, not float64.
	if n, ok := m["big"].(json.Number); !ok || n.String() != "18446744073709551615" {
		t.Fatalf("big must decode to json.Number: %#v", m["big"])
	}
	if n, ok := m["usd"].(json.Number); !ok || n.String() != "1.5" {
		t.Fatalf("usd must decode to json.Number: %#v", m["usd"])
	}
}

// Final-gate F5+F6: a [0, 8.8e9) scan never selects ts_unix=-1 sentinel rows,
// and a row whose payload column is NULL (Scan fails: NULL → string) counts
// toward skipped the same way unmarshal/normalize failures do.
func TestLoadEventsRangeSentinelAndScanSkip(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "fg56.sqlite"))
	ts := float64(time.Date(2026, 10, 10, 12, 0, 0, 0, time.Local).Unix())
	mu.Lock()
	good := seedLegacyRow(t, db, "2026-10-10 12:00:00", "g1",
		`{"timestamp":"2026-10-10T12:00:00Z","model":"m"}`, ts)
	// Sentinels fail the ts_unix>=lo bound at lo=0, so they never reach the
	// row scan (before the fix, lo=-4e11 selected them and payload-parse
	// dropped them one by one).
	sentinel := seedLegacyRow(t, db, "garbage", "s1", `{not json`, -1.0)
	// NULL ts_unix rows enter via the safety-net branch and fail Scan when
	// payload is NULL → skipped++.
	nullScan := seedLegacyRow(t, db, "2026-10-10 12:00:00", "n1", "", nil)
	if _, err := db.Exec(`UPDATE usage_events SET payload=NULL WHERE id=?`, nullScan); err != nil {
		t.Fatal(err)
	}
	// Same Scan failure via the range branch (real ts_unix, NULL payload).
	rangeScan := seedLegacyRow(t, db, "2026-10-10 12:00:00", "n2", "", ts)
	if _, err := db.Exec(`UPDATE usage_events SET payload=NULL WHERE id=?`, rangeScan); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()

	events, skipped, err := loadEventsRange(0, 8.8e9, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].id != good {
		t.Fatalf("events=%v want only id %d", events, good)
	}
	if skipped != 2 {
		t.Fatalf("scan failures must count toward skipped: %d want 2", skipped)
	}
	for _, e := range events {
		if e.id == sentinel || e.id == nullScan || e.id == rangeScan {
			t.Fatalf("row %d must not be returned", e.id)
		}
	}
}
