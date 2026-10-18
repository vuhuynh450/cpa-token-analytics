package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- fixtures (inline, no network) ----

const modelsDevFixture = `{
  "openai": {
    "id": "openai", "name": "OpenAI",
    "models": {
      "gpt-5.4": {"id": "gpt-5.4", "cost": {"input": 2.5, "output": 15, "cache_read": 0.25}, "canonical_model_id": "openai/gpt-5.4"},
      "gpt-4o-2024-08-06": {"id": "gpt-4o-2024-08-06", "cost": {"input": 2.5, "output": 10}},
      "gpt-5-mini": {"id": "gpt-5-mini", "cost": {"input": 0.25, "output": 2, "cache_read": 0.025}},
      "chatgpt-image-latest": {"id": "chatgpt-image-latest"}
    }
  },
  "anthropic": {
    "id": "anthropic", "name": "Anthropic",
    "models": {
      "claude-sonnet-4-5": {"id": "claude-sonnet-4-5", "cost": {"input": 3, "output": 15, "cache_read": 0.3, "cache_write": 3.75}, "canonical_model_id": "anthropic/claude-sonnet-4-5"}
    }
  },
  "google": {
    "id": "google", "name": "Google",
    "models": {
      "gemini-2.5-pro": {"id": "gemini-2.5-pro", "cost": {"input": 1.25, "output": 10, "cache_read": 0.125}}
    }
  }
}`

const litellmFixture = `{
  "sample_spec": {"litellm_provider": "openai", "mode": "chat"},
  "gpt-4o": {"input_cost_per_token": 2.5e-06, "output_cost_per_token": 1e-05, "cache_read_input_token_cost": 1.25e-06, "litellm_provider": "openai"},
  "claude-sonnet-4-5": {"input_cost_per_token": 3e-06, "output_cost_per_token": 1.5e-05, "cache_read_input_token_cost": 3e-07, "cache_creation_input_token_cost": 3.75e-06, "litellm_provider": "anthropic"},
  "gemini-2.5-pro": {"input_cost_per_token": 1.25e-06, "output_cost_per_token": 1e-05, "litellm_provider": "vertex_ai-language-models"},
  "gemini/gemini-2.5-flash": {"input_cost_per_token": 3e-07, "output_cost_per_token": 2.5e-06, "litellm_provider": "gemini"},
  "no-price-model": {"litellm_provider": "openai", "mode": "chat"}
}`

func stubFetch(t *testing.T, fn func(ctx context.Context, source string) ([]byte, error)) {
	t.Helper()
	old := fetchPricingSource
	fetchPricingSource = fn
	t.Cleanup(func() { fetchPricingSource = old })
}

func fixtureFetch(data string) func(ctx context.Context, source string) ([]byte, error) {
	return func(ctx context.Context, source string) ([]byte, error) {
		switch source {
		case pricingSourceModelsDev:
			return []byte(data), nil
		default:
			return nil, fmt.Errorf("litellm unreachable")
		}
	}
}

// ---- parser tests ----

func TestParseModelsDev(t *testing.T) {
	cat, aliases, err := parseModelsDev([]byte(modelsDevFixture))
	if err != nil {
		t.Fatal(err)
	}
	r, ok := cat["openai"]["gpt-5.4"]
	if !ok {
		t.Fatal("missing openai/gpt-5.4")
	}
	if *r.in != 2.5 || *r.out != 15 || *r.cr != 0.25 {
		t.Fatalf("gpt-5.4 prices: %+v", r)
	}
	if r.cw != nil {
		t.Fatal("cache_write must be nil when absent (sparse ~23%)")
	}
	if _, ok := cat["openai"]["chatgpt-image-latest"]; ok {
		t.Fatal("model without cost must be skipped")
	}
	if _, ok := cat["anthropic"]["claude-sonnet-4-5"]; !ok || *cat["anthropic"]["claude-sonnet-4-5"].cw != 3.75 {
		t.Fatal("anthropic row with cache_write missing/wrong")
	}
	if len(aliases["anthropic"]) != 1 || aliases["anthropic"][0] != "claude-sonnet-4-5" {
		t.Fatalf("canonical aliases: %v", aliases)
	}
	if got := catalogRowCount(cat); got != 5 {
		t.Fatalf("row count: %d (5 cost-bearing models)", got)
	}
}

func TestParseLiteLLM(t *testing.T) {
	cat, err := parseLiteLLM([]byte(litellmFixture))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cat["sample_spec"]; ok {
		t.Fatal("sample_spec must be skipped")
	}
	r, ok := cat["openai"]["gpt-4o"]
	if !ok || *r.in != 2.5 || *r.out != 10 || *r.cr != 1.25 {
		t.Fatalf("gpt-4o ×1e6 conversion: %+v", r)
	}
	// bare gemini key → vertex_ai-language-models
	if _, ok := cat["vertex_ai-language-models"]["gemini-2.5-pro"]; !ok {
		t.Fatal("bare gemini key must land under vertex_ai-language-models")
	}
	// prefixed gemini/ key → provider gemini + vendor row both kept
	if _, ok := cat["gemini"]["gemini-2.5-flash"]; !ok {
		t.Fatal("gemini/ key must keep provider=gemini row")
	}
	if _, ok := cat["no-price-model"]; ok {
		t.Fatal("entry without any price must be skipped")
	}
	cw := cat["anthropic"]["claude-sonnet-4-5"].cw
	if cw == nil || *cw != 3.75 {
		t.Fatalf("cache_creation_input_token_cost: %v", cw)
	}
}

// ---- matching ----

func seedCatalog(t *testing.T, rows [][6]string) {
	t.Helper()
	for _, r := range rows {
		// r: model, provider, source, in, out, synced_at
		if _, err := db.Exec(`INSERT OR REPLACE INTO price_catalog
			(model, provider, source, input_per_m, output_per_m, synced_at) VALUES (?,?,?,?,?,?)`,
			r[0], r[1], r[2], r[3], r[4], r[5]); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMatchLevels(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "match.sqlite"))
	mu.Lock()
	seedCatalog(t, [][6]string{
		{"gpt-5", "openai", "models.dev", "1", "10", "2026-10-01T00:00:00Z"},
		{"gpt-5", "aaa-other", "models.dev", "9", "9", "2026-10-01T00:00:00Z"},
		{"claude-sonnet-4-5", "anthropic", "models.dev", "3", "15", "2026-10-01T00:00:00Z"},
		{"gpt-5-20261001", "openai", "models.dev", "7", "7", "2026-10-01T00:00:00Z"},
		{"special-7", "otherprov", "models.dev", "2", "2", "2026-10-01T00:00:00Z"},
	})
	mu.Unlock()

	cases := []struct {
		provider, model string
		wantOK          bool
		wantMethod      string
		wantProv        string
		wantModel       string
	}{
		{"openai", "gpt-5", true, "exact", "openai", "gpt-5"},
		{"unknownp", "claude-sonnet-4-5", true, "model_exact", "anthropic", "claude-sonnet-4-5"},
		// alias preference: "codex"→openai beats alphabetical "aaa-other"
		{"codex", "gpt-5", true, "model_exact", "openai", "gpt-5"},
		// normalized: vendor prefix strip
		{"weird", "anthropic/claude-sonnet-4-5", true, "normalized", "anthropic", "claude-sonnet-4-5"},
		// normalized: separators + lowercase
		{"openai", "GPT_5", true, "normalized", "openai", "gpt-5"},
		// dated exact variant wins over stripping
		{"openai", "gpt-5-20261001", true, "exact", "openai", "gpt-5-20261001"},
		// canonical: tail after last '/' only
		{"weird", "a/b/special-7", true, "canonical", "otherprov", "special-7"},
		// unmatched + never fuzzy-substring
		{"", "totally-random-x", false, "unmatched", "", "totally-random-x"},
		{"openai", "gpt", false, "unmatched", "openai", "gpt"},
		{"openai", "gpt-5-turbo-fake", false, "unmatched", "openai", "gpt-5-turbo-fake"},
	}
	for _, c := range cases {
		mr := matchPrice(c.provider, c.model)
		if mr.ok != c.wantOK || mr.method != c.wantMethod {
			t.Fatalf("match(%q,%q): ok=%v method=%s want ok=%v %s", c.provider, c.model, mr.ok, mr.method, c.wantOK, c.wantMethod)
		}
		if mr.ok && (mr.provider != c.wantProv || mr.model != c.wantModel) {
			t.Fatalf("match(%q,%q): matched %s/%s want %s/%s", c.provider, c.model, mr.provider, mr.model, c.wantProv, c.wantModel)
		}
	}
}

// ---- effective price ordering ----

func TestPriceForPriority(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "pfor.sqlite"))
	mu.Lock()
	if _, err := db.Exec(`INSERT INTO price_catalog (model,provider,source,input_per_m,output_per_m,synced_at)
		VALUES ('m1','p1','models.dev',1,10,'2026-10-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO price_snapshots (model,provider,source,input_per_m,output_per_m,effective_from)
		VALUES ('m1','p1','models.dev',5,50,'2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()
	now := float64(time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC).Unix())

	ep, ok := priceFor("p1", "m1", now, false)
	if !ok || ep.snapshot != "historical" || *ep.row.in != 5 {
		t.Fatalf("snapshot must win over latest: %+v", ep)
	}
	ep, ok = priceFor("p1", "m1", now, true)
	if !ok || ep.snapshot != "latest" || *ep.row.in != 1 {
		t.Fatalf("recalc must use latest catalog: %+v", ep)
	}
	// event before first snapshot → latest fallback
	ep, ok = priceFor("p1", "m1", float64(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Unix()), false)
	if !ok || ep.snapshot != "latest" {
		t.Fatalf("pre-snapshot event must fall back to latest: %+v", ep)
	}
	// override wins over everything
	mu.Lock()
	_, err := db.Exec(`INSERT INTO price_overrides (provider,model,input_per_m,output_per_m,updated_at)
		VALUES ('p1','m1',100,200,'2026-10-17T00:00:00Z')`)
	mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	ep, ok = priceFor("p1", "m1", now, false)
	if !ok || ep.snapshot != "override" || *ep.row.in != 100 {
		t.Fatalf("override must win: %+v", ep)
	}
}

// ---- event cost ----

func TestCostEvent(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "cost.sqlite"))
	mu.Lock()
	if _, err := db.Exec(`INSERT INTO price_catalog (model,provider,source,input_per_m,output_per_m,cache_read_per_m,cache_write_per_m,synced_at)
		VALUES
		('m','p','models.dev',1,10,0.1,2,'2026-10-01T00:00:00Z'),
		('m2','p','models.dev',1,10,0.1,NULL,'2026-10-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()

	ev := eventRow{ts: float64(time.Now().Unix()), provider: "p", model: "m",
		input: 1000, output: 100, cache: 200, cacheWrite: 50}
	c := costEvent(ev, false)
	// baseIn = 1000-200-50 = 750 → 750*1 + 200*0.1 + 50*2 + 100*10 = 1870 µUSD/1e6
	want := (750*1 + 200*0.1 + 50*2 + 100*10) / 1e6
	if !c.matched || !c.complete || math.Abs(c.usd-want) > 1e-12 {
		t.Fatalf("cost: %+v want %v", c, want)
	}
	if usd, ok := eventCost(ev); !ok || usd != c.usd {
		t.Fatalf("eventCost hook: %v %v", usd, ok)
	}

	// missing cache_write price → partial (not unpriced, not zero)
	ev2 := ev
	ev2.model = "m2"
	c2 := costEvent(ev2, false)
	if !c2.matched || c2.complete {
		t.Fatalf("partial expected: %+v", c2)
	}
	partial := (750*1 + 200*0.1 + 0 + 100*10) / 1e6
	if math.Abs(c2.usd-partial) > 1e-12 {
		t.Fatalf("partial usd: %v want %v (cache_write must not double into input)", c2.usd, partial)
	}
	if usd, ok := eventCost(ev2); ok {
		t.Fatalf("partial event must not be 'priced': %v", usd)
	}

	// no match → unpriced
	ev3 := ev
	ev3.model = "nope"
	c3 := costEvent(ev3, false)
	if c3.matched || c3.usd != 0 {
		t.Fatalf("unpriced expected: %+v", c3)
	}

	// cache tokens never double-counted in input (base clamps at 0)
	ev4 := eventRow{ts: ev.ts, provider: "p", model: "m", input: 100, output: 0, cache: 150}
	c4 := costEvent(ev4, false)
	want4 := 150 * 0.1 / 1e6
	if !c4.complete || math.Abs(c4.usd-want4) > 1e-15 {
		t.Fatalf("clamped input: %+v want %v", c4, want4)
	}
}

// ---- sync + endpoints ----

func callMgmtBody(t *testing.T, path, method string, body any) (int, []byte) {
	t.Helper()
	req, _ := json.Marshal(map[string]interface{}{
		"result": map[string]interface{}{
			"Method":  method,
			"Path":    path,
			"Query":   map[string][]string{},
			"Headers": map[string][]string{},
			"Body":    body,
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
		Body       []byte
	}
	if err := json.Unmarshal(e.Result, &resp); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Body
}

func postPricing(t *testing.T, name string, body any) map[string]any {
	t.Helper()
	status, raw := callMgmtBody(t, "/v0/management/plugins/cpa-token-analytics/api/pricing/"+name, "POST", body)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v %s", name, err, raw)
	}
	if status != 200 {
		t.Fatalf("POST %s: %d %s", name, status, raw)
	}
	return out
}

func getPricing(t *testing.T, name string) (int, map[string]any) {
	t.Helper()
	status, raw := callMgmtBody(t, "/v0/management/plugins/cpa-token-analytics/api/pricing/"+name, "GET", nil)
	var out map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return status, out
}

func catCount(t *testing.T) int {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM price_catalog").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPricingSyncEndpoints(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "sync.sqlite"))
	stubFetch(t, fixtureFetch(modelsDevFixture))

	// POST sync → ok, models.dev wins
	res := postPricing(t, "sync", nil)
	if res["ok"] != true || res["source"] != "models.dev" || num(res["models"]) != 5 {
		t.Fatalf("sync: %v", res)
	}
	if got := catCount(t); got != 5 {
		t.Fatalf("catalog rows: %d", got)
	}
	status, st := getPricing(t, "status")
	if status != 200 {
		t.Fatalf("status: %d", status)
	}
	if st["last_source"] != "models.dev" || st["last_sync"] == nil || st["enabled"] != true {
		t.Fatalf("status: %v", st)
	}
	if num(st["catalog_count"]) != 5 || st["syncing"] != false {
		t.Fatalf("status fields: %v", st)
	}

	// litellm fallback when models.dev fails
	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		if source == pricingSourceModelsDev {
			return nil, fmt.Errorf("models.dev down")
		}
		return []byte(litellmFixture), nil
	})
	res = postPricing(t, "sync", nil)
	if res["ok"] != true || res["source"] != "litellm" {
		t.Fatalf("fallback sync: %v", res)
	}
	litellmCount := catCount(t)
	if litellmCount == 0 {
		t.Fatal("litellm catalog empty")
	}

	// both fail → catalog untouched, last_error set, last_source preserved
	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		return nil, fmt.Errorf("%s down", source)
	})
	res = postPricing(t, "sync", nil)
	if res["ok"] != false || res["error"] == nil {
		t.Fatalf("failing sync: %v", res)
	}
	if got := catCount(t); got != litellmCount {
		t.Fatalf("catalog must stay untouched on failure: %d vs %d", got, litellmCount)
	}
	_, st = getPricing(t, "status")
	if st["last_error"] == nil || st["last_source"] != "litellm" {
		t.Fatalf("status after failure: %v", st)
	}

	// GET on a POST route → 404
	if s, _ := getPricing(t, "sync"); s != 404 {
		t.Fatalf("GET sync must 404, got %d", s)
	}
}

