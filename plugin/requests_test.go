package main

// Tests for the request inspector endpoints (GET /api/requests and
// /api/requests/{id}). All fixtures go through the real usage.handle seed
// path or direct usage_events inserts for legacy rows; pricing fixtures use
// direct price_catalog / cost_estimates inserts (no network).

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubNet keeps detail-view lazy syncs (pricing auto-sync, fx sync) off the
// network: the background fetchers fail fast instead of dialing out.
// Swapping the fetch vars must not race an in-flight background sync kicked
// by an earlier test — waitFxIdle/waitPricingIdle drain both sides, and the
// cleanup drains again before restoring.
func stubNet(t *testing.T) {
	t.Helper()
	waitFxIdle(t)
	waitPricingIdle(t)
	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		return nil, fmt.Errorf("no network")
	})
	stubFxFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		return nil, fmt.Errorf("no network")
	})
	t.Cleanup(func() {
		waitFxIdle(t)
		waitPricingIdle(t)
	})
}

// getRequests decodes a GET /api/requests response (any status).
func getRequests(t *testing.T, q map[string][]string) (int, map[string]any) {
	t.Helper()
	status, body := callMgmtQuery(t, "/v0/management/plugins/cpa-token-analytics/api/requests", "GET", q)
	var out map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode list: %v body=%s", err, body)
		}
	}
	return status, out
}

func getRequestDetail(t *testing.T, id any) (int, map[string]any, []byte) {
	t.Helper()
	status, body := callMgmtQuery(t,
		fmt.Sprintf("/v0/management/plugins/cpa-token-analytics/api/requests/%v", id), "GET", nil)
	var out map[string]any
	if len(body) > 0 {
		_ = json.Unmarshal(body, &out)
	}
	return status, out, body
}

func requestIDs(resp map[string]any) []int64 {
	var ids []int64
	for _, it := range resp["items"].([]any) {
		ids = append(ids, int64(num(it.(map[string]any)["id"])))
	}
	return ids
}

// seedRequestPage writes n events one minute apart (oldest first) and
// returns their usage_events ids in insertion order.
func seedRequestPage(t *testing.T, n int, base time.Time) []int64 {
	t.Helper()
	for i := 0; i < n; i++ {
		seedRecord(t, base.Add(time.Duration(i)*time.Minute), map[string]any{
			"Model": fmt.Sprintf("m-%d", i%3),
			"Detail": map[string]int64{
				"InputTokens": int64(10 + i), "OutputTokens": int64(5 + i),
			},
		})
	}
	mu.Lock()
	defer mu.Unlock()
	var ids []int64
	rows, err := db.Query("SELECT id FROM usage_events ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func TestRequestListBasics(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "rl.sqlite"))
	now := time.Now()
	seedRecord(t, now.Add(-2*time.Hour), map[string]any{
		"Model": "m-a", "Provider": "codex",
		"Latency": int64(1_500_000_000), "TTFT": int64(350_000_000),
		"Detail": map[string]int64{"InputTokens": 10000, "CacheReadTokens": 3000, "OutputTokens": 1500, "TotalTokens": 11500},
	})
	// No latency / ttft / status / provider at all → nulls, not zeros.
	seedRecord(t, now.Add(-time.Hour), map[string]any{
		"Model":  "m-b",
		"Detail": map[string]int64{"InputTokens": 5, "OutputTokens": 5},
	})

	status, resp := getRequests(t, nil)
	if status != 200 {
		t.Fatalf("list: %d %v", status, resp)
	}
	if num(resp["limit"]) != 20 || len(resp["items"].([]any)) != 2 {
		t.Fatalf("items/limit: %v", resp)
	}
	if resp["has_more"] != false || resp["next_cursor"] != nil {
		t.Fatalf("page terminal: %v", resp)
	}
	first := resp["items"].([]any)[0].(map[string]any) // newest first
	second := resp["items"].([]any)[1].(map[string]any)
	if first["model"] != "m-b" || second["model"] != "m-a" {
		t.Fatalf("default sort must be newest-first: %v %v", first["model"], second["model"])
	}
	for _, k := range []string{"id", "ts", "model", "provider", "key", "failed", "status_code",
		"input", "cache", "cache_write", "output", "reasoning", "total",
		"latency_ms", "ttft_ms", "estimated_cost_usd", "pricing_status"} {
		if _, ok := first[k]; !ok {
			t.Fatalf("item missing key %q: %v", k, first)
		}
	}
	// Absent latency/ttft/status_code must serialize as null, never 0.
	if first["latency_ms"] != nil || first["ttft_ms"] != nil || first["status_code"] != nil {
		t.Fatalf("absent fields must be null: %v", first)
	}
	if first["failed"] != false || first["pricing_status"] != "unpriced" || first["estimated_cost_usd"] != nil {
		t.Fatalf("defaults: %v", first)
	}
	if num(second["latency_ms"]) != 1500 || num(second["ttft_ms"]) != 350 {
		t.Fatalf("latency/ttft: %v", second)
	}
	// codex subset convention: cache stays inside input (total=input+output).
	if num(second["input"]) != 10000 || num(second["cache"]) != 3000 ||
		num(second["output"]) != 1500 || num(second["total"]) != 11500 {
		t.Fatalf("normalized tokens: %v", second)
	}
	if !strings.HasPrefix(second["key"].(string), "key-") {
		t.Fatalf("key must be anonymized: %v", second["key"])
	}
}

func TestRequestListPagination(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "rlp.sqlite"))
	base := time.Now().Add(-time.Hour)
	ids := seedRequestPage(t, 5, base)

	q := map[string][]string{"limit": {"2"}}
	s1, p1 := getRequests(t, q)
	if s1 != 200 {
		t.Fatalf("page1: %d", s1)
	}
	if num(p1["limit"]) != 2 || p1["has_more"] != true || p1["next_cursor"] == nil {
		t.Fatalf("page1 meta: %v", p1)
	}
	got1 := requestIDs(p1)
	if len(got1) != 2 || got1[0] != ids[4] || got1[1] != ids[3] {
		t.Fatalf("page1 ids=%v want newest %d,%d", got1, ids[4], ids[3])
	}

	q["cursor"] = []string{p1["next_cursor"].(string)}
	s2, p2 := getRequests(t, q)
	if s2 != 200 || p2["has_more"] != true {
		t.Fatalf("page2: %d %v", s2, p2)
	}
	got2 := requestIDs(p2)
	if len(got2) != 2 || got2[0] != ids[2] || got2[1] != ids[1] {
		t.Fatalf("page2 ids=%v", got2)
	}

	q["cursor"] = []string{p2["next_cursor"].(string)}
	s3, p3 := getRequests(t, q)
	if s3 != 200 {
		t.Fatalf("page3: %d", s3)
	}
	got3 := requestIDs(p3)
	if len(got3) != 1 || got3[0] != ids[0] {
		t.Fatalf("page3 ids=%v", got3)
	}
	if p3["has_more"] != false || p3["next_cursor"] != nil {
		t.Fatalf("terminal page must report has_more=false + null cursor: %v", p3)
	}

	// Determinism: replaying page1 + page2 yields identical id sequences.
	_, r1 := getRequests(t, map[string][]string{"limit": {"2"}})
	_, r2 := getRequests(t, map[string][]string{"limit": {"2"}, "cursor": {r1["next_cursor"].(string)}})
	if fmt.Sprint(requestIDs(r1)) != fmt.Sprint(got1) || fmt.Sprint(requestIDs(r2)) != fmt.Sprint(got2) {
		t.Fatalf("pagination must be deterministic: %v %v", requestIDs(r1), requestIDs(r2))
	}

	// limit clamp: >max clamps to 100 (still a single page here).
	s, clamped := getRequests(t, map[string][]string{"limit": {"500"}})
	if s != 200 || num(clamped["limit"]) != 100 || len(clamped["items"].([]any)) != 5 {
		t.Fatalf("limit must clamp to 100: %v", clamped)
	}
}

func TestRequestListValidation(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "rlv.sqlite"))
	cases := []map[string][]string{
		{"sort": {"bogus"}},
		{"sort": {"ts_unix DESC; DROP TABLE usage_events"}},
		{"limit": {"abc"}},
		{"limit": {"0"}},
		{"cursor": {"!!!not-base64!!!"}},
		{"status": {"weird"}},
		{"pricing_status": {"weird"}},
		{"min_latency_ms": {"abc"}},
		{"min_latency_ms": {"NaN"}},
		{"min_total_tokens": {"1.5"}},
		{"min_cost_usd": {"xyz"}},
		{"min_cost_usd": {"+Inf"}},
		{"start": {"not-a-date"}},
	}
	for i, q := range cases {
		if s, resp := getRequests(t, q); s != 400 {
			t.Fatalf("case %d %v: got %d %v", i, q, s, resp)
		}
	}
	// Cursor bound to a different sort must be rejected, not silently applied.
	openDB(filepath.Join(t.TempDir(), "rlv2.sqlite"))
	seedRequestPage(t, 3, time.Now().Add(-time.Hour))
	_, p1 := getRequests(t, map[string][]string{"limit": {"1"}, "sort": {"oldest"}})
	s, resp := getRequests(t, map[string][]string{
		"limit": {"1"}, "sort": {"newest"}, "cursor": {p1["next_cursor"].(string)},
	})
	if s != 400 {
		t.Fatalf("sort-mismatched cursor must 400: %d %v", s, resp)
	}
	// Structurally corrupt cursor (valid base64, wrong json) → 400.
	s, resp = getRequests(t, map[string][]string{"cursor": {"aGVsbG8"}})
	if s != 400 {
		t.Fatalf("corrupt cursor must 400: %d %v", s, resp)
	}
}