func TestPricingOverrideAndSettings(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "ovr.sqlite"))
	stubFetch(t, fixtureFetch(modelsDevFixture))
	postPricing(t, "sync", nil)

	// override upsert mirrors into catalog
	res := postPricing(t, "override", map[string]any{"model": "gpt-5.4", "provider": "openai", "input": 9.5, "output": 19})
	if res["ok"] != true {
		t.Fatalf("override: %v", res)
	}
	ep, ok := priceFor("openai", "gpt-5.4", float64(time.Now().Unix()), false)
	if !ok || ep.snapshot != "override" || *ep.row.in != 9.5 {
		t.Fatalf("override effective price: %+v", ep)
	}
	// override survives a resync (external rows replaced, overrides kept)
	postPricing(t, "sync", nil)
	ep, ok = priceFor("openai", "gpt-5.4", float64(time.Now().Unix()), false)
	if !ok || ep.snapshot != "override" {
		t.Fatalf("override must survive resync: %+v", ep)
	}
	// models endpoint exposes source=override
	_, models := getPricing(t, "models")
	found := false
	for _, m := range models["models"].([]any) {
		mm := m.(map[string]any)
		if mm["model"] == "gpt-5.4" && mm["provider"] == "openai" && mm["source"] == "override" {
			found = true
			prices := mm["prices"].(map[string]any)
			if num(prices["input"]) != 9.5 || num(prices["output"]) != 19 {
				t.Fatalf("override prices: %v", prices)
			}
		}
	}
	if !found {
		t.Fatal("override row missing in /pricing/models")
	}
	// validation: negative and empty bodies rejected
	if s, _ := callMgmtBody(t, "/v0/management/plugins/cpa-token-analytics/api/pricing/override", "POST",
		map[string]any{"model": "x", "input": -1}); s != 400 {
		t.Fatal("negative price must 400")
	}
	if s, _ := callMgmtBody(t, "/v0/management/plugins/cpa-token-analytics/api/pricing/override", "POST",
		map[string]any{"provider": "p"}); s != 400 {
		t.Fatal("missing model must 400")
	}
	// delete removes override + catalog mirror
	res = postPricing(t, "override", map[string]any{"model": "gpt-5.4", "provider": "openai", "delete": true})
	if res["ok"] != true {
		t.Fatalf("delete override: %v", res)
	}
	ep, ok = priceFor("openai", "gpt-5.4", float64(time.Now().Unix()), false)
	if !ok || ep.snapshot == "override" {
		t.Fatalf("after delete must fall back: %+v", ep)
	}

	// settings: auto_sync toggle + fx
	postPricing(t, "settings", map[string]any{"auto_sync": false})
	_, st := getPricing(t, "status")
	if st["enabled"] != false {
		t.Fatalf("auto_sync off: %v", st)
	}
	postPricing(t, "settings", map[string]any{"auto_sync": true, "fx_usd_vnd": 25000})
	_, st = getPricing(t, "status")
	fx, _ := st["fx"].(map[string]any)
	if num(fx["rate"]) != 25000 || fx["updated_at"] == nil {
		t.Fatalf("fx: %v", st["fx"])
	}
	if s, _ := callMgmtBody(t, "/v0/management/plugins/cpa-token-analytics/api/pricing/settings", "POST",
		map[string]any{"fx_usd_vnd": -5}); s != 400 {
		t.Fatal("negative fx must 400")
	}
	postPricing(t, "settings", map[string]any{"fx_usd_vnd": nil})
	_, st = getPricing(t, "status")
	if st["fx"] != nil {
		t.Fatalf("fx must be null after delete: %v", st["fx"])
	}
}

// TestManagementWireBase64Body reproduces the real host wire format: the host
// marshals ManagementRequest.Body ([]byte) into JSON, so the plugin receives
// the request body as a base64 STRING inside the wire envelope — not as raw
// JSON text and not as a decoded map. Every POST that consumes a body
// (settings, override) must decode it; pre-fix this returned 400 invalid body.
func TestManagementWireBase64Body(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "wire.sqlite"))
	stubFetch(t, fixtureFetch(modelsDevFixture))

	wireCall := func(path, method string, rawBody []byte) (int, map[string]any) {
		t.Helper()
		req, _ := json.Marshal(map[string]any{
			"result": map[string]any{
				"Method":  method,
				"Path":    path,
				"Query":   map[string][]string{},
				"Headers": map[string][]string{},
				"Body":    base64.StdEncoding.EncodeToString(rawBody), // host wire shape
			},
		})
		raw, err := handleMethod("management.handle", req)
		if err != nil {
			t.Fatalf("handleMethod: %v", err)
		}
		var e envelope
		if err := json.Unmarshal(raw, &e); err != nil || !e.OK {
			t.Fatalf("envelope: %s", raw)
		}
		var resp struct {
			StatusCode int
			Body       []byte
		}
		if err := json.Unmarshal(e.Result, &resp); err != nil {
			t.Fatal(err)
		}
		var parsed map[string]any
		_ = json.Unmarshal(resp.Body, &parsed)
		return resp.StatusCode, parsed
	}

	base := "/v0/management/plugins/cpa-token-analytics/api/pricing/"

	// auto_sync toggle — the exact action that 400'd in the dashboard
	code, res := wireCall(base+"settings", "POST", []byte(`{"auto_sync":false}`))
	if code != 200 || res["ok"] != true {
		t.Fatalf("settings auto_sync over wire: code=%d res=%v", code, res)
	}
	if pricingAutoSyncEnabled() {
		t.Fatal("auto_sync must be off after wire POST")
	}

	// fx save over the wire
	code, res = wireCall(base+"settings", "POST", []byte(`{"fx_usd_vnd":26000}`))
	if code != 200 || res["ok"] != true {
		t.Fatalf("settings fx over wire: code=%d res=%v", code, res)
	}

	// override upsert over the wire
	code, res = wireCall(base+"override", "POST", []byte(`{"model":"gpt-5.4","provider":"openai","input":1.5,"output":6}`))
	if code != 200 || res["ok"] != true {
		t.Fatalf("override over wire: code=%d res=%v", code, res)
	}

	// invalid JSON still rejects cleanly (400, not a panic/other code)
	if code, _ = wireCall(base+"settings", "POST", []byte(`{"auto_sync":`)); code != 400 {
		t.Fatalf("malformed body must 400, got %d", code)
	}

	// empty body → no fields applied, still 200
	if code, res = wireCall(base+"settings", "POST", nil); code != 200 || res["ok"] != true {
		t.Fatalf("empty body: code=%d res=%v", code, res)
	}
}

// TestAutoSyncTrigger sets a stale last_sync and expects maybeAutoSync to
// schedule a background sync (async — poll briefly).
func TestAutoSyncTrigger(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "auto.sqlite"))
	var calls int64
	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		atomic.AddInt64(&calls, 1)
		return fixtureFetch(modelsDevFixture)(ctx, source)
	})
	mu.Lock()
	metaSet("pricing_last_sync", time.Now().Add(-48*time.Hour).UTC().Format(time.RFC3339))
	mu.Unlock()
	maybeAutoSync()
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt64(&calls) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if atomic.LoadInt64(&calls) == 0 {
		t.Fatal("stale last_sync must trigger a background sync")
	}
	// wait for the sync to finish writing before the test DB is torn down
	for time.Now().Before(deadline) {
		mu.Lock()
		ls := metaGet("pricing_last_sync")
		mu.Unlock()
		if ls != nil {
			if ts, err := time.Parse(time.RFC3339, *ls); err == nil && time.Since(ts) < time.Hour {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("background sync did not update last_sync")
}

// TestAutoSyncFirstRun: with auto_sync on and an empty catalog, the first
// maybeAutoSync (lazy, e.g. via /analytics/cost) schedules a background sync
// without waiting for a manual POST /sync.
func TestAutoSyncFirstRun(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "autofirst.sqlite"))
	var calls int64
	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		atomic.AddInt64(&calls, 1)
		return fixtureFetch(modelsDevFixture)(ctx, source)
	})
	maybeAutoSync()
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt64(&calls) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if atomic.LoadInt64(&calls) == 0 {
		t.Fatal("empty catalog must trigger a first-run background sync")
	}
	// wait for the sync to finish writing before the test DB is torn down
	for time.Now().Before(deadline) {
		mu.Lock()
		ls := metaGet("pricing_last_sync")
		mu.Unlock()
		if ls != nil && *ls != "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("first-run sync did not set pricing_last_sync")
}

// ---- cost endpoint ----