func TestRequestListFilters(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "rlf.sqlite"))
	now := time.Now()
	seedRecord(t, now.Add(-50*time.Minute), map[string]any{
		"Model": "m-alpha", "Provider": "codex", "Source": "sk-one",
		"Latency": int64(2_000_000_000),
		"Detail":  map[string]int64{"InputTokens": 100, "OutputTokens": 50},
	})
	seedRecord(t, now.Add(-40*time.Minute), map[string]any{
		"Model": "m-beta", "Provider": "claude", "Source": "sk-two",
		"Latency": int64(500_000_000),
		"Detail":  map[string]int64{"InputTokens": 5000, "OutputTokens": 500},
	})
	seedRecord(t, now.Add(-30*time.Minute), map[string]any{
		"Model": "m-alpha", "Provider": "codex", "Source": "sk-one",
		"Failed": true, // zero-token failed request: no Detail at all
	})

	ids := []int64{}
	mu.Lock()
	rows, err := db.Query("SELECT id FROM usage_events ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	mu.Unlock()
	// anon keys for the two sources
	mu.Lock()
	var p1, p2 string
	if err := db.QueryRow("SELECT payload FROM usage_events WHERE id=?", ids[0]).Scan(&p1); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT payload FROM usage_events WHERE id=?", ids[1]).Scan(&p2); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()
	var e1, e2 dbEvent
	if err := json.Unmarshal([]byte(p1), &e1); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(p2), &e2); err != nil {
		t.Fatal(err)
	}
	keyOne, keyTwo := anonKey(e1.Source), anonKey(e2.Source)

	type tc struct {
		name string
		q    map[string][]string
		want []int64 // expected ids in response order (newest first)
	}
	run := func(c tc) {
		t.Helper()
		s, resp := getRequests(t, c.q)
		if s != 200 {
			t.Fatalf("%s: %d %v", c.name, s, resp)
		}
		got := requestIDs(resp)
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Fatalf("%s: ids=%v want %v", c.name, got, c.want)
		}
	}
	run(tc{"model exact", map[string][]string{"model": {"m-alpha"}}, []int64{ids[2], ids[0]}})
	run(tc{"model misses alias-empty", map[string][]string{"model": {"nope"}}, nil})
	run(tc{"key exact", map[string][]string{"key": {keyTwo}}, []int64{ids[1]}})
	run(tc{"key combined model", map[string][]string{"key": {keyOne}, "model": {"m-alpha"}}, []int64{ids[2], ids[0]}})
	run(tc{"status failed", map[string][]string{"status": {"failed"}}, []int64{ids[2]}})
	run(tc{"status success", map[string][]string{"status": {"success"}}, []int64{ids[1], ids[0]}})
	run(tc{"min_latency excludes zero", map[string][]string{"min_latency_ms": {"100"}}, []int64{ids[1], ids[0]}})
	run(tc{"min_latency high", map[string][]string{"min_latency_ms": {"1500"}}, []int64{ids[0]}})
	run(tc{"min_total_tokens", map[string][]string{"min_total_tokens": {"1000"}}, []int64{ids[1]}})
	run(tc{"search model", map[string][]string{"search": {"ALPHA"}}, []int64{ids[2], ids[0]}})
	run(tc{"search provider", map[string][]string{"search": {"claude"}}, []int64{ids[1]}})
	run(tc{"search key", map[string][]string{"search": {keyTwo}}, []int64{ids[1]}})
	run(tc{"search id exact", map[string][]string{"search": {fmt.Sprint(ids[1])}}, []int64{ids[1]}})
	run(tc{"search none", map[string][]string{"search": {"zzz"}}, nil})
	run(tc{"combined", map[string][]string{
		"model": {"m-alpha"}, "status": {"success"}, "min_total_tokens": {"10"},
	}, []int64{ids[0]}})
	// date range: yesterday-only window must exclude all three events.
	yesterday := now.AddDate(0, 0, -1)
	s, resp := getRequests(t, map[string][]string{
		"start": {yesterday.Format("2006-01-02")}, "end": {yesterday.Format("2006-01-02")},
	})
	if s != 200 || len(resp["items"].([]any)) != 0 {
		t.Fatalf("out-of-range date filter: %v", resp)
	}
}