func TestAnalyticsCost(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "costep.sqlite"))
	mu.Lock()
	if _, err := db.Exec(`INSERT INTO price_catalog (model,provider,source,input_per_m,output_per_m,synced_at)
		VALUES ('m-cost','codex','models.dev',1,10,'2026-10-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO price_snapshots (model,provider,source,input_per_m,output_per_m,effective_from)
		VALUES ('m-cost','codex','models.dev',2,20,'2020-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()
	// Anchor seeds at local noon today: daily buckets are local-date keyed, so
	// seeding relative to wall-clock now flakes when run near midnight.
	tn := time.Now()
	now := time.Date(tn.Year(), tn.Month(), tn.Day(), 12, 0, 0, 0, tn.Location())
	seedRecord(t, now.Add(-2*time.Hour), map[string]any{
		"Provider": "codex", "Model": "m-cost",
		"Detail": map[string]int64{"InputTokens": 1000, "OutputTokens": 100, "TotalTokens": 1100},
	})
	seedRecord(t, now.Add(-time.Hour), map[string]any{
		"Provider": "codex", "Model": "m-cost",
		"Detail": map[string]int64{"InputTokens": 2000, "OutputTokens": 200, "TotalTokens": 2200},
	})
	seedRecord(t, now.Add(-30*time.Minute), map[string]any{
		"Provider": "codex", "Model": "m-noprice",
		"Detail": map[string]int64{"InputTokens": 5, "OutputTokens": 5},
	})

	resp := getAnalytics(t, "cost", nil)
	if resp["snapshot"] != "historical" || resp["billing_note"] != "api_equivalent" || resp["currency"] != "USD" {
		t.Fatalf("meta fields: %v", resp)
	}
	totals := resp["totals"].(map[string]any)
	// snapshot price 2/20: e1 = 1000*2+100*20 = 4000; e2 = 2000*2+200*20 = 8000 (µUSD)
	want := 12000 / 1e6
	if math.Abs(num(totals["priced_usd"])-want) > 1e-12 {
		t.Fatalf("priced_usd: %v want %v", totals["priced_usd"], want)
	}
	if num(totals["priced_events"]) != 2 || num(totals["unpriced_events"]) != 1 {
		t.Fatalf("event counts: %v", totals)
	}
	comp := totals["by_component"].(map[string]any)
	if num(comp["input"]) != 3000*2/1e6 || num(comp["output"]) != 300*20/1e6 {
		t.Fatalf("by_component: %v", comp)
	}
	cov := resp["coverage"].(map[string]any)
	if num(cov["total_requests"]) != 3 || num(cov["priced_requests"]) != 2 {
		t.Fatalf("coverage: %v", cov)
	}
	if math.Abs(num(cov["pct"])-200.0/3) > 0.01 {
		t.Fatalf("coverage pct: %v", cov["pct"])
	}
	un := resp["unmatched_models"].([]any)
	if len(un) != 1 || un[0].(map[string]any)["model"] != "m-noprice" {
		t.Fatalf("unmatched: %v", un)
	}
	pm := resp["per_model"].([]any)
	if len(pm) != 2 {
		t.Fatalf("per_model: %v", pm)
	}
	var mcost, mnop map[string]any
	for _, m := range pm {
		mm := m.(map[string]any)
		if mm["model"] == "m-cost" {
			mcost = mm
		} else {
			mnop = mm
		}
	}
	if mcost["price_status"] != "priced" || num(mcost["usd"]) != want {
		t.Fatalf("per_model priced: %v", mcost)
	}
	if mnop["price_status"] != "unpriced" || num(mnop["usd"]) != 0 {
		t.Fatalf("per_model unpriced: %v", mnop)
	}
	if len(resp["daily"].([]any)) != 1 {
		t.Fatalf("daily: %v", resp["daily"])
	}
	if resp["auth_type"] != "unknown" || resp["fx"] != nil {
		t.Fatalf("auth/fx: %v %v", resp["auth_type"], resp["fx"])
	}
	if num(resp["today_usd"]) != want || num(resp["month_usd"]) != want {
		t.Fatalf("today/month: %v %v", resp["today_usd"], resp["month_usd"])
	}

	// recalc=1 → latest catalog prices (1/10): e1=1000+1000=2000? 1000*1+100*10=2000µ; e2=2000+2000=4000µ
	rec := getAnalytics(t, "cost", map[string][]string{"recalc": {"1"}})
	if rec["snapshot"] != "latest" {
		t.Fatalf("recalc snapshot: %v", rec["snapshot"])
	}
	if math.Abs(num(rec["totals"].(map[string]any)["priced_usd"])-6000/1e6) > 1e-12 {
		t.Fatalf("recalc priced_usd: %v", rec["totals"])
	}
}

// TestSyncKeepsDedup: a resync must never touch usage_events.
func TestSyncKeepsDedup(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "dedup2.sqlite"))
	stubFetch(t, fixtureFetch(modelsDevFixture))
	ts := time.Now()
	seedRecord(t, ts, map[string]any{"Model": "m-x", "Detail": map[string]int64{"InputTokens": 1}})
	seedRecord(t, ts, map[string]any{"Model": "m-x", "Detail": map[string]int64{"InputTokens": 1}}) // identical → dedup
	postPricing(t, "sync", nil)
	mu.Lock()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM usage_events").Scan(&n); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()
	if n != 1 {
		t.Fatalf("dedup broken: %d rows", n)
	}
}

// TestCatalogReplaceAtomic: a second sync replaces external rows wholesale
// and appends snapshots only for changed prices.
func TestCatalogReplaceAtomic(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "atomic.sqlite"))
	var payload atomic.Value
	payload.Store(modelsDevFixture)
	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		return []byte(payload.Load().(string)), nil
	})
	postPricing(t, "sync", nil)
	mu.Lock()
	var snaps1 int
	if err := db.QueryRow("SELECT COUNT(*) FROM price_snapshots").Scan(&snaps1); err != nil {
		t.Fatal(err)
	}
	ls1 := metaGet("pricing_last_sync")
	mu.Unlock()
	if snaps1 != 5 {
		t.Fatalf("first sync snapshots: %d", snaps1)
	}
	if ls1 == nil {
		t.Fatal("last_sync missing")
	}
	sync1Ts, err := time.Parse(time.RFC3339, *ls1)
	if err != nil {
		t.Fatal(err)
	}
	// second sync, one price changed → exactly one new snapshot, catalog still 5
	time.Sleep(1100 * time.Millisecond) // RFC3339 second resolution must differ
	changed := strings.Replace(modelsDevFixture, `"input": 2.5, "output": 15, "cache_read": 0.25`, `"input": 3.5, "output": 15, "cache_read": 0.25`, 1)
	payload.Store(changed)
	postPricing(t, "sync", nil)
	mu.Lock()
	var snaps2 int
	if err := db.QueryRow("SELECT COUNT(*) FROM price_snapshots").Scan(&snaps2); err != nil {
		t.Fatal(err)
	}
	var in float64
	if err := db.QueryRow("SELECT input_per_m FROM price_catalog WHERE provider='openai' AND model='gpt-5.4'").Scan(&in); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()
	if snaps2 != 6 {
		t.Fatalf("only changed prices append snapshots: %d", snaps2)
	}
	if in != 3.5 {
		t.Fatalf("catalog updated: %v", in)
	}
	// event at first-sync time sees the old price; a later event sees the new
	ep, ok := priceFor("openai", "gpt-5.4", float64(sync1Ts.Unix()), false)
	if !ok || ep.snapshot != "historical" || *ep.row.in != 2.5 {
		t.Fatalf("historical price at sync1: %+v", ep)
	}
	ep, ok = priceFor("openai", "gpt-5.4", float64(time.Now().Unix()), false)
	if !ok || ep.snapshot != "historical" || *ep.row.in != 3.5 {
		t.Fatalf("new snapshot for later event: %+v", ep)
	}
}

// TestPricingStatusCoverageAndMatches: coverage_pct is an EVENT ratio
// (matching coverage.priced_requests/total_requests on the cost endpoint)
// while matched_count stays the distinct matched model count; status also
// emits the model_matches table (contract B.4).
func TestPricingStatusCoverageAndMatches(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "statuscov.sqlite"))
	stubFetch(t, fixtureFetch(modelsDevFixture))
	postPricing(t, "sync", nil)
	now := time.Now()
	for i, in := range []int64{100, 200, 300} {
		seedRecord(t, now.Add(time.Duration(-3-i)*time.Hour), map[string]any{
			"Provider": "openai", "Model": "gpt-5.4",
			"Detail": map[string]int64{"InputTokens": in, "OutputTokens": 10},
		})
	}
	seedRecord(t, now.Add(-30*time.Minute), map[string]any{
		"Provider": "openai", "Model": "m-noprice",
		"Detail": map[string]int64{"InputTokens": 5, "OutputTokens": 5},
	})

	status, st := getPricing(t, "status")
	if status != 200 {
		t.Fatalf("status: %d", status)
	}
	if num(st["matched_count"]) != 1 {
		t.Fatalf("matched_count must count distinct matched models: %v", st["matched_count"])
	}
	if math.Abs(num(st["coverage_pct"])-75) > 0.01 {
		t.Fatalf("coverage_pct must be event-based 3/4: %v", st["coverage_pct"])
	}
	un := st["unmatched_models"].([]any)
	if len(un) != 1 || un[0].(map[string]any)["model"] != "m-noprice" {
		t.Fatalf("unmatched_models: %v", un)
	}
	mm, ok := st["model_matches"].(map[string]any)
	if !ok || len(mm) == 0 {
		t.Fatalf("model_matches missing: %v", st)
	}
	entry, ok := mm["gpt-5.4"].(map[string]any)
	if !ok {
		t.Fatalf("model_matches must be keyed by event model: %v", mm)
	}
	if entry["matched_model"] != "gpt-5.4" || entry["provider"] != "openai" || entry["method"] != "exact" {
		t.Fatalf("model_matches entry: %v", entry)
	}
	unEntry, ok := mm["m-noprice"].(map[string]any)
	if !ok || unEntry["method"] != "unmatched" {
		t.Fatalf("unmatched pair must still appear in model_matches: %v", mm)
	}
}

// TestPriceForSnapshotFallback: after a model is removed from the catalog
// (e.g. dropped upstream), an old snapshot still prices historical events
// at-or-after its effective_from; events before the first snapshot stay
// unmatched (contract B.6).
func TestPriceForSnapshotFallback(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "snapfb.sqlite"))
	mu.Lock()
	if _, err := db.Exec(`INSERT INTO price_catalog (model,provider,source,input_per_m,output_per_m,synced_at)
		VALUES ('gone','p1','models.dev',1,10,'2026-10-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO price_snapshots (model,provider,source,input_per_m,output_per_m,effective_from)
		VALUES ('gone','p1','models.dev',5,50,'2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	// Another provider's snapshot must not leak into provider-scoped lookup.
	if _, err := db.Exec(`INSERT INTO price_snapshots (model,provider,source,input_per_m,output_per_m,effective_from)
		VALUES ('gone','p9','models.dev',9,90,'2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	// Catalog row disappears upstream (snapshot stays).
	if _, err := db.Exec(`DELETE FROM price_catalog WHERE provider='p1' AND model='gone'`); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()
	after := float64(time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC).Unix())
	before := float64(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Unix())

	ep, ok := priceFor("p1", "gone", after, false)
	if !ok || ep.snapshot != "historical" || ep.method != "snapshot-legacy" || *ep.row.in != 5 {
		t.Fatalf("deleted-catalog event must fall back to snapshot: %+v ok=%v", ep, ok)
	}
	if ep.matchedProv != "p1" || ep.matchedModel != "gone" {
		t.Fatalf("fallback provenance: %+v", ep)
	}
	// Empty provider scans all providers; equal effective_from ties break on
	// provider name, so p1's row (5) wins deterministically.
	ep, ok = priceFor("", "gone", after, false)
	if !ok || *ep.row.in != 5 || ep.matchedProv != "p1" {
		t.Fatalf("empty-provider fallback must hit some snapshot: %+v ok=%v", ep, ok)
	}
	// Events before the first snapshot stay unmatched.
	if _, ok := priceFor("p1", "gone", before, false); ok {
		t.Fatal("pre-snapshot event must stay unmatched")
	}
	// No snapshot at all → unmatched.
	if _, ok := priceFor("p1", "never-priced", after, false); ok {
		t.Fatal("model without any snapshot must stay unmatched")
	}
	// recalc also keeps the snapshot fallback (only usable price left).
	ep, ok = priceFor("p1", "gone", after, true)
	if !ok || ep.snapshot != "historical" || *ep.row.in != 5 {
		t.Fatalf("recalc must still fall back to snapshot: %+v ok=%v", ep, ok)
	}
}

// ============================================================
// Audit-fix P1-A tests (contract B2): strict provider-aware matching,
// per-model source merge, stable cost_estimates, override validation.
// ============================================================

func catPrice(t *testing.T, provider, model string) (source string, in, out float64, cr, cw *float64) {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	var crN, cwN sql.NullFloat64
	if err := db.QueryRow(`SELECT source, input_per_m, output_per_m, cache_read_per_m, cache_write_per_m
		FROM price_catalog WHERE provider=? AND model=?`, provider, model).
		Scan(&source, &in, &out, &crN, &cwN); err != nil {
		t.Fatalf("catalog row %s/%s: %v", provider, model, err)
	}
	return source, in, out, nf(crN), nf(cwN)
}

// ---- Issue 1: provider-aware strict matching ----

func TestMatchAmbiguousCrossProvider(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "amb.sqlite"))
	mu.Lock()
	seedCatalog(t, [][6]string{
		{"shared-m", "pa", "models.dev", "1", "10", "2026-10-01T00:00:00Z"},
		{"shared-m", "pb", "models.dev", "9", "90", "2026-10-01T00:00:00Z"}, // differing tuples
		{"sameprice", "pa", "models.dev", "1", "10", "2026-10-01T00:00:00Z"},
		{"sameprice", "pb", "models.dev", "1", "10", "2026-10-01T00:00:00Z"}, // identical tuples
		{"solo", "ponly", "models.dev", "2", "20", "2026-10-01T00:00:00Z"},   // single row
	})
	mu.Unlock()

	// differing prices across providers → ambiguous, unpriced
	mr := matchPrice("other", "shared-m")
	if mr.ok || mr.status != "ambiguous" {
		t.Fatalf("differing cross-provider must be ambiguous: %+v", mr)
	}
	if len(mr.candidates) != 2 || mr.candidates[0] != "pa" || mr.candidates[1] != "pb" {
		t.Fatalf("ambiguous must list candidate providers sorted: %v", mr.candidates)
	}
	// identical tuples across providers → exact (unambiguous)
	mr = matchPrice("other", "sameprice")
	if !mr.ok || mr.status != "exact" || mr.provider != "pa" {
		t.Fatalf("identical tuples must resolve deterministically: %+v", mr)
	}
	// single provider row → exact
	mr = matchPrice("other", "solo")
	if !mr.ok || mr.status != "exact" || mr.provider != "ponly" {
		t.Fatalf("single-provider bare match must be exact: %+v", mr)
	}
	// event provider among candidates → its own row, never ambiguous
	mr = matchPrice("pa", "shared-m")
	if !mr.ok || mr.status != "exact" || mr.provider != "pa" || *mr.row.in != 1 {
		t.Fatalf("own provider row must win: %+v", mr)
	}
}

func TestMatchAliasResolvesAmbiguity(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "aliasamb.sqlite"))
	mu.Lock()
	seedCatalog(t, [][6]string{
		{"shared-m", "pa", "models.dev", "1", "10", "2026-10-01T00:00:00Z"},
		{"shared-m", "openai", "models.dev", "9", "90", "2026-10-01T00:00:00Z"},
	})
	mu.Unlock()
	// codex → openai alias: deterministic provider resolution is NOT
	// cross-provider guessing → alias status even though pa also prices it.
	mr := matchPrice("codex", "shared-m")
	if !mr.ok || mr.status != "alias" || mr.provider != "openai" || *mr.row.in != 9 {
		t.Fatalf("alias must resolve within ambiguity: %+v", mr)
	}
}

func TestMatchNormalizedScopedAndDateStrip(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "normsc.sqlite"))
	mu.Lock()
	seedCatalog(t, [][6]string{
		{"sonnet", "anthropic", "models.dev", "3", "15", "2026-10-01T00:00:00Z"},
		{"sonnet", "otherx", "models.dev", "7", "70", "2026-10-01T00:00:00Z"},
		{"gemini-2.5-pro", "google", "models.dev", "1.25", "10", "2026-10-01T00:00:00Z"},
		{"vers-m-20260101", "px", "models.dev", "4", "40", "2026-10-01T00:00:00Z"},
	})
	mu.Unlock()

	// vendor/ prefix inside the model id is a provider hint → same-provider
	// scoped normalized match beats the differing cross-provider rows.
	mr := matchPrice("weird", "anthropic/sonnet")
	if !mr.ok || mr.status != "normalized" || mr.provider != "anthropic" || mr.model != "sonnet" {
		t.Fatalf("vendor prefix must scope the normalized match: %+v", mr)
	}
	// bare normalized id with two differing providers → ambiguous
	mr = matchPrice("weird", "SONNET")
	if mr.ok || mr.status != "ambiguous" {
		t.Fatalf("normalized cross-provider differing must be ambiguous: %+v", mr)
	}
	// normalized same-provider match even when other providers differ
	mr = matchPrice("google", "Gemini_2.5_Pro")
	if !mr.ok || mr.status != "normalized" || mr.provider != "google" {
		t.Fatalf("same-provider normalized: %+v", mr)
	}
	// cross-provider date strip forbidden: px absent + dated id → unmatched
	// (a foreign provider's non-dated row must NOT be picked).
	mu.Lock()
	seedCatalog(t, [][6]string{
		{"vers-m", "py", "models.dev", "5", "50", "2026-10-01T00:00:00Z"},
	})
	mu.Unlock()
	mr = matchPrice("px", "vers-m-20260202")
	if mr.ok {
		t.Fatalf("cross-provider date strip is forbidden: %+v", mr)
	}
	// same-provider date strip allowed when the dated variant is absent
	mr = matchPrice("py", "vers-m-20260202")
	if !mr.ok || mr.provider != "py" || mr.model != "vers-m" {
		t.Fatalf("same-provider date strip: %+v", mr)
	}
	// dated id present under another provider but differing → ambiguous
	mr = matchPrice("other", "vers-m-20260101")
	if !mr.ok || mr.provider != "px" {
		t.Fatalf("single-provider dated id must match: %+v", mr)
	}
}

func TestMatchMissingProviderAndUnknown(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "miss.sqlite"))
	mu.Lock()
	seedCatalog(t, [][6]string{
		{"known", "pa", "models.dev", "1", "10", "2026-10-01T00:00:00Z"},
	})
	mu.Unlock()
	if mr := matchPrice("", "known"); !mr.ok || mr.provider != "pa" || mr.status != "exact" {
		t.Fatalf("missing provider must still resolve unique model: %+v", mr)
	}
	if mr := matchPrice("ghost", "totally-unknown"); mr.ok || mr.status != "unmatched" || mr.method != "unmatched" {
		t.Fatalf("unknown model: %+v", mr)
	}
}

func TestPriceForAmbiguous(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "ambpf.sqlite"))
	mu.Lock()
	seedCatalog(t, [][6]string{
		{"shared-m", "pa", "models.dev", "1", "10", "2026-10-01T00:00:00Z"},
		{"shared-m", "pb", "models.dev", "9", "90", "2026-10-01T00:00:00Z"},
	})
	// Foreign provider's snapshot must NEVER rescue an ambiguous id.
	if _, err := db.Exec(`INSERT INTO price_snapshots (model,provider,source,input_per_m,output_per_m,effective_from)
		VALUES ('shared-m','p9','models.dev',9,90,'2020-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	// The event's OWN provider snapshot is the only allowed fallback.
	if _, err := db.Exec(`INSERT INTO price_snapshots (model,provider,source,input_per_m,output_per_m,effective_from)
		VALUES ('shared-m','pa', 'models.dev',4,44,'2021-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()
	after := float64(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Unix())

	// Event from a provider with no catalog row and no own snapshot: the
	// ambiguous id must not borrow pa's/pb's/p9's prices.
	ep, ok := priceFor("pc", "shared-m", after, false)
	if ok {
		t.Fatalf("ambiguous id must not borrow another provider's snapshot: %+v", ep)
	}
	// 'pa' has an exact catalog row AND an own-provider snapshot (2021):
	// snapshot ≤ ts wins → historical price, never the foreign p9 row.
	ep, ok = priceFor("pa", "shared-m", after, false)
	if !ok || ep.snapshot != "historical" || *ep.row.in != 4 || ep.matchedProv != "pa" {
		t.Fatalf("pa must resolve via its own snapshot: %+v ok=%v", ep, ok)
	}
}

// TestPriceForAmbiguousSnapshotScope: ambiguous event whose OWN provider has
// a snapshot gets priced from it; empty-provider ambiguous event does not.
func TestPriceForAmbiguousSnapshotScope(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "ambsnap.sqlite"))
	mu.Lock()
	seedCatalog(t, [][6]string{
		{"amb-m", "pa", "models.dev", "1", "10", "2026-10-01T00:00:00Z"},
		{"amb-m", "pb", "models.dev", "9", "90", "2026-10-01T00:00:00Z"},
	})
	// own-provider snapshot for 'pc' (not in catalog)
	if _, err := db.Exec(`INSERT INTO price_snapshots (model,provider,source,input_per_m,output_per_m,effective_from)
		VALUES ('amb-m','pc','models.dev',3,33,'2020-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()
	after := float64(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Unix())
	ep, ok := priceFor("pc", "amb-m", after, false)
	if !ok || ep.snapshot != "historical" || *ep.row.in != 3 || ep.matchedProv != "pc" {
		t.Fatalf("own-provider snapshot must price ambiguous event: %+v ok=%v", ep, ok)
	}
	// empty provider: ambiguous + no own row → must stay unpriced (no scan)
	if _, ok := priceFor("", "amb-m", after, false); ok {
		t.Fatal("empty-provider ambiguous must not scan all providers")
	}
}

func TestPricingStatusAmbiguousModels(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "ambstat.sqlite"))
	mu.Lock()
	seedCatalog(t, [][6]string{
		{"amb-m", "pa", "models.dev", "1", "10", "2026-10-01T00:00:00Z"},
		{"amb-m", "pb", "models.dev", "9", "90", "2026-10-01T00:00:00Z"},
		{"ok-m", "pa", "models.dev", "1", "10", "2026-10-01T00:00:00Z"},
	})
	mu.Unlock()
	now := time.Now()
	seedRecord(t, now.Add(-time.Hour), map[string]any{
		"Provider": "pc", "Model": "amb-m",
		"Detail": map[string]int64{"InputTokens": 10, "OutputTokens": 5},
	})
	seedRecord(t, now.Add(-30*time.Minute), map[string]any{
		"Provider": "pa", "Model": "ok-m",
		"Detail": map[string]int64{"InputTokens": 10, "OutputTokens": 5},
	})
	_, st := getPricing(t, "status")
	am, _ := st["ambiguous_models"].([]any)
	if len(am) != 1 || am[0].(map[string]any)["model"] != "amb-m" {
		t.Fatalf("ambiguous_models: %v", am)
	}
	un := st["unmatched_models"].([]any)
	if len(un) != 0 {
		t.Fatalf("ambiguous must not appear in unmatched_models: %v", un)
	}
	mm := st["model_matches"].(map[string]any)
	entry := mm["amb-m"].(map[string]any)
	if entry["status"] != "ambiguous" || entry["method"] != "ambiguous" {
		t.Fatalf("model_matches status: %v", entry)
	}
	cands, _ := entry["candidates"].([]any)
	if len(cands) != 2 {
		t.Fatalf("ambiguous candidates: %v", entry)
	}
	if entry["status"] == nil {
		t.Fatal("model_matches entries must carry status (contract B2)")
	}
}

// ---- Issue 2: per-model source merge ----

const litellmSharedFixture = `{
  "sample_spec": {"litellm_provider": "openai", "mode": "chat"},
  "gpt-5.4": {"input_cost_per_token": 9e-06, "output_cost_per_token": 99e-06, "litellm_provider": "openai"},
  "ll-only": {"input_cost_per_token": 1e-06, "output_cost_per_token": 2e-06, "litellm_provider": "openai"}
}`

func TestSyncMergeBothSources(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "merge.sqlite"))
	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		if source == pricingSourceModelsDev {
			return []byte(modelsDevFixture), nil
		}
		return []byte(litellmSharedFixture), nil
	})
	res := postPricing(t, "sync", nil)
	if res["ok"] != true || res["source"] != "models.dev+litellm" {
		t.Fatalf("merged sync: %v", res)
	}
	// contested key: models.dev wins, litellm row discarded
	src, in, out, _, _ := catPrice(t, "openai", "gpt-5.4")
	if src != "models.dev" || in != 2.5 || out != 15 {
		t.Fatalf("models.dev must win contested key: %s %v %v", src, in, out)
	}
	// models.dev-absent key filled by litellm
	src, in, out, _, _ = catPrice(t, "openai", "ll-only")
	if src != "litellm" || in != 1 || out != 2 {
		t.Fatalf("litellm must fill missing keys: %s %v %v", src, in, out)
	}
	// merged last_source
	_, st := getPricing(t, "status")
	if st["last_source"] != "models.dev+litellm" {
		t.Fatalf("last_source: %v", st["last_source"])
	}
}

func TestSyncPrimaryFailKeepsOldAndLitellmFills(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "pf.sqlite"))
	// 1. successful models.dev sync
	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		if source == pricingSourceModelsDev {
			return []byte(modelsDevFixture), nil
		}
		return nil, fmt.Errorf("litellm down")
	})
	res := postPricing(t, "sync", nil)
	if res["ok"] != true || res["source"] != "models.dev" {
		t.Fatalf("first sync: %v", res)
	}
	oldCount := catCount(t)

	// 2. models.dev fails → old models.dev rows stay, litellm fills new keys
	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		if source == pricingSourceModelsDev {
			return nil, fmt.Errorf("models.dev down")
		}
		return []byte(litellmSharedFixture), nil
	})
	res = postPricing(t, "sync", nil)
	if res["ok"] != true || res["source"] != "litellm" {
		t.Fatalf("partial sync: %v", res)
	}
	src, in, out, _, _ := catPrice(t, "openai", "gpt-5.4")
	if src != "models.dev" || in != 2.5 || out != 15 {
		t.Fatalf("old models.dev rows must survive primary failure: %s %v %v", src, in, out)
	}
	src, _, _, _, _ = catPrice(t, "openai", "ll-only")
	if src != "litellm" {
		t.Fatalf("litellm must still fill its keys: %s", src)
	}
	if got := catCount(t); got != oldCount+1 {
		t.Fatalf("catalog must grow by litellm-only keys: %d vs %d", got, oldCount)
	}
	_, st := getPricing(t, "status")
	if st["last_source"] != "litellm" {
		t.Fatalf("last_source: %v", st["last_source"])
	}
	// partial failure surfaces the error but does not corrupt the catalog
	if st["last_error"] == nil || st["last_error"] == "" {
		t.Fatalf("partial failure should record last_error: %v", st["last_error"])
	}
}

func TestSyncLitellmFailModelsDevStillApplies(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "lf.sqlite"))
	// seed a litellm-only key first
	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		if source == pricingSourceModelsDev {
			return nil, fmt.Errorf("down")
		}
		return []byte(litellmSharedFixture), nil
	})
	postPricing(t, "sync", nil)

	// now litellm fails; models.dev delivers a changed price → applied,
	// litellm rows preserved.
	changed := strings.Replace(modelsDevFixture, `"input": 2.5, "output": 15, "cache_read": 0.25`, `"input": 3.5, "output": 15, "cache_read": 0.25`, 1)
	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		if source == pricingSourceModelsDev {
			return []byte(changed), nil
		}
		return nil, fmt.Errorf("litellm down")
	})
	res := postPricing(t, "sync", nil)
	if res["ok"] != true || res["source"] != "models.dev" {
		t.Fatalf("sync: %v", res)
	}
	src, in, _, _, _ := catPrice(t, "openai", "gpt-5.4")
	if src != "models.dev" || in != 3.5 {
		t.Fatalf("models.dev apply: %s %v", src, in)
	}
	src, _, _, _, _ = catPrice(t, "openai", "ll-only")
	if src != "litellm" {
		t.Fatalf("litellm rows preserved on its own failure: %s", src)
	}
	_, st := getPricing(t, "status")
	if st["last_source"] != "models.dev" {
		t.Fatalf("last_source: %v", st["last_source"])
	}
}