func TestRequestListSorts(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "rls.sqlite"))
	now := time.Now()
	// a: newest, mid tokens, mid latency
	seedRecord(t, now.Add(-10*time.Minute), map[string]any{
		"Model": "a", "Latency": int64(500_000_000),
		"Detail": map[string]int64{"InputTokens": 100, "OutputTokens": 100},
	})
	// b: oldest, most tokens, lowest latency
	seedRecord(t, now.Add(-30*time.Minute), map[string]any{
		"Model": "b", "Latency": int64(100_000_000),
		"Detail": map[string]int64{"InputTokens": 9000, "OutputTokens": 900},
	})
	// c: middle, fewest tokens, highest latency, failed
	seedRecord(t, now.Add(-20*time.Minute), map[string]any{
		"Model": "c", "Latency": int64(9_000_000_000), "Failed": true,
		"Detail": map[string]int64{"InputTokens": 5, "OutputTokens": 5},
	})
	mu.Lock()
	var ids []int64
	rows, err := db.Query("SELECT id FROM usage_events ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	mu.Unlock()
	a, b, c := ids[0], ids[1], ids[2]

	check := func(sort string, want []int64) {
		t.Helper()
		s, resp := getRequests(t, map[string][]string{"sort": {sort}})
		if s != 200 {
			t.Fatalf("sort %s: %d", sort, s)
		}
		if got := requestIDs(resp); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("sort %s: ids=%v want %v", sort, got, want)
		}
	}
	check("newest", []int64{a, c, b})
	check("oldest", []int64{b, c, a})
	check("most_tokens", []int64{b, a, c})
	check("least_tokens", []int64{c, a, b})
	check("highest_latency", []int64{c, a, b})
	check("failed_first", []int64{c, a, b})

	// Cursor paging works on derived sorts too (keyset over the in-memory order).
	s, p1 := getRequests(t, map[string][]string{"sort": {"least_tokens"}, "limit": {"1"}})
	if s != 200 || requestIDs(p1)[0] != c {
		t.Fatalf("least_tokens page1: %v", p1)
	}
	s, p2 := getRequests(t, map[string][]string{
		"sort": {"least_tokens"}, "limit": {"1"}, "cursor": {p1["next_cursor"].(string)},
	})
	if s != 200 || requestIDs(p2)[0] != a {
		t.Fatalf("least_tokens page2: %v %v", s, p2)
	}
}