func TestSyncBothFailKeepsCache(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "bothfail.sqlite"))
	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		if source == pricingSourceModelsDev {
			return []byte(modelsDevFixture), nil
		}
		return []byte(litellmSharedFixture), nil
	})
	postPricing(t, "sync", nil)
	n0 := catCount(t)

	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		return nil, fmt.Errorf("%s down", source)
	})
	res := postPricing(t, "sync", nil)
	if res["ok"] != false {
		t.Fatalf("total failure must report ok=false: %v", res)
	}
	if got := catCount(t); got != n0 {
		t.Fatalf("catalog must be preserved: %d vs %d", got, n0)
	}
	_, st := getPricing(t, "status")
	if st["last_error"] == nil {
		t.Fatal("last_error must be set")
	}
	if st["last_source"] != "models.dev+litellm" {
		t.Fatalf("last_source preserved: %v", st["last_source"])
	}
}

func TestSyncCacheSourceLabel(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "cachesrc.sqlite"))
	// catalog populated without any recorded last_source (e.g. seeded rows)
	mu.Lock()
	seedCatalog(t, [][6]string{
		{"m", "p", "models.dev", "1", "2", "2026-10-01T00:00:00Z"},
	})
	mu.Unlock()
	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		return nil, fmt.Errorf("down")
	})
	res := postPricing(t, "sync", nil)
	if res["ok"] != false {
		t.Fatalf("expected failure: %v", res)
	}
	_, st := getPricing(t, "status")
	if st["last_source"] != "cache" {
		t.Fatalf("last_source must become 'cache' when nothing recorded: %v", st["last_source"])
	}
}

func TestSyncSourceValidation(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "valid.sqlite"))
	// 1. seed a good catalog
	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		if source == pricingSourceModelsDev {
			return []byte(modelsDevFixture), nil
		}
		return nil, fmt.Errorf("litellm down")
	})
	postPricing(t, "sync", nil)
	n0 := catCount(t)

	// 2. negative / string / huge prices → per-entry skip; whole-doc garbage → source reject
	badMd := `{
	  "openai": {"models": {
	    "neg-m": {"id":"neg-m", "cost": {"input": -1, "output": 5}},
	    "str-m": {"id":"str-m", "cost": {"input": "1.5", "output": 5}},
	    "ok-m":  {"id":"ok-m",  "cost": {"input": 1, "output": 2}}
	  }}}`
	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		if source == pricingSourceModelsDev {
			return []byte(badMd), nil
		}
		return nil, fmt.Errorf("litellm down")
	})
	res := postPricing(t, "sync", nil)
	if res["ok"] != true {
		t.Fatalf("partial-valid source should sync: %v", res)
	}
	var dummy int
	if err := db.QueryRow("SELECT 1 FROM price_catalog WHERE model='neg-m'").Scan(&dummy); err == nil {
		t.Fatal("negative price entry must be skipped")
	}
	if err := db.QueryRow("SELECT 1 FROM price_catalog WHERE model='str-m'").Scan(&dummy); err == nil {
		t.Fatal("non-numeric price entry must be skipped")
	}
	src, in, out, _, _ := catPrice(t, "openai", "ok-m")
	if src != "models.dev" || in != 1 || out != 2 {
		t.Fatalf("valid entries still applied: %v %v %v", src, in, out)
	}
	_ = n0

	// 3. corrupt whole source (invalid JSON) → source's prior rows preserved
	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		if source == pricingSourceModelsDev {
			return []byte("{not json"), nil
		}
		return []byte(litellmSharedFixture), nil
	})
	res = postPricing(t, "sync", nil)
	if res["ok"] != true || res["source"] != "litellm" {
		t.Fatalf("corrupt models.dev must not fail the sync: %v", res)
	}
	src, in, out, _, _ = catPrice(t, "openai", "ok-m")
	if src != "models.dev" || in != 1 || out != 2 {
		t.Fatalf("prior models.dev rows preserved on source failure: %s %v %v", src, in, out)
	}
}

func TestMissingCacheFieldsNULL(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "nullc.sqlite"))
	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		if source == pricingSourceModelsDev {
			return []byte(modelsDevFixture), nil
		}
		return nil, fmt.Errorf("litellm down")
	})
	postPricing(t, "sync", nil)
	mu.Lock()
	var cr, cw sql.NullFloat64
	if err := db.QueryRow(`SELECT cache_read_per_m, cache_write_per_m
		FROM price_catalog WHERE provider='openai' AND model='gpt-4o-2024-08-06'`).Scan(&cr, &cw); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()
	if cr.Valid || cw.Valid {
		t.Fatalf("absent cache prices must stay NULL, not 0: %v %v", cr, cw)
	}
}

// ---- Issue 3: stable historical cost estimates ----

func estimateCount(t *testing.T) int {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM cost_estimates").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCostEstimatesStableAcrossPriceChange(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "est.sqlite"))
	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		return nil, fmt.Errorf("no network in tests")
	})
	mu.Lock()
	if _, err := db.Exec(`INSERT INTO price_catalog (model,provider,source,input_per_m,output_per_m,synced_at)
		VALUES ('m1','p1','models.dev',1,10,'2026-10-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()
	now := time.Now()
	seedRecord(t, now.Add(-2*time.Hour), map[string]any{
		"Provider": "p1", "Model": "m1",
		"Detail": map[string]int64{"InputTokens": 1000, "OutputTokens": 100},
	})

	resp := getAnalytics(t, "cost", nil)
	if resp["estimate_basis"] != "stored" || resp["snapshot"] != "historical" {
		t.Fatalf("basis/snapshot: %v %v", resp["estimate_basis"], resp["snapshot"])
	}
	want := (1000*1 + 100*10) / 1e6
	if math.Abs(num(resp["totals"].(map[string]any)["priced_usd"])-want) > 1e-12 {
		t.Fatalf("first estimate: %v", resp["totals"])
	}
	if estimateCount(t) != 1 {
		t.Fatalf("estimate must be persisted: %d", estimateCount(t))
	}

	// Price changes via a new catalog row — stored estimate must NOT move.
	mu.Lock()
	if _, err := db.Exec(`INSERT OR REPLACE INTO price_catalog (model,provider,source,input_per_m,output_per_m,synced_at)
		VALUES ('m1','p1','models.dev',99,990,'2026-10-17T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()
	resp = getAnalytics(t, "cost", nil)
	if math.Abs(num(resp["totals"].(map[string]any)["priced_usd"])-want) > 1e-12 {
		t.Fatalf("stored estimate must be stable across price changes: %v", resp["totals"])
	}
	if estimateCount(t) != 1 {
		t.Fatal("no new estimate row expected")
	}

	// recalc=1 recomputes from the latest catalog and replaces the row.
	resp = getAnalytics(t, "cost", map[string][]string{"recalc": {"1"}})
	want2 := (1000*99 + 100*990) / 1e6
	if resp["estimate_basis"] != "latest" || resp["snapshot"] != "latest" {
		t.Fatalf("recalc basis: %v %v", resp["estimate_basis"], resp["snapshot"])
	}
	if math.Abs(num(resp["totals"].(map[string]any)["priced_usd"])-want2) > 1e-9 {
		t.Fatalf("recalc must recompute at latest price: %v want %v", resp["totals"], want2)
	}
	if estimateCount(t) != 1 {
		t.Fatalf("recalc must replace, not duplicate: %d", estimateCount(t))
	}
	// subsequent read keeps the recalculated value
	resp = getAnalytics(t, "cost", nil)
	if math.Abs(num(resp["totals"].(map[string]any)["priced_usd"])-want2) > 1e-9 {
		t.Fatalf("replaced estimate must stick: %v", resp["totals"])
	}
}

func TestCostEstimatesUnpricedPartialAmbiguous(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "eststat.sqlite"))
	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		return nil, fmt.Errorf("no network")
	})
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
		"Provider": "p1", "Model": "m-full", // fully priced
		"Detail": map[string]int64{"InputTokens": 100, "OutputTokens": 10},
	})
	seedRecord(t, now.Add(-3*time.Hour), map[string]any{
		"Provider": "p1", "Model": "m-part", // partial: no output price
		"Detail": map[string]int64{"InputTokens": 1000, "OutputTokens": 100, "CacheReadTokens": 50},
	})
	seedRecord(t, now.Add(-2*time.Hour), map[string]any{
		"Provider": "p9", "Model": "amb-m", // ambiguous
		"Detail": map[string]int64{"InputTokens": 10, "OutputTokens": 10},
	})
	seedRecord(t, now.Add(-time.Hour), map[string]any{
		"Provider": "p1", "Model": "m-none", // unpriced entirely
		"Detail": map[string]int64{"InputTokens": 5, "OutputTokens": 5},
	})

	resp := getAnalytics(t, "cost", nil)
	totals := resp["totals"].(map[string]any)
	// partial + ambiguous + unpriced all count as unpriced_events
	if num(totals["unpriced_events"]) != 3 || num(totals["priced_events"]) != 1 {
		t.Fatalf("counts: %v", totals)
	}
	pm := resp["per_model"].([]any)
	st := map[string]string{}
	usd := map[string]float64{}
	for _, m := range pm {
		mm := m.(map[string]any)
		st[mm["model"].(string)] = mm["price_status"].(string)
		usd[mm["model"].(string)] = num(mm["usd"])
	}
	if st["m-full"] != "priced" || st["m-part"] != "partial" || st["amb-m"] != "ambiguous" || st["m-none"] != "unpriced" {
		t.Fatalf("per-model statuses: %v", st)
	}
	if usd["amb-m"] != 0 || usd["m-none"] != 0 {
		t.Fatalf("unpriced/ambiguous must contribute 0: %v", usd)
	}
	// partial contributes its priced components only: baseIn=1000-50=950 at
	// p_in=1 plus 50 cache_read at 0.1 (output has no price → excluded).
	wantPart := (950*1 + 50*0.1) / 1e6
	if math.Abs(usd["m-part"]-wantPart) > 1e-12 {
		t.Fatalf("partial usd: %v want %v", usd["m-part"], wantPart)
	}
	// persisted rows: unpriced/ambiguous have NULL usd, partial stores NULL
	// for the unpriced component.
	type estRow struct {
		status    string
		usd, outU sql.NullFloat64
	}
	readEst := func() map[string]estRow {
		mu.Lock()
		defer mu.Unlock()
		got := map[string]estRow{}
		rows, err := db.Query(`SELECT c.model, c.status, c.usd, c.output_usd FROM cost_estimates c`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var model string
			var er estRow
			if err := rows.Scan(&model, &er.status, &er.usd, &er.outU); err != nil {
				t.Fatal(err)
			}
			got[model] = er
		}
		return got
	}
	got := readEst()
	if got["m-full"].status != "priced" || !got["m-full"].usd.Valid || !got["m-full"].outU.Valid {
		t.Fatalf("priced row must carry component costs: %+v", got["m-full"])
	}
	if got["m-none"].status != "unpriced" || got["m-none"].usd.Valid {
		t.Fatalf("unpriced row: %+v", got["m-none"])
	}
	if got["amb-m"].status != "ambiguous" || got["amb-m"].usd.Valid {
		t.Fatalf("ambiguous row: %+v", got["amb-m"])
	}
	if got["m-part"].status != "partial" || !got["m-part"].usd.Valid || got["m-part"].outU.Valid {
		t.Fatalf("partial row: usd must be set, missing component NULL: %+v", got["m-part"])
	}
	if got["amb-m"].status == "unpriced" {
		t.Fatal("ambiguous must be its own status")
	}
	// idempotent: a second call inserts nothing
	getAnalytics(t, "cost", nil)
	if n := estimateCount(t); n != 4 {
		t.Fatalf("INSERT OR IGNORE must not duplicate rows: %d", n)
	}
}