func TestRequestListCostFilters(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "rlc.sqlite"))
	stubNet(t)
	mu.Lock()
	if _, err := db.Exec(`INSERT INTO price_catalog
		(model,provider,source,input_per_m,output_per_m,cache_read_per_m,cache_write_per_m,synced_at) VALUES
		('m-full','p1','models.dev',1,10,0.1,NULL,'2026-10-01T00:00:00Z'),
		('m-part','p1','models.dev',1,NULL,0.1,NULL,'2026-10-01T00:00:00Z'),
		('amb-m','pa','models.dev',1,10,NULL,NULL,'2026-10-01T00:00:00Z'),
		('amb-m','pb','models.dev',9,90,NULL,NULL,'2026-10-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()
	now := time.Now()
	seedRecord(t, now.Add(-4*time.Hour), map[string]any{
		"Provider": "p1", "Model": "m-full",
		"Detail": map[string]int64{"InputTokens": 1000, "OutputTokens": 100},
	}) // 0.002 usd priced
	seedRecord(t, now.Add(-3*time.Hour), map[string]any{
		"Provider": "p1", "Model": "m-part",
		"Detail": map[string]int64{"InputTokens": 1000, "OutputTokens": 100},
	}) // partial (no output price) → 0.001 usd
	seedRecord(t, now.Add(-2*time.Hour), map[string]any{
		"Provider": "p9", "Model": "amb-m",
		"Detail": map[string]int64{"InputTokens": 10, "OutputTokens": 10},
	}) // ambiguous
	seedRecord(t, now.Add(-time.Hour), map[string]any{
		"Provider": "p1", "Model": "m-none",
		"Detail": map[string]int64{"InputTokens": 5, "OutputTokens": 5},
	}) // unpriced

	mu.Lock()
	var ids []int64
	rows, err := db.Query("SELECT e.id FROM usage_events e ORDER BY e.id")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	mu.Unlock()
	full, part, amb, none := ids[0], ids[1], ids[2], ids[3]

	listStatus := map[int64]string{}
	s, resp := getRequests(t, nil)
	if s != 200 || len(resp["items"].([]any)) != 4 {
		t.Fatalf("list: %d %v", s, resp)
	}
	for _, it := range resp["items"].([]any) {
		m := it.(map[string]any)
		listStatus[int64(num(m["id"]))] = m["pricing_status"].(string)
	}
	if listStatus[full] != "priced" || listStatus[part] != "partial" ||
		listStatus[amb] != "ambiguous" || listStatus[none] != "unpriced" {
		t.Fatalf("pricing_status per item: %v", listStatus)
	}
	if math.Abs(num(resp["items"].([]any)[3].(map[string]any)["estimated_cost_usd"])-0.002) > 1e-12 {
		t.Fatalf("m-full usd: %v", resp["items"])
	}

	// pricing_status filter
	onlyStatus := func(ps string) []int64 {
		s, r := getRequests(t, map[string][]string{"pricing_status": {ps}})
		if s != 200 {
			t.Fatalf("pricing_status=%s: %d", ps, s)
		}
		return requestIDs(r)
	}
	if got := onlyStatus("priced"); fmt.Sprint(got) != fmt.Sprint([]int64{full}) {
		t.Fatalf("priced filter: %v", got)
	}
	if got := onlyStatus("partial"); fmt.Sprint(got) != fmt.Sprint([]int64{part}) {
		t.Fatalf("partial filter: %v", got)
	}
	if got := onlyStatus("unpriced"); fmt.Sprint(got) != fmt.Sprint([]int64{none}) {
		t.Fatalf("unpriced filter: %v", got)
	}
	if got := onlyStatus("ambiguous"); fmt.Sprint(got) != fmt.Sprint([]int64{amb}) {
		t.Fatalf("ambiguous filter: %v", got)
	}

	// min_cost_usd: 0.002 (full) and 0.001 (part) qualify at 0.0005; only
	// full qualifies at 0.0015; unpriced/ambiguous never qualify.
	if got := requestIDsAt(t, map[string][]string{"min_cost_usd": {"0.0005"}}); fmt.Sprint(got) != fmt.Sprint([]int64{part, full}) {
		t.Fatalf("min_cost 0.0005: %v", got)
	}
	if got := requestIDsAt(t, map[string][]string{"min_cost_usd": {"0.0015"}}); fmt.Sprint(got) != fmt.Sprint([]int64{full}) {
		t.Fatalf("min_cost 0.0015: %v", got)
	}
	// highest_cost: priced rows by usd desc; unpriced tail by id desc.
	if got := requestIDsAt(t, map[string][]string{"sort": {"highest_cost"}}); fmt.Sprint(got) != fmt.Sprint([]int64{full, part, none, amb}) {
		t.Fatalf("highest_cost order: %v", got)
	}
}

func requestIDsAt(t *testing.T, q map[string][]string) []int64 {
	t.Helper()
	s, resp := getRequests(t, q)
	if s != 200 {
		t.Fatalf("query %v: %d %v", q, s, resp)
	}
	return requestIDs(resp)
}

func TestRequestListLegacyNullTsUnix(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "rll.sqlite"))
	now := time.Now()
	seedRecord(t, now.Add(-time.Hour), map[string]any{"Model": "fresh"})
	// Legacy row: ts_unix NULL (pre-backfill state) with an in-range payload ts.
	legacy := fmt.Sprintf(`{"timestamp":%q,"model":"legacy-m","failed":false,"tokens":{"input_tokens":7,"output_tokens":3}}`,
		now.Add(-30*time.Minute).Format(time.RFC3339Nano))
	mu.Lock()
	legacyID := seedLegacyRow(t, db, now.Add(-30*time.Minute).Format("2006-01-02 15:04:05"), "leg-1", legacy, nil)
	mu.Unlock()

	s, resp := getRequests(t, nil)
	if s != 200 {
		t.Fatalf("list: %d", s)
	}
	got := requestIDs(resp)
	found := false
	for _, id := range got {
		if id == legacyID {
			found = true
		}
	}
	if !found || len(got) != 2 {
		t.Fatalf("legacy NULL ts_unix row must appear: ids=%v legacy=%d", got, legacyID)
	}
}

func TestRequestDetail(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "rd.sqlite"))
	stubNet(t)
	mu.Lock()
	if _, err := db.Exec(`INSERT INTO price_catalog
		(model,provider,source,input_per_m,output_per_m,cache_read_per_m,cache_write_per_m,synced_at) VALUES
		('m-d','p1','models.dev',2,10,0.5,NULL,'2026-10-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()
	now := time.Now()
	seedRecord(t, now.Add(-time.Hour), map[string]any{
		"Provider": "p1", "Model": "m-d", "Source": "sk-secret-key",
		"Latency": int64(2_000_000_000), "TTFT": int64(200_000_000),
		"StatusCode": 200, "AuthType": "oauth",
		"Detail": map[string]int64{
			"InputTokens": 1000, "CacheReadTokens": 200, "OutputTokens": 100,
			"ReasoningTokens": 7,
		},
	})
	mu.Lock()
	var id int64
	if err := db.QueryRow("SELECT id FROM usage_events").Scan(&id); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()

	status, d, raw := getRequestDetail(t, id)
	if status != 200 {
		t.Fatalf("detail: %d %s", status, raw)
	}
	if num(d["id"]) != float64(id) || d["model"] != "m-d" || d["provider"] != "p1" {
		t.Fatalf("detail identity: %v", d)
	}
	if d["failed"] != false || num(d["status_code"]) != 200 || d["auth_type"] != "oauth" {
		t.Fatalf("detail flags: %v", d)
	}
	tok := d["tokens"].(map[string]any)
	// codex-like provider p1: no cache fold-in → input 1000, cache_read 200.
	if num(tok["input"]) != 1000 || num(tok["cache_read"]) != 200 || num(tok["output"]) != 100 ||
		num(tok["total"]) != 1100 || num(tok["base_input"]) != 800 || num(tok["reasoning"]) != 7 {
		t.Fatalf("tokens: %v", tok)
	}
	perf := d["performance"].(map[string]any)
	if num(perf["latency_ms"]) != 2000 || num(perf["ttft_ms"]) != 200 {
		t.Fatalf("performance: %v", perf)
	}
	if math.Abs(num(perf["output_per_sec_estimate"])-50) > 1e-9 {
		t.Fatalf("output_per_sec_estimate: %v", perf)
	}
	cost := d["cost"].(map[string]any)
	// usd = (800*2 + 200*0.5 + 100*10) / 1e6
	wantUSD := (800*2 + 200*0.5 + 100*10) / 1e6
	if math.Abs(num(cost["usd"])-wantUSD) > 1e-12 || cost["status"] != "priced" {
		t.Fatalf("cost: %v want usd %v", cost, wantUSD)
	}
	if cost["estimate"] != true || cost["matched_model"] != "m-d" || cost["matched_provider"] != "p1" {
		t.Fatalf("cost meta: %v", cost)
	}
	if pr, ok := cost["partial_reasons"].([]any); !ok || len(pr) != 0 {
		t.Fatalf("partial_reasons must be []: %v", cost["partial_reasons"])
	}
	if _, ok := d["fx"]; !ok {
		t.Fatal("fx key must be present (null when no rate)")
	}
	if d["tokens_note"] != nil {
		t.Fatalf("no cache fold-in → tokens_note null: %v", d["tokens_note"])
	}

	// Privacy: no raw secret material anywhere in the serialized body.
	body := string(raw)
	for _, banned := range []string{"sk-secret-key", `"api_key"`, `"payload"`, `"executor_type"`, `"Authorization"`} {
		if strings.Contains(body, banned) {
			t.Fatalf("detail leaks %q: %s", banned, body)
		}
	}
	if _, leaked := d["source"]; leaked {
		t.Fatalf("detail must not expose source: %v", d)
	}
	if _, leaked := d["api_key"]; leaked {
		t.Fatalf("detail must not expose api_key: %v", d)
	}
}

func TestRequestDetailStoredEstimateWins(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "rds.sqlite"))
	stubNet(t)
	now := time.Now()
	seedRecord(t, now.Add(-time.Hour), map[string]any{
		"Provider": "p1", "Model": "m-d",
		"Detail": map[string]int64{"InputTokens": 1000, "OutputTokens": 100},
	})
	mu.Lock()
	var id int64
	if err := db.QueryRow("SELECT id FROM usage_events").Scan(&id); err != nil {
		t.Fatal(err)
	}
	// A stored estimate with a fixed timestamp + an arbitrary cost: the
	// detail view must report it verbatim and never rewrite the row.
	if _, err := db.Exec(`INSERT INTO cost_estimates
		(event_id, model, provider, matched_model, matched_provider, source, usd,
		 input_usd, output_usd, status, calculated_at)
		VALUES (?, 'm-d', 'p1', 'm-d', 'p1', 'models.dev', 0.42, 0.4, 0.02, 'priced',
		 '2026-10-01T00:00:00Z')`, id); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()

	status, d, _ := getRequestDetail(t, id)
	if status != 200 {
		t.Fatalf("detail: %d", status)
	}
	cost := d["cost"].(map[string]any)
	if num(cost["usd"]) != 0.42 || cost["status"] != "priced" || cost["source"] != "models.dev" {
		t.Fatalf("stored estimate must win: %v", cost)
	}
	var calculated string
	var n int
	mu.Lock()
	if err := db.QueryRow("SELECT calculated_at FROM cost_estimates WHERE event_id=?", id).Scan(&calculated); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM cost_estimates").Scan(&n); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()
	if calculated != "2026-10-01T00:00:00Z" || n != 1 {
		t.Fatalf("stored estimate must not be recalculated: calculated_at=%v rows=%d", calculated, n)
	}
	// A second detail call still writes nothing.
	getRequestDetail(t, id)
	mu.Lock()
	if err := db.QueryRow("SELECT COUNT(*) FROM cost_estimates").Scan(&n); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()
	if n != 1 {
		t.Fatalf("read-only detail must not persist estimates: %d", n)
	}
}