func TestCostEstimatesOverrideDoesNotRewrite(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "estovr.sqlite"))
	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		return nil, fmt.Errorf("no network")
	})
	mu.Lock()
	if _, err := db.Exec(`INSERT INTO price_catalog (model,provider,source,input_per_m,output_per_m,synced_at)
		VALUES ('m1','p1','models.dev',1,10,'2026-10-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()
	seedRecord(t, time.Now().Add(-time.Hour), map[string]any{
		"Provider": "p1", "Model": "m1",
		"Detail": map[string]int64{"InputTokens": 1000, "OutputTokens": 100},
	})
	resp := getAnalytics(t, "cost", nil)
	want := (1000*1 + 100*10) / 1e6
	if math.Abs(num(resp["totals"].(map[string]any)["priced_usd"])-want) > 1e-12 {
		t.Fatalf("baseline: %v", resp["totals"])
	}
	// override lands AFTER the estimate → stored row untouched
	res := postPricing(t, "override", map[string]any{"model": "m1", "provider": "p1", "input": 50, "output": 500})
	if res["ok"] != true {
		t.Fatalf("override: %v", res)
	}
	resp = getAnalytics(t, "cost", nil)
	if math.Abs(num(resp["totals"].(map[string]any)["priced_usd"])-want) > 1e-12 {
		t.Fatalf("override must not rewrite stored estimate: %v", resp["totals"])
	}
	// ...but a NEW event picks the override up
	seedRecord(t, time.Now().Add(-30*time.Minute), map[string]any{
		"Provider": "p1", "Model": "m1",
		"Detail": map[string]int64{"InputTokens": 100, "OutputTokens": 10},
	})
	resp = getAnalytics(t, "cost", nil)
	wantNew := want + (100*50+10*500)/1e6
	if math.Abs(num(resp["totals"].(map[string]any)["priced_usd"])-wantNew) > 1e-9 {
		t.Fatalf("override must price not-yet-estimated events: %v want %v", resp["totals"], wantNew)
	}
	// recalc replaces stored rows at override prices
	resp = getAnalytics(t, "cost", map[string][]string{"recalc": {"1"}})
	wantAll := (1000*50 + 100*500 + 100*50 + 10*500) / 1e6
	if math.Abs(num(resp["totals"].(map[string]any)["priced_usd"])-wantAll) > 1e-9 {
		t.Fatalf("recalc at override price: %v want %v", resp["totals"], wantAll)
	}
}

func TestCostEstimateOldEventReadable(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "estold.sqlite"))
	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		return nil, fmt.Errorf("no network")
	})
	mu.Lock()
	if _, err := db.Exec(`INSERT INTO price_catalog (model,provider,source,input_per_m,output_per_m,synced_at)
		VALUES ('m1','p1','models.dev',1,10,'2026-10-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO price_snapshots (model,provider,source,input_per_m,output_per_m,effective_from)
		VALUES ('m1','p1','models.dev',2,20,'2020-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()
	seedRecord(t, time.Date(2024, 3, 5, 10, 0, 0, 0, time.Local), map[string]any{
		"Provider": "p1", "Model": "m1",
		"Detail": map[string]int64{"InputTokens": 1000, "OutputTokens": 100},
	})
	resp := getAnalytics(t, "cost", map[string][]string{
		"start": {"2024-03-01"}, "end": {"2024-03-31"},
	})
	// snapshot price 2/20 applies (event predates latest catalog sync too,
	// but the snapshot effective 2020 covers it)
	want := (1000*2 + 100*20) / 1e6
	if math.Abs(num(resp["totals"].(map[string]any)["priced_usd"])-want) > 1e-12 {
		t.Fatalf("old event must read: %v want %v", resp["totals"], want)
	}
	mu.Lock()
	defer mu.Unlock()
	var sid sql.NullInt64
	var src, st string
	if err := db.QueryRow(`SELECT snapshot_id, source, status FROM cost_estimates`).Scan(&sid, &src, &st); err != nil {
		t.Fatal(err)
	}
	if !sid.Valid || src != "models.dev" || st != "priced" {
		t.Fatalf("estimate must record snapshot_id+source+status: %v %v %v", sid, src, st)
	}
}

func TestCostEstimateHeatmapAndComparison(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "esthc.sqlite"))
	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		return nil, fmt.Errorf("no network")
	})
	mu.Lock()
	if _, err := db.Exec(`INSERT INTO price_catalog (model,provider,source,input_per_m,output_per_m,synced_at)
		VALUES ('m1','p1','models.dev',1,10,'2026-10-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()
	ts := time.Date(2026, 9, 10, 15, 30, 0, 0, time.Local)
	seedRecord(t, ts, map[string]any{
		"Provider": "p1", "Model": "m1",
		"Detail": map[string]int64{"InputTokens": 1000, "OutputTokens": 100},
	})
	// heatmap sums stored/computed estimate usd
	resp := getAnalytics(t, "heatmap", map[string][]string{
		"start": {"2026-09-10"}, "end": {"2026-09-10"},
	})
	costs := resp["dow_hours"].(map[string]any)["cost_usd"].([]any)
	want := (1000*1 + 100*10) / 1e6
	got := num(costs[3].([]any)[15])
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("heatmap cost cell: %v want %v", got, want)
	}
	if estimateCount(t) != 1 {
		t.Fatalf("heatmap must persist the estimate: %d", estimateCount(t))
	}
	// comparison cost_usd uses stored estimates too
	seedRecord(t, ts.Add(-24*time.Hour), map[string]any{
		"Provider": "p1", "Model": "m1",
		"Detail": map[string]int64{"InputTokens": 2000, "OutputTokens": 200},
	})
	resp = getAnalytics(t, "comparison", map[string][]string{
		"preset": {"custom"}, "custom_start": {"2026-09-10"}, "custom_end": {"2026-09-10"}, "mode": {"full"},
	})
	cm := resp["metrics"].(map[string]any)["cost_usd"].(map[string]any)
	if math.Abs(num(cm["current"])-want) > 1e-12 {
		t.Fatalf("comparison cost_usd: %v want %v", cm, want)
	}
	if cm["previous"] == nil {
		t.Fatal("previous period (priced event) must report a cost")
	}
}

// ---- Issue 4 (BE): override price validation ----

func TestOverrideInvalidPrice(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "ovrval.sqlite"))
	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		return nil, fmt.Errorf("no network")
	})

	bad := []map[string]any{
		{"model": "x", "input": -1},
		{"model": "x", "input": -0.001},
		{"model": "x", "input": "5"},
		{"model": "x", "input": true},
		{"model": "x", "output": map[string]any{"a": 1}},
	}
	for _, b := range bad {
		s, raw := callMgmtBody(t, "/v0/management/plugins/cpa-token-analytics/api/pricing/override", "POST", b)
		if s != 400 {
			t.Fatalf("invalid price %v must 400, got %d %s", b, s, raw)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if out["ok"] != false || out["error"] != "invalid_price" {
			t.Fatalf("must return {ok:false,error:invalid_price}: %s", raw)
		}
	}
	// NaN/±Inf must reject too — send raw JSON (map can't carry 1e999)
	s, raw := callMgmtBody(t, "/v0/management/plugins/cpa-token-analytics/api/pricing/override", "POST",
		`{"model":"x","input":1e999}`)
	if s != 400 {
		t.Fatalf("Inf price must 400, got %d %s", s, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err == nil && (out["ok"] != false || out["error"] != "invalid_price") {
		t.Fatalf("Inf must map to invalid_price: %s", raw)
	}
	// no partial save on a mixed payload
	s, _ = callMgmtBody(t, "/v0/management/plugins/cpa-token-analytics/api/pricing/override", "POST",
		map[string]any{"model": "mix", "input": 1, "output": "bogus"})
	if s != 400 {
		t.Fatalf("mixed good+bad must reject entirely, got %d", s)
	}
	mu.Lock()
	var n int
	_ = db.QueryRow("SELECT COUNT(*) FROM price_overrides WHERE model='mix'").Scan(&n)
	mu.Unlock()
	if n != 0 {
		t.Fatal("invalid payload must not partially save")
	}
	// null fields are skipped (cleared), absent fields likewise
	res := postPricing(t, "override", map[string]any{"model": "ok", "input": 1, "output": nil})
	if res["ok"] != true {
		t.Fatalf("null field must be accepted as absent: %v", res)
	}
	mu.Lock()
	var in float64
	var outp sql.NullFloat64
	if err := db.QueryRow("SELECT input_per_m, output_per_m FROM price_overrides WHERE model='ok'").Scan(&in, &outp); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()
	if in != 1 || outp.Valid {
		t.Fatalf("null must store NULL (field skipped): %v %v", in, outp)
	}
}

// ============================================================
// Gate rework tests (F1-F4, F7a, F7d). Add-only.
// ============================================================

// ---- F1: commitCatalog must not snapshot a discarded source row ----
//
// Scenario: models.dev owns (openai, gpt-5.4) at in=2.5. A later sync where
// models.dev fails entirely (mdCat=nil) but litellm returns the SAME key at
// in=9 must leave the catalog row at 2.5 AND must not record the discarded
// litellm price into price_snapshots — otherwise post-sync events resolve
// against a price the catalog does not carry.
func TestCommitCatalogDiscardedRowNoSnapshot(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "f1.sqlite"))
	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		return nil, fmt.Errorf("no network")
	})

	mu.Lock()
	md := map[string]map[string]priceRow{
		"openai": {"gpt-5.4": {in: fptr(2.5), out: fptr(15)}},
	}
	if err := commitCatalog(md, nil, "2026-10-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()

	src, in, _, _, _ := catPrice(t, "openai", "gpt-5.4")
	if src != "models.dev" || in != 2.5 {
		t.Fatalf("seed: %s %v", src, in)
	}

	// T2: models.dev failed (nil catalog), litellm contests the same key.
	mu.Lock()
	ll := map[string]map[string]priceRow{
		"openai": {"gpt-5.4": {in: fptr(9), out: fptr(99)}},
	}
	if err := commitCatalog(nil, ll, "2026-10-02T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	var snaps int
	var latestIn float64
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE((SELECT input_per_m FROM price_snapshots
		WHERE provider='openai' AND model='gpt-5.4' ORDER BY effective_from DESC LIMIT 1), -1)
		FROM price_snapshots`).Scan(&snaps, &latestIn); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()

	src, in, out, _, _ := catPrice(t, "openai", "gpt-5.4")
	if src != "models.dev" || in != 2.5 || out != 15 {
		t.Fatalf("models.dev row must survive litellm contest: %s %v %v", src, in, out)
	}
	if snaps != 1 || latestIn != 2.5 {
		t.Fatalf("discarded litellm row must not append a snapshot: snaps=%d latest_in=%v", snaps, latestIn)
	}
	// Event after T2 resolves to 2.5, never the discarded 9.
	evAt := float64(time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC).Unix())
	ep, ok := priceFor("openai", "gpt-5.4", evAt, false)
	if !ok || *ep.row.in != 2.5 || *ep.row.out != 15 {
		t.Fatalf("post-sync resolution must use models.dev price: %+v ok=%v", ep, ok)
	}
	if ep.snapshot != "historical" {
		t.Fatalf("event after a snapshot must resolve historically: %+v", ep)
	}

	// Mirror the same contest through analytics: the event is priced at the
	// models.dev row, not the discarded litellm price.
	seedRecord(t, time.Now().Add(-time.Hour), map[string]any{
		"Provider": "openai", "Model": "gpt-5.4",
		"Detail": map[string]int64{"InputTokens": 1000, "OutputTokens": 100},
	})
	resp := getAnalytics(t, "cost", nil)
	want := (1000*2.5 + 100*15) / 1e6
	if math.Abs(num(resp["totals"].(map[string]any)["priced_usd"])-want) > 1e-9 {
		t.Fatalf("analytics must price at catalog price 2.5, not discarded 9: %v", resp["totals"])
	}
}

// F1 second leg: an override owner still gets the mirror snapshot — the fix
// must not starve the existing override→history semantics (external price
// changes keep landing in price_snapshots even though the override row owns
// the catalog PK and the insert is ignored).
func TestCommitCatalogOverrideOwnerStillSnapshots(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "f1o.sqlite"))
	mu.Lock()
	if _, err := db.Exec(`INSERT INTO price_overrides (provider,model,input_per_m,output_per_m,updated_at)
		VALUES ('p1','m-ovr',100,200,'2026-10-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO price_catalog (model,provider,source,input_per_m,output_per_m,synced_at)
		VALUES ('m-ovr','p1','override',100,200,'2026-10-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	// Prior external history exists so the mirror has a baseline to diff.
	if _, err := db.Exec(`INSERT INTO price_snapshots (model,provider,source,input_per_m,output_per_m,effective_from)
		VALUES ('m-ovr','p1','models.dev',2,20,'2026-09-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	// models.dev sync carrying the overridden key at a NEW external price:
	// insert is ignored (override owns the PK) but the external price still
	// lands in snapshots so history keeps tracking the upstream row.
	md := map[string]map[string]priceRow{
		"p1": {"m-ovr": {in: fptr(3), out: fptr(30)}},
	}
	if err := commitCatalog(md, nil, "2026-10-02T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	var snaps int
	var in float64
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE((SELECT input_per_m FROM price_snapshots
		WHERE provider='p1' AND model='m-ovr' ORDER BY effective_from DESC LIMIT 1), -1)
		FROM price_snapshots`).Scan(&snaps, &in); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()
	if snaps != 2 || in != 3 {
		t.Fatalf("override-owned key must keep mirroring external history: snaps=%d in=%v", snaps, in)
	}
}

// ---- F2: pricingStatus must agree with analyticsCost (snapshot fallback) ----
//
// An event whose model is gone from the catalog but has a historical
// snapshot is priced by analyticsCost; pricingStatus must count it as priced
// (not unmatched) and report status "historical" in model_matches.
func TestPricingStatusSnapshotFallbackCoverage(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "f2.sqlite"))
	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		return nil, fmt.Errorf("no network")
	})
	mu.Lock()
	if _, err := db.Exec(`INSERT INTO price_snapshots (model,provider,source,input_per_m,output_per_m,effective_from)
		VALUES ('rw-gone','p1','models.dev',5,50,'2020-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()
	now := time.Now()
	seedRecord(t, now.Add(-2*time.Hour), map[string]any{
		"Provider": "p1", "Model": "rw-gone",
		"Detail": map[string]int64{"InputTokens": 100, "OutputTokens": 10},
	})
	seedRecord(t, now.Add(-time.Hour), map[string]any{
		"Provider": "p1", "Model": "rw-none", // genuinely unpriced
		"Detail": map[string]int64{"InputTokens": 5, "OutputTokens": 5},
	})

	// analytics prices rw-gone via the snapshot (5/50): 100*5 + 10*50 = 1000µ
	resp := getAnalytics(t, "cost", nil)
	want := (100*5 + 10*50) / 1e6
	if math.Abs(num(resp["totals"].(map[string]any)["priced_usd"])-want) > 1e-12 {
		t.Fatalf("analyticsCost snapshot pricing: %v", resp["totals"])
	}
	if num(resp["coverage"].(map[string]any)["priced_requests"]) != 1 {
		t.Fatalf("analytics coverage: %v", resp["coverage"])
	}

	status, st := getPricing(t, "status")
	if status != 200 {
		t.Fatalf("status: %d", status)
	}
	// coverage must match analyticsCost: 1 of 2 events priced → 50%
	if math.Abs(num(st["coverage_pct"])-50) > 0.01 {
		t.Fatalf("coverage_pct must count snapshot-priced events: %v", st["coverage_pct"])
	}
	if num(st["matched_count"]) != 1 {
		t.Fatalf("matched_count: %v", st["matched_count"])
	}
	un := st["unmatched_models"].([]any)
	if len(un) != 1 || un[0].(map[string]any)["model"] != "rw-none" {
		t.Fatalf("snapshot-priced model must not be unmatched: %v", un)
	}
	entry, ok := st["model_matches"].(map[string]any)["rw-gone"].(map[string]any)
	if !ok || entry["status"] != "historical" || entry["method"] != "snapshot-legacy" {
		t.Fatalf("model_matches must report historical: %v", entry)
	}
}

// ---- F3: provider normalization (case/trim) ----

func TestProviderNormalization(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "f3.sqlite"))
	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		return nil, fmt.Errorf("no network")
	})
	mu.Lock()
	seedCatalog(t, [][6]string{
		{"m", "openai", "models.dev", "1", "10", "2026-10-01T00:00:00Z"},
	})
	mu.Unlock()

	// Event provider casing/whitespace must exact-match the catalog row.
	for _, p := range []string{"OpenAI", " openai ", "OPENAI"} {
		mr := matchPrice(p, "m")
		if !mr.ok || mr.status != "exact" || mr.provider != "openai" {
			t.Fatalf("provider %q must normalize to exact match: %+v", p, mr)
		}
	}

	// Override written under "OpenAI" must apply to events priced as "openai"
	// (and vice-versa: the override row is stored under the normalized id).
	res := postPricing(t, "override", map[string]any{
		"model": "m", "provider": "OpenAI", "input": 7, "output": 70,
	})
	if res["ok"] != true {
		t.Fatalf("override: %v", res)
	}
	mu.Lock()
	var storedProv string
	if err := db.QueryRow("SELECT provider FROM price_overrides WHERE model='m'").Scan(&storedProv); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()
	if storedProv != "openai" {
		t.Fatalf("override must persist normalized provider: %q", storedProv)
	}
	ep, ok := priceFor("openai", "m", float64(time.Now().Unix()), false)
	if !ok || ep.snapshot != "override" || *ep.row.in != 7 {
		t.Fatalf("normalized override must price event provider: %+v", ep)
	}
	ep, ok = priceFor("OPENAI", "m", float64(time.Now().Unix()), false)
	if !ok || ep.snapshot != "override" || *ep.row.in != 7 {
		t.Fatalf("override must apply regardless of event casing: %+v", ep)
	}
}

// ---- F4: global override (provider="") is GLOBAL ----
//
// A provider=” override row beats even a same-provider catalog row for the
// model, and reports snapshot:"override".
func TestGlobalOverrideWins(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "f4.sqlite"))
	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		return nil, fmt.Errorf("no network")
	})
	mu.Lock()
	seedCatalog(t, [][6]string{
		{"m", "p1", "models.dev", "1", "10", "2026-10-01T00:00:00Z"},
	})
	mu.Unlock()

	res := postPricing(t, "override", map[string]any{
		"model": "m", "provider": "", "input": 9, "output": 90,
	})
	if res["ok"] != true {
		t.Fatalf("global override: %v", res)
	}
	now := float64(time.Now().Unix())
	ep, ok := priceFor("p1", "m", now, false)
	if !ok || ep.snapshot != "override" || *ep.row.in != 9 {
		t.Fatalf("global override must beat p1's catalog row: %+v ok=%v", ep, ok)
	}
	if ep.source != pricingSourceOverride {
		t.Fatalf("override source label: %+v", ep)
	}
	ep, ok = priceFor("other-prov", "m", now, false)
	if !ok || ep.snapshot != "override" || *ep.row.in != 9 {
		t.Fatalf("global override must cover any provider: %+v ok=%v", ep, ok)
	}
	// A scoped override for p2 still wins over the global one for p2 only.
	res = postPricing(t, "override", map[string]any{
		"model": "m", "provider": "p2", "input": 5, "output": 5,
	})
	ep, ok = priceFor("p2", "m", now, false)
	if !ok || ep.snapshot != "override" || *ep.row.in != 5 {
		t.Fatalf("scoped override must beat global for its own provider: %+v", ep)
	}
	ep, ok = priceFor("p3", "m", now, false)
	if !ok || *ep.row.in != 9 {
		t.Fatalf("p3 must keep resolving via the global override: %+v", ep)
	}
	// analytics path agrees: the event prices at the global override.
	seedRecord(t, time.Now().Add(-time.Hour), map[string]any{
		"Provider": "p1", "Model": "m",
		"Detail": map[string]int64{"InputTokens": 100, "OutputTokens": 10},
	})
	resp := getAnalytics(t, "cost", nil)
	want := (100*9 + 10*90) / 1e6
	if math.Abs(num(resp["totals"].(map[string]any)["priced_usd"])-want) > 1e-9 {
		t.Fatalf("analytics must use global override: %v", resp["totals"])
	}
}

// ---- F7a: parseModelsDev accepts one-sided price rows ----

func TestParseModelsDevPartialRows(t *testing.T) {
	cat, _, err := parseModelsDev([]byte(`{
	  "p": {"models": {
	    "out-only": {"cost": {"output": 5}},
	    "in-only":  {"cost": {"input": 1}},
	    "none":     {"cost": {"cache_read": 0.1}},
	    "null-in":  {"cost": {"input": null, "output": 2}},
	    "empty":    {"cost": {}}
	  }}}`))
	if err != nil {
		t.Fatal(err)
	}
	r, ok := cat["p"]["out-only"]
	if !ok || r.in != nil || *r.out != 5 {
		t.Fatalf("output-only row must be kept: %+v", r)
	}
	if r2, ok := cat["p"]["in-only"]; !ok || r2.out != nil || *r2.in != 1 {
		t.Fatalf("input-only row must be kept: %+v", r2)
	}
	if r3, ok := cat["p"]["null-in"]; !ok || r3.in != nil || *r3.out != 2 {
		t.Fatalf("explicit null input must behave as absent: %+v", r3)
	}
	if _, ok := cat["p"]["none"]; ok {
		t.Fatal("cache-only row (no input/output price) must be skipped")
	}
	if _, ok := cat["p"]["empty"]; ok {
		t.Fatal("price-less row must be skipped")
	}
}

// ---- F7d: delete:true ignores invalid price fields ----

func TestOverrideDeleteSkipsPriceValidation(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "f7d.sqlite"))
	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		return nil, fmt.Errorf("no network")
	})
	res := postPricing(t, "override", map[string]any{
		"model": "dm", "provider": "p1", "input": 1, "output": 2,
	})
	if res["ok"] != true {
		t.Fatalf("seed override: %v", res)
	}
	// delete:true with garbage price fields must still delete (400 before fix).
	s, raw := callMgmtBody(t, "/v0/management/plugins/cpa-token-analytics/api/pricing/override", "POST",
		map[string]any{"model": "dm", "provider": "p1", "delete": true, "input": "abc"})
	if s != 200 {
		t.Fatalf("delete must not validate prices, got %d %s", s, raw)
	}
	mu.Lock()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM price_overrides WHERE model='dm'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()
	if n != 0 {
		t.Fatal("delete must remove the override row")
	}
}

// ---- Issue 6: pricing catalog cache — invalidation, correctness, races ----

// A cached pricing snapshot must observe override writes on the very next
// read (endpoint and direct writes alike, via the generation bump).
func TestPricingCacheInvalidationOnOverride(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "cache-ovr.sqlite"))
	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		return nil, fmt.Errorf("no network")
	})
	mu.Lock()
	seedCatalog(t, [][6]string{
		{"m", "p1", "models.dev", "1", "10", "2026-10-01T00:00:00Z"},
	})
	mu.Unlock()
	ts := float64(time.Now().Unix())

	ep, ok := priceFor("p1", "m", ts, true)
	if !ok || ep.snapshot != "latest" || *ep.row.in != 1 {
		t.Fatalf("warm: %+v ok=%v", ep, ok)
	}
	// Endpoint upsert → next priceFor must return the override.
	res := postPricing(t, "override", map[string]any{
		"model": "m", "provider": "p1", "input": 100, "output": 200,
	})
	if res["ok"] != true {
		t.Fatalf("override: %v", res)
	}
	ep, ok = priceFor("p1", "m", ts, false)
	if !ok || ep.snapshot != "override" || *ep.row.in != 100 {
		t.Fatalf("override must be visible immediately: %+v ok=%v", ep, ok)
	}
	// Endpoint delete → catalog row must be restored from its snapshot.
	mu.Lock()
	if _, err := db.Exec(`INSERT INTO price_snapshots (model,provider,source,input_per_m,output_per_m,effective_from)
		VALUES ('m','p1','models.dev',2,20,'2026-10-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()
	res = postPricing(t, "override", map[string]any{
		"model": "m", "provider": "p1", "delete": true,
	})
	if res["ok"] != true {
		t.Fatalf("delete: %v", res)
	}
	ep, ok = priceFor("p1", "m", ts, false)
	if !ok || ep.snapshot != "historical" || *ep.row.in != 2 {
		t.Fatalf("delete must restore snapshot pricing: %+v ok=%v", ep, ok)
	}
	// Direct write (bypassing endpoints) must also invalidate — the same
	// pattern existing tests rely on.
	mu.Lock()
	if _, err := db.Exec(`INSERT INTO price_overrides (provider,model,input_per_m,output_per_m,updated_at)
		VALUES ('p1','m',55,550,'2026-10-17T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()
	ep, ok = priceFor("p1", "m", ts, false)
	if !ok || ep.snapshot != "override" || *ep.row.in != 55 {
		t.Fatalf("direct override write must invalidate the cache: %+v ok=%v", ep, ok)
	}
}

// Two catalog commits at different effective_from values must keep snapshot
// resolution separated: events before/after T2 resolve to their own price.
func TestPricingCacheSnapshotSeparationAcrossSyncs(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "cache-sync.sqlite"))
	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		return nil, fmt.Errorf("no network")
	})
	tsE := float64(time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC).Unix())  // between T1 and T2
	tsL := float64(time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC).Unix()) // after T2
	tsOld := float64(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Unix()) // before T1

	mu.Lock()
	md1 := map[string]map[string]priceRow{"p1": {"m": {in: fptr(1), out: fptr(10)}}}
	if err := commitCatalog(md1, nil, "2026-10-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()

	ep, ok := priceFor("p1", "m", tsE, false)
	if !ok || ep.snapshot != "historical" || *ep.row.in != 1 {
		t.Fatalf("T1 snapshot must price mid events at 1: %+v ok=%v", ep, ok)
	}

	mu.Lock()
	md2 := map[string]map[string]priceRow{"p1": {"m": {in: fptr(9), out: fptr(90)}}}
	if err := commitCatalog(md2, nil, "2026-10-15T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()

	// Mid-window event keeps T1 price; late event moves to T2; pre-history
	// falls back to latest (9); recalc reports latest (9) — no bleed across.
	ep, ok = priceFor("p1", "m", tsE, false)
	if !ok || ep.snapshot != "historical" || *ep.row.in != 1 {
		t.Fatalf("pre-T2 event must stay at T1 price 1: %+v", ep)
	}
	ep, ok = priceFor("p1", "m", tsL, false)
	if !ok || ep.snapshot != "historical" || *ep.row.in != 9 {
		t.Fatalf("post-T2 event must resolve to T2 price 9: %+v", ep)
	}
	ep, ok = priceFor("p1", "m", tsOld, false)
	if !ok || ep.snapshot != "latest" || *ep.row.in != 9 {
		t.Fatalf("pre-history event must use latest 9: %+v", ep)
	}
	ep, ok = priceFor("p1", "m", tsL, true)
	if !ok || ep.snapshot != "latest" || *ep.row.in != 9 {
		t.Fatalf("recalc must use latest 9: %+v", ep)
	}

	// resolveEstimates must agree with priceFor at every timestamp.
	evs := []eventRow{
		{ts: tsOld, provider: "p1", model: "m", input: 1000},
		{ts: tsE, provider: "p1", model: "m", input: 1000},
		{ts: tsL, provider: "p1", model: "m", input: 1000},
	}
	infos, _ := resolveEstimates(evs, false)
	want := []float64{1000 * 9 / 1e6, 1000 * 1 / 1e6, 1000 * 9 / 1e6}
	for i, c := range infos {
		if !c.matched || math.Abs(c.usd-want[i]) > 1e-12 {
			t.Fatalf("resolveEstimates event %d: %+v want usd %v", i, c, want[i])
		}
	}
}

// Gate-P1 F5: an event with a stored priced estimate whose CURRENT catalog
// match is ambiguous must appear in ambiguous_models (and still count its
// stored USD — estimates are stable).
func TestAmbiguousModelsStoredEstimate(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "f5.sqlite"))
	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		return nil, fmt.Errorf("no network")
	})
	mu.Lock()
	seedCatalog(t, [][6]string{
		{"m", "p1", "models.dev", "1", "10", "2026-10-01T00:00:00Z"},
	})
	mu.Unlock()
	seedRecord(t, time.Now().Add(-2*time.Hour), map[string]any{
		"Provider": "p1", "Model": "m",
		"Detail": map[string]int64{"InputTokens": 1000, "OutputTokens": 100},
	})

	resp := getAnalytics(t, "cost", nil)
	want := (1000*1 + 100*10) / 1e6
	if math.Abs(num(resp["totals"].(map[string]any)["priced_usd"])-want) > 1e-12 {
		t.Fatalf("first estimate: %v", resp["totals"])
	}
	if estimateCount(t) != 1 {
		t.Fatal("estimate must be stored")
	}

	// Catalog changes so (p1,m) now matches two providers with different
	// price tuples → live status ambiguous; the stored USD stays.
	mu.Lock()
	if _, err := db.Exec(`DELETE FROM price_catalog WHERE provider='p1' AND model='m'`); err != nil {
		t.Fatal(err)
	}
	seedCatalog(t, [][6]string{
		{"m", "pa", "models.dev", "2", "20", "2026-10-02T00:00:00Z"},
		{"m", "pb", "models.dev", "3", "30", "2026-10-02T00:00:00Z"},
	})
	mu.Unlock()

	resp = getAnalytics(t, "cost", nil)
	totals := resp["totals"].(map[string]any)
	if math.Abs(num(totals["priced_usd"])-want) > 1e-12 {
		t.Fatalf("stored estimate must keep its USD: %v", totals)
	}
	amb := resp["ambiguous_models"].([]any)
	if len(amb) != 1 || amb[0].(map[string]any)["model"] != "m" || amb[0].(map[string]any)["provider"] != "p1" {
		t.Fatalf("stored-but-ambiguous pair must be listed: %v", amb)
	}
	if un := resp["unmatched_models"].([]any); len(un) != 0 {
		t.Fatalf("ambiguous stored pair must not be unmatched: %v", un)
	}
	// model status stays priced (stored USD is authoritative).
	pm := resp["per_model"].([]any)
	if len(pm) != 1 || pm[0].(map[string]any)["price_status"] != "priced" {
		t.Fatalf("per_model price_status: %v", pm)
	}
	// pricingStatus agrees: the pair is ambiguous there too.
	if _, st := getPricing(t, "status"); st != nil {
		if sa, ok := st["ambiguous_models"].([]any); !ok || len(sa) != 1 {
			t.Fatalf("pricingStatus ambiguous_models: %v", st["ambiguous_models"])
		}
	}
}