func TestRequestDetailStatusesAndNulls(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "rdn.sqlite"))
	stubNet(t)
	mu.Lock()
	if _, err := db.Exec(`INSERT INTO price_catalog
		(model,provider,source,input_per_m,output_per_m,cache_read_per_m,synced_at) VALUES
		('m-part','p1','models.dev',1,NULL,0.1,'2026-10-01T00:00:00Z'),
		('amb-m','pa','models.dev',1,10,NULL,'2026-10-01T00:00:00Z'),
		('amb-m','pb','models.dev',9,90,NULL,'2026-10-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()
	now := time.Now()
	seedRecord(t, now.Add(-3*time.Hour), map[string]any{
		"Provider": "p1", "Model": "m-part",
		"Detail": map[string]int64{"InputTokens": 1000, "CacheReadTokens": 200, "OutputTokens": 100},
	})
	seedRecord(t, now.Add(-2*time.Hour), map[string]any{
		"Provider": "p9", "Model": "amb-m",
		"Detail": map[string]int64{"InputTokens": 10, "OutputTokens": 10},
	})
	// Zero-token failed request, no latency, no provider/auth.
	seedRecord(t, now.Add(-time.Hour), map[string]any{
		"Model": "m-fail", "Failed": true,
	})
	mu.Lock()
	var ids []int64
	rows, err := db.Query("SELECT id FROM usage_events ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	mu.Unlock()
	partID, ambID, failID := ids[0], ids[1], ids[2]

	// partial: usd sums priced components, output_usd null, reason listed.
	_, d, _ := getRequestDetail(t, partID)
	cost := d["cost"].(map[string]any)
	if cost["status"] != "partial" || cost["usd"] == nil {
		t.Fatalf("partial must carry usd: %v", cost)
	}
	wantUSD := (800*1 + 200*0.1) / 1e6
	if math.Abs(num(cost["usd"])-wantUSD) > 1e-12 {
		t.Fatalf("partial usd: %v want %v", cost["usd"], wantUSD)
	}
	if cost["output_usd"] != nil || cost["cache_write_usd"] != nil {
		t.Fatalf("unpriced components must be null: %v", cost)
	}
	reasons := cost["partial_reasons"].([]any)
	if len(reasons) != 1 || reasons[0] != "output" {
		t.Fatalf("partial_reasons: %v", reasons)
	}
	if cost["matched_model"] != "m-part" || cost["matched_provider"] != "p1" {
		t.Fatalf("partial match meta: %v", cost)
	}

	// ambiguous: usd null, status ambiguous.
	_, d, _ = getRequestDetail(t, ambID)
	cost = d["cost"].(map[string]any)
	if cost["status"] != "ambiguous" || cost["usd"] != nil {
		t.Fatalf("ambiguous must be usd-null: %v", cost)
	}

	// zero-token failed: totals 0, all perf fields null, cost unpriced.
	_, d, _ = getRequestDetail(t, failID)
	tok := d["tokens"].(map[string]any)
	if num(tok["total"]) != 0 || num(tok["input"]) != 0 || num(tok["base_input"]) != 0 {
		t.Fatalf("zero-token request: %v", tok)
	}
	perf := d["performance"].(map[string]any)
	if perf["latency_ms"] != nil || perf["ttft_ms"] != nil || perf["output_per_sec_estimate"] != nil {
		t.Fatalf("missing perf must be null: %v", perf)
	}
	if d["failed"] != true || d["status_code"] != nil || d["auth_type"] != nil {
		t.Fatalf("failed row: %v", d)
	}
	cost = d["cost"].(map[string]any)
	if cost["status"] != "unpriced" || cost["usd"] != nil {
		t.Fatalf("unpriced: %v", cost)
	}

	// Unknown id → 404; malformed id → 400.
	if s, d, _ := getRequestDetail(t, 999999); s != 404 || d["error"] == nil {
		t.Fatalf("unknown id must 404: %d %v", s, d)
	}
	if s, _, _ := getRequestDetail(t, "abc"); s != 400 {
		t.Fatalf("non-digit id must 400: %d", s)
	}
	if s, _ := callMgmtQuery(t, "/v0/management/plugins/cpa-token-analytics/api/requests/1/2", "GET", nil); s != 404 {
		t.Fatalf("deep path must 404: %d", s)
	}
	// Non-GET methods are not part of the contract.
	if s, _ := callMgmtQuery(t, "/v0/management/plugins/cpa-token-analytics/api/requests", "POST", nil); s != 404 {
		t.Fatalf("POST list must 404: %d", s)
	}
}

// TestRequestDetailCacheFoldNote: a claude-convention row (cache folded into
// input by addInputCache) must surface tokens_note and a consistent
// base_input (input − cache − cache_write).
func TestRequestDetailCacheFoldNote(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "rdc.sqlite"))
	stubNet(t)
	seedRecord(t, time.Now().Add(-time.Hour), map[string]any{
		"Provider": "claude", "Model": "claude-x",
		"Detail": map[string]int64{
			"InputTokens": 100, "CacheReadTokens": 50, "OutputTokens": 10,
			"TotalTokens": 160, // 100 + 50 + 10 → fold cache into input
		},
	})
	mu.Lock()
	var id int64
	if err := db.QueryRow("SELECT id FROM usage_events").Scan(&id); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()
	status, d, _ := getRequestDetail(t, id)
	if status != 200 {
		t.Fatalf("detail: %d", status)
	}
	tok := d["tokens"].(map[string]any)
	if num(tok["input"]) != 150 || num(tok["base_input"]) != 100 || num(tok["cache_read"]) != 50 {
		t.Fatalf("cache fold-in tokens: %v", tok)
	}
	if d["tokens_note"] == nil {
		t.Fatalf("folded input must be flagged via tokens_note: %v", d)
	}
}

// TestRequestListNoEstimateWrites: the list path is read-only — resolving
// costs for a page must never create cost_estimates rows.
func TestRequestListNoEstimateWrites(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "rlnw.sqlite"))
	stubNet(t)
	mu.Lock()
	if _, err := db.Exec(`INSERT INTO price_catalog
		(model,provider,source,input_per_m,output_per_m,synced_at) VALUES
		('m-d','p1','models.dev',1,10,'2026-10-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()
	now := time.Now()
	seedRecord(t, now.Add(-time.Hour), map[string]any{
		"Provider": "p1", "Model": "m-d",
		"Detail": map[string]int64{"InputTokens": 1000, "OutputTokens": 100},
	})
	if s, r := getRequests(t, map[string][]string{"min_cost_usd": {"0"}, "pricing_status": {"priced"}, "sort": {"highest_cost"}}); s != 200 {
		t.Fatalf("cost-filtered list: %d %v", s, r)
	}
	if n := estimateCount(t); n != 0 {
		t.Fatalf("request list must not persist estimates: %d", n)
	}
	if s, _, _ := getRequestDetail(t, 1); s != 200 {
		t.Fatalf("detail: %d", s)
	}
	if n := estimateCount(t); n != 0 {
		t.Fatalf("request detail must not persist estimates: %d", n)
	}
}

// BenchmarkRequestList exercises the default newest sort over the seeded
// 30-day window (pattern: analytics_perf_test.go). Estimate persistence is
// pre-seeded so the page-cost resolution stays on the stored path.
func BenchmarkRequestList(b *testing.B) {
	for _, n := range benchSizes() {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			openBenchDB(b, seedBenchDB(b, n, 0, true))
			preseedEstimates(b)
			q := benchQuery30d()
			q["limit"] = []string{"100"}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				status, body := requestList(q)
				if status != 200 {
					b.Fatalf("list: %d %s", status, body)
				}
				_ = body
			}
		})
	}
}

// BenchmarkRequestListHighestCost is the heavy path: every candidate must be
// cost-resolved and the whole window sorted before paging.
func BenchmarkRequestListHighestCost(b *testing.B) {
	for _, n := range benchSizes() {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			openBenchDB(b, seedBenchDB(b, n, 0, true))
			preseedEstimates(b)
			q := benchQuery30d()
			q["limit"] = []string{"100"}
			q["sort"] = []string{"highest_cost"}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				status, body := requestList(q)
				if status != 200 {
					b.Fatalf("list: %d %s", status, body)
				}
				_ = body
			}
		})
	}
}