// Concurrent readers (priceFor + resolveEstimates) racing override writes
// must never observe torn state: the price is either the catalog value or
// the override value, never anything else. Run under -race.
func TestPricingCacheConcurrentInvalidation(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "cache-race.sqlite"))
	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		return nil, fmt.Errorf("no network")
	})
	mu.Lock()
	seedCatalog(t, [][6]string{
		{"rm", "p1", "models.dev", "1", "10", "2026-10-01T00:00:00Z"},
	})
	mu.Unlock()
	ts := float64(time.Now().Unix())
	evs := []eventRow{{ts: ts, provider: "p1", model: "rm", input: 100}}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	errs := make(chan error, 8)
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				ep, ok := priceFor("p1", "rm", ts, false)
				if ok {
					if v := *ep.row.in; v != 1 && v != 100 {
						errs <- fmt.Errorf("torn price read: %v", v)
						return
					}
				}
				infos, _ := resolveEstimates(evs, false)
				if len(infos) != 1 {
					errs <- fmt.Errorf("resolveEstimates len %d", len(infos))
					return
				}
			}
		}()
	}
	// Writer: flip the override on/off 50 times via the real endpoint.
	for i := 0; i < 50; i++ {
		res := postPricing(t, "override", map[string]any{
			"model": "rm", "provider": "p1", "input": 100, "output": 200,
		})
		if res["ok"] != true {
			t.Fatalf("override: %v", res)
		}
		res = postPricing(t, "override", map[string]any{
			"model": "rm", "provider": "p1", "delete": true,
		})
		if res["ok"] != true {
			t.Fatalf("delete: %v", res)
		}
	}
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// ---- benchmarks: resolution no longer queries per event ----
//
// BenchmarkResolveEstimates measures estimate resolution over synthetic
// in-memory events (id=0 → no cost_estimates rows, no persist writes), so
// the timing is pure pricing work. "cold" invalidates the catalog cache per
// iteration, approximating the pre-cache behaviour where every event pair
// re-scanned the catalog and re-probed overrides/snapshots.
func BenchmarkResolveEstimates(b *testing.B) {
	for _, n := range benchSizes() {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			openBenchDB(b, filepath.Join(b.TempDir(), "prices-bench.sqlite"))
			mu.Lock()
			for i, m := range benchModels {
				for j, p := range benchProvider {
					if (i+j)%4 == 3 {
						continue // leave a quarter unmatched
					}
					if _, err := db.Exec(`INSERT OR REPLACE INTO price_catalog
						(model,provider,source,input_per_m,output_per_m,cache_read_per_m,synced_at)
						VALUES (?,?,'models.dev',1.5,7.5,0.15,'2026-01-01T00:00:00Z')`, m, p); err != nil {
						mu.Unlock()
						b.Fatal(err)
					}
				}
			}
			// Snapshot history for two pairs so the snap-list cache matters.
			if _, err := db.Exec(`INSERT INTO price_snapshots (model,provider,source,input_per_m,output_per_m,effective_from) VALUES
				('gpt-5','openai','models.dev',1,5,'2026-01-01T00:00:00Z'),
				('claude-sonnet-4-5','anthropic','models.dev',3,15,'2026-01-01T00:00:00Z')`); err != nil {
				mu.Unlock()
				b.Fatal(err)
			}
			mu.Unlock()

			rng := rand.New(rand.NewSource(7))
			now := time.Now()
			evs := make([]eventRow, n)
			for i := range evs {
				evs[i] = eventRow{
					ts:       float64(now.Add(-time.Duration(rng.Intn(30*24)) * time.Hour).Unix()),
					model:    benchModels[i%len(benchModels)],
					provider: benchProvider[i%len(benchProvider)],
					input:    int64(100 + rng.Intn(2000)),
					output:   int64(50 + rng.Intn(800)),
				}
			}
			b.Run("warm", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					infos, _ := resolveEstimates(evs, false)
					if len(infos) != n {
						b.Fatal("resolveEstimates returned wrong length")
					}
				}
			})
			b.Run("cold", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					pricingTablesCache.Store(nil)
					infos, _ := resolveEstimates(evs, false)
					if len(infos) != n {
						b.Fatal("resolveEstimates returned wrong length")
					}
				}
			})
		})
	}
}

// BenchmarkPriceFor measures the per-event entry point used by the usage
// ingest path: warm = cached catalog/overrides (one indexed snapshot probe
// per call); cold = catalog cache invalidated every call (≈ pre-fix state,
// which re-scanned price_catalog and probed overrides per event).
func BenchmarkPriceFor(b *testing.B) {
	openBenchDB(b, filepath.Join(b.TempDir(), "pricefor-bench.sqlite"))
	mu.Lock()
	if _, err := db.Exec(`INSERT INTO price_catalog (model,provider,source,input_per_m,output_per_m,synced_at)
		VALUES ('m1','p1','models.dev',1,10,'2026-01-01T00:00:00Z')`); err != nil {
		mu.Unlock()
		b.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO price_snapshots (model,provider,source,input_per_m,output_per_m,effective_from)
		VALUES ('m1','p1','models.dev',2,20,'2026-06-01T00:00:00Z')`); err != nil {
		mu.Unlock()
		b.Fatal(err)
	}
	mu.Unlock()
	ts := float64(time.Now().Unix())
	b.Run("warm", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			ep, ok := priceFor("p1", "m1", ts, false)
			if !ok || ep.snapshot != "historical" {
				b.Fatalf("priceFor: %+v", ep)
			}
		}
	})
	b.Run("cold", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			pricingTablesCache.Store(nil)
			ep, ok := priceFor("p1", "m1", ts, false)
			if !ok || ep.snapshot != "historical" {
				b.Fatalf("priceFor: %+v", ep)
			}
		}
	})
}

// BenchmarkSnapshotProbePerEvent measures the per-event price_snapshots
// query the pre-cache code ran inside priceFromBasisLocked for every
// event — the cost now amortized to one list query per distinct pair.
func BenchmarkSnapshotProbePerEvent(b *testing.B) {
	openBenchDB(b, filepath.Join(b.TempDir(), "snap-probe.sqlite"))
	mu.Lock()
	if _, err := db.Exec(`INSERT INTO price_snapshots (model,provider,source,input_per_m,output_per_m,effective_from)
		VALUES ('m1','p1','models.dev',2,20,'2026-06-01T00:00:00Z')`); err != nil {
		mu.Unlock()
		b.Fatal(err)
	}
	ts := time.Now().UTC().Format(time.RFC3339)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var sid int64
		var src string
		var ein, eout, ecr, ecw sql.NullFloat64
		err := db.QueryRow(`SELECT id, source, input_per_m, output_per_m, cache_read_per_m, cache_write_per_m
			FROM price_snapshots WHERE provider=? AND model=? AND effective_from<=?
			ORDER BY effective_from DESC LIMIT 1`, "p1", "m1", ts).
			Scan(&sid, &src, &ein, &eout, &ecr, &ecw)
		if err != nil || sid == 0 {
			b.Fatalf("probe: %v", err)
		}
	}
	mu.Unlock()
}

// ============================================================
// Final-gate rework tests. Add-only.
// ============================================================

// Final-gate F1: an event with a stored priced estimate whose CURRENT
// catalog match is unmatched (the catalog row was deleted after the estimate
// was stored) must appear in unmatched_models — mirroring the
// stored-but-ambiguous case — while its stored USD keeps counting and
// pricingStatus reports the pair unmatched too.
func TestUnmatchedModelsStoredEstimate(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "fg1.sqlite"))
	stubFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		return nil, fmt.Errorf("no network")
	})
	mu.Lock()
	seedCatalog(t, [][6]string{
		{"m-gone", "p1", "models.dev", "1", "10", "2026-10-01T00:00:00Z"},
	})
	mu.Unlock()
	seedRecord(t, time.Now().Add(-2*time.Hour), map[string]any{
		"Provider": "p1", "Model": "m-gone",
		"Detail": map[string]int64{"InputTokens": 1000, "OutputTokens": 100},
	})

	resp := getAnalytics(t, "cost", nil)
	want := (1000*1 + 100*10) / 1e6
	if math.Abs(num(resp["totals"].(map[string]any)["priced_usd"])-want) > 1e-12 {
		t.Fatalf("first estimate: %v", resp["totals"])
	}
	if estimateCount(t) != 1 {
		t.Fatal("estimate must be stored")
	}

	// The catalog row disappears after the estimate is stored: the live
	// match is now unmatched while the stored USD stays authoritative.
	mu.Lock()
	if _, err := db.Exec(`DELETE FROM price_catalog WHERE provider='p1' AND model='m-gone'`); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()

	resp = getAnalytics(t, "cost", nil)
	totals := resp["totals"].(map[string]any)
	if math.Abs(num(totals["priced_usd"])-want) > 1e-12 {
		t.Fatalf("stored estimate must keep its USD: %v", totals)
	}
	un := resp["unmatched_models"].([]any)
	if len(un) != 1 || un[0].(map[string]any)["model"] != "m-gone" || un[0].(map[string]any)["provider"] != "p1" {
		t.Fatalf("stored-but-unmatched pair must be listed: %v", un)
	}
	if amb := resp["ambiguous_models"].([]any); len(amb) != 0 {
		t.Fatalf("unmatched stored pair must not be ambiguous: %v", amb)
	}
	// per_model stays priced — the stored USD is authoritative.
	pm := resp["per_model"].([]any)
	if len(pm) != 1 || pm[0].(map[string]any)["price_status"] != "priced" {
		t.Fatalf("per_model price_status: %v", pm)
	}
	// pricingStatus agrees: the pair is unmatched there too.
	_, st := getPricing(t, "status")
	if su, ok := st["unmatched_models"].([]any); !ok || len(su) != 1 {
		t.Fatalf("pricingStatus unmatched_models: %v", st["unmatched_models"])
	}
	mm := st["model_matches"].(map[string]any)
	if e, ok := mm["m-gone"].(map[string]any); !ok || e["status"] != "unmatched" {
		t.Fatalf("model_matches must report unmatched: %v", mm["m-gone"])
	}
}
