package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The whole test binary shares this package: any endpoint that reaches
// maybeFxSync (pricingStatus, analyticsCost, fxStatus) would otherwise kick
// REAL HTTP calls to the FX providers from tests that never stubbed them.
// A disabled default keeps every test hermetic; FX tests override per-test
// via stubFxFetch.
func init() {
	fetchFxSource = func(ctx context.Context, source string) ([]byte, error) {
		return nil, errors.New("fx fetch disabled in tests")
	}
}

// ---- fixtures ----

// erAPIFixture: time_last_update_unix 1760659200 = 2025-10-17T00:00:00Z.
const erAPIFixture = `{
  "result": "success",
  "provider": "https://www.exchangerate-api.com",
  "base_code": "USD",
  "time_last_update_unix": 1760659200,
  "time_next_update_unix": 1760745600,
  "rates": {"EUR": 0.857, "VND": 26300.5, "JPY": 151.2}
}`

const fawazFixture = `{"date": "2025-10-16", "usd": {"vnd": 26310.75, "eur": 0.857}}`

func stubFxFetch(t *testing.T, fn func(ctx context.Context, source string) ([]byte, error)) {
	t.Helper()
	waitFxIdle(t) // a lingering background sync must not read the swapped var
	old := fetchFxSource
	fetchFxSource = fn
	t.Cleanup(func() {
		waitFxIdle(t)
		fetchFxSource = old
	})
}

// waitFxIdle blocks until no FX sync is running or queued. In-flight syncs
// hold fxSyncing; a queued but unconsumed signal sits in fxSyncCh.
func waitFxIdle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !fxSyncing.Load() {
			select {
			case <-fxSyncCh:
				continue // drop queued signal, re-check
			default:
			}
			// The worker may have just consumed a signal but not yet set the
			// flag — give it a beat, then re-verify.
			time.Sleep(10 * time.Millisecond)
			if !fxSyncing.Load() {
				select {
				case <-fxSyncCh:
					continue
				default:
				}
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("fx sync never went idle")
}

func getFx(t *testing.T, name string) (int, map[string]any) {
	t.Helper()
	status, raw := callMgmtBody(t, "/v0/management/plugins/cpa-token-analytics/api/fx/"+name, "GET", nil)
	var out map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return status, out
}

func postFx(t *testing.T, name string, body any) (int, map[string]any) {
	t.Helper()
	status, raw := callMgmtBody(t, "/v0/management/plugins/cpa-token-analytics/api/fx/"+name, "POST", body)
	var out map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return status, out
}

func fxStateRow(t *testing.T) (map[string]any, bool) {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	var rate sql.NullFloat64
	var src, provUpd, fetched, succ, attempt, lastErr sql.NullString
	err := db.QueryRow(`SELECT rate, source, provider_updated_at, fetched_at,
		last_success_at, last_attempt_at, last_error FROM fx_state WHERE pair=?`, fxPair).
		Scan(&rate, &src, &provUpd, &fetched, &succ, &attempt, &lastErr)
	if err != nil {
		return nil, false
	}
	return map[string]any{
		"rate":                rate,
		"source":              src.String,
		"provider_updated_at": provUpd.String,
		"fetched_at":          fetched.String,
		"last_success_at":     succ.String,
		"last_attempt_at":     attempt.String,
		"last_error":          lastErr.String,
	}, true
}

func metaVal(t *testing.T, key string) (string, bool) {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	var v string
	if err := db.QueryRow("SELECT v FROM plugin_meta WHERE k=?", key).Scan(&v); err != nil {
		return "", false
	}
	return v, true
}

func asStr(v any) string {
	s, _ := v.(string)
	return s
}

// waitPricingIdle drains any background pricing sync kicked by status
// readers (maybeAutoSync) so stubFetch cleanup never swaps the var under an
// in-flight read — same discipline as waitFxIdle.
func waitPricingIdle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		syncing := pricingSyncing
		mu.Unlock()
		if !syncing {
			select {
			case <-autoSyncCh:
				continue
			default:
			}
			time.Sleep(10 * time.Millisecond)
			mu.Lock()
			syncing = pricingSyncing
			mu.Unlock()
			if !syncing {
				select {
				case <-autoSyncCh:
					continue
				default:
				}
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("pricing sync never went idle")
}

// ---- sync behaviour ----

func TestFxPrimarySuccess(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "fxp.sqlite"))
	stubFxFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		if source == fxSourceERAPI {
			return []byte(erAPIFixture), nil
		}
		return nil, fmt.Errorf("fallback should not be called")
	})

	code, st := postFx(t, "sync", nil)
	if code != 200 {
		t.Fatalf("sync: %d %v", code, st)
	}
	if num(st["rate"]) != 26300.5 || asStr(st["source"]) != fxSourceERAPI {
		t.Fatalf("status: %v", st)
	}
	if asStr(st["provider_updated_at"]) != "2025-10-17T00:00:00Z" {
		t.Fatalf("provider_updated_at must come from time_last_update_unix: %v", st["provider_updated_at"])
	}
	if st["fetched_at"] == nil || st["last_attempt_at"] == nil {
		t.Fatalf("timestamps missing: %v", st)
	}
	if st["stale"] != false || st["syncing"] != false || st["last_error"] != nil {
		t.Fatalf("flags: %v", st)
	}
	if asStr(st["attribution"]) != fxSourceERAPI {
		t.Fatalf("attribution: %v", st["attribution"])
	}
	if asStr(st["mode"]) != fxModeAuto || asStr(st["pair"]) != fxPair {
		t.Fatalf("mode/pair: %v", st)
	}
	row, ok := fxStateRow(t)
	if !ok || num(row["rate"].(sql.NullFloat64).Float64) != 26300.5 {
		t.Fatalf("fx_state row: %v", row)
	}
	if asStr(row["last_success_at"]) == "" {
		t.Fatal("last_success_at must be recorded")
	}
}

func TestFxFallbackSuccess(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "fxf.sqlite"))
	var calls []string
	stubFxFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		calls = append(calls, source)
		if source == fxSourceERAPI {
			return nil, fmt.Errorf("primary down")
		}
		return []byte(fawazFixture), nil
	})

	code, st := postFx(t, "sync", nil)
	if code != 200 {
		t.Fatalf("sync: %d %v", code, st)
	}
	if num(st["rate"]) != 26310.75 || asStr(st["source"]) != fxSourceFawaz {
		t.Fatalf("fallback status: %v", st)
	}
	if asStr(st["provider_updated_at"]) != "2025-10-16T00:00:00Z" {
		t.Fatalf("date must map to RFC3339: %v", st["provider_updated_at"])
	}
	if len(calls) != 2 || calls[0] != fxSourceERAPI || calls[1] != fxSourceFawaz {
		t.Fatalf("source order: %v", calls)
	}
	if st["last_error"] != nil {
		t.Fatalf("successful fallback must clear last_error: %v", st)
	}
}

func TestFxBothFailKeepsCachedRate(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "fxc.sqlite"))
	stubFxFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		if source == fxSourceERAPI {
			return []byte(erAPIFixture), nil
		}
		return nil, fmt.Errorf("unused")
	})
	if code, _ := postFx(t, "sync", nil); code != 200 {
		t.Fatal("first sync")
	}

	stubFxFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		return nil, fmt.Errorf("%s unreachable", source)
	})
	code, st := postFx(t, "sync", nil)
	if code != 200 {
		t.Fatalf("sync: %d", code)
	}
	// The good cached rate survives a total outage.
	if num(st["rate"]) != 26300.5 || asStr(st["source"]) != fxSourceERAPI {
		t.Fatalf("cached rate must survive: %v", st)
	}
	if st["last_error"] == nil || st["last_attempt_at"] == nil {
		t.Fatalf("failure bookkeeping missing: %v", st)
	}
	// fetched_at still reflects the successful fetch, not the failed attempt.
	row, _ := fxStateRow(t)
	if asStr(row["last_error"]) == "" || asStr(row["last_attempt_at"]) == "" {
		t.Fatalf("row: %v", row)
	}
}

func TestFxBothFailNoCache(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "fxn.sqlite"))
	stubFxFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		return nil, fmt.Errorf("%s down", source)
	})
	code, st := postFx(t, "sync", nil)
	if code != 200 {
		t.Fatalf("sync: %d", code)
	}
	if st["rate"] != nil || st["source"] != nil {
		t.Fatalf("no cache → rate must be null: %v", st)
	}
	if st["last_error"] == nil || st["last_attempt_at"] == nil {
		t.Fatalf("attempt bookkeeping: %v", st)
	}
}

// TestFxRejectsBadPayloads: corrupt payloads never reach fx_state.
func TestFxRejectsBadPayloads(t *testing.T) {
	cases := []struct {
		name    string
		primary string
	}{
		{"missing_vnd", `{"result":"success","base_code":"USD","rates":{"EUR":0.9}}`},
		{"rate_zero", `{"result":"success","base_code":"USD","rates":{"VND":0}}`},
		{"rate_negative", `{"result":"success","base_code":"USD","rates":{"VND":-26300}}`},
		{"rate_below_sanity", `{"result":"success","base_code":"USD","rates":{"VND":0.5}}`},
		{"rate_above_sanity", `{"result":"success","base_code":"USD","rates":{"VND":1.5e9}}`},
		{"wrong_base", `{"result":"success","base_code":"EUR","rates":{"VND":26300}}`},
		{"result_error", `{"result":"error","error-type":"unsupported-code"}`},
		{"malformed", `{not json`},
		{"vnd_null", `{"result":"success","base_code":"USD","rates":{"VND":null}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			openDB(filepath.Join(t.TempDir(), "fxbad.sqlite"))
			stubFxFetch(t, func(ctx context.Context, source string) ([]byte, error) {
				if source == fxSourceERAPI {
					return []byte(tc.primary), nil
				}
				// Fallback fails too → isolation: only the primary payload
				// under test decides the outcome.
				return nil, fmt.Errorf("fallback down")
			})
			code, st := postFx(t, "sync", nil)
			if code != 200 {
				t.Fatalf("sync: %d", code)
			}
			if st["rate"] != nil {
				t.Fatalf("bad payload must be rejected: %v", st)
			}
			if st["last_error"] == nil {
				t.Fatal("rejection must surface in last_error")
			}
		})
	}
	// Fallback-side validation: usd.vnd garbage is rejected too.
	t.Run("fallback_bad_vnd", func(t *testing.T) {
		openDB(filepath.Join(t.TempDir(), "fxbadfb.sqlite"))
		stubFxFetch(t, func(ctx context.Context, source string) ([]byte, error) {
			if source == fxSourceERAPI {
				return nil, fmt.Errorf("primary down")
			}
			return []byte(`{"date":"2025-10-16","usd":{"vnd":10}}`), nil
		})
		_, st := postFx(t, "sync", nil)
		if st["rate"] != nil || st["last_error"] == nil {
			t.Fatalf("fallback out-of-range must reject: %v", st)
		}
	})
}

// ---- auto-sync trigger ----

func TestFxMaybeSyncInterval(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "fxi.sqlite"))
	var calls int64
	stubFxFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		atomic.AddInt64(&calls, 1)
		if source == fxSourceERAPI {
			return []byte(erAPIFixture), nil
		}
		return nil, fmt.Errorf("unused")
	})

	// No rate yet → first maybeFxSync kicks a background fetch.
	maybeFxSync()
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt64(&calls) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if atomic.LoadInt64(&calls) == 0 {
		t.Fatal("empty fx_state must trigger a background sync")
	}
	waitFxIdle(t)

	// Fresh fetched_at → no new fetch.
	maybeFxSync()
	if atomic.LoadInt64(&calls) != 1 {
		t.Fatalf("fresh rate must not refetch: calls=%d", calls)
	}

	// Stale fetched_at (>24h) → refetch.
	mu.Lock()
	_, err := db.Exec("UPDATE fx_state SET fetched_at=? WHERE pair=?",
		time.Now().Add(-48*time.Hour).UTC().Format(time.RFC3339), fxPair)
	mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	maybeFxSync()
	for atomic.LoadInt64(&calls) == 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if atomic.LoadInt64(&calls) != 2 {
		t.Fatalf("stale rate must refetch: calls=%d", calls)
	}
	waitFxIdle(t)

	// Manual mode → never auto-fetch even when stale.
	mu.Lock()
	_, err = db.Exec("UPDATE fx_state SET fetched_at=? WHERE pair=?",
		time.Now().Add(-48*time.Hour).UTC().Format(time.RFC3339), fxPair)
	metaSet("fx_mode", fxModeManual)
	mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	maybeFxSync()
	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt64(&calls) != 2 {
		t.Fatalf("manual mode must not auto-fetch: calls=%d", calls)
	}
}

// TestFxConcurrentSync: parallel syncFx calls single-flight — exactly one
// fetch runs; others return false immediately.
func TestFxConcurrentSync(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "fxcc.sqlite"))
	var calls int64
	release := make(chan struct{})
	stubFxFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		atomic.AddInt64(&calls, 1)
		<-release // hold the in-flight sync so contenders overlap it
		return []byte(erAPIFixture), nil
	})

	var wg sync.WaitGroup
	ran := make([]bool, 8)
	for i := range ran {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ran[i] = syncFx(context.Background())
		}(i)
	}
	// Wait until the single winner is inside the fetch, then let it finish.
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt64(&calls) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // let all contenders observe the flag
	close(release)
	wg.Wait()
	waitFxIdle(t)

	if atomic.LoadInt64(&calls) != 1 {
		t.Fatalf("concurrent syncs must single-flight: calls=%d", calls)
	}
	trueN := 0
	for _, r := range ran {
		if r {
			trueN++
		}
	}
	if trueN != 1 {
		t.Fatalf("exactly one sync may run: %v", ran)
	}

	// POST /api/fx/sync while a sync is in flight → status with syncing=true,
	// no stacked fetch.
	release2 := make(chan struct{})
	stubFxFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		atomic.AddInt64(&calls, 1)
		<-release2
		return []byte(erAPIFixture), nil
	})
	go syncFx(context.Background())
	for atomic.LoadInt64(&calls) == 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	code, st := postFx(t, "sync", nil)
	if code != 200 || st["syncing"] != true {
		t.Fatalf("in-flight sync must report syncing=true: code=%d %v", code, st)
	}
	if atomic.LoadInt64(&calls) != 2 {
		t.Fatalf("endpoint must not stack a second fetch: calls=%d", calls)
	}
	close(release2)
	waitFxIdle(t)
}

// ---- settings + mode transitions ----

func TestFxSettingsModeTransitions(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "fxm.sqlite"))
	stubFxFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		if source == fxSourceERAPI {
			return []byte(erAPIFixture), nil
		}
		return nil, fmt.Errorf("unused")
	})

	// manual mode + manual_rate
	code, res := postFx(t, "settings", map[string]any{"mode": "manual", "manual_rate": 25500})
	if code != 200 || res["ok"] != true {
		t.Fatalf("settings: %d %v", code, res)
	}
	if v, ok := metaVal(t, "fx_mode"); !ok || v != "manual" {
		t.Fatalf("fx_mode: %v %v", v, ok)
	}
	if v, ok := metaVal(t, "fx_usd_vnd"); !ok || v != "25500" {
		t.Fatalf("fx_usd_vnd: %v %v", v, ok)
	}
	code, st := getFx(t, "status")
	if code != 200 || num(st["rate"]) != 25500 || asStr(st["source"]) != fxSourceManual || num(st["manual_rate"]) != 25500 {
		t.Fatalf("manual status: %v", st)
	}
	if st["stale"] != false {
		t.Fatalf("explicit manual rate is never stale: %v", st)
	}

	// invalid manual_rate → 400, nothing written
	if code, _ := postFx(t, "settings", map[string]any{"manual_rate": -3}); code != 400 {
		t.Fatalf("negative manual_rate must 400: %d", code)
	}
	if code, _ := postFx(t, "settings", map[string]any{"manual_rate": "abc"}); code != 400 {
		t.Fatalf("string manual_rate must 400: %d", code)
	}
	if code, _ := postFx(t, "settings", map[string]any{"mode": "weekly"}); code != 400 {
		t.Fatalf("bad mode must 400: %d", code)
	}

	// manual_rate:null clears manual keys but keeps mode
	code, res = postFx(t, "settings", map[string]any{"manual_rate": nil})
	if code != 200 || res["ok"] != true {
		t.Fatalf("clear: %d %v", code, res)
	}
	if _, ok := metaVal(t, "fx_usd_vnd"); ok {
		t.Fatal("manual_rate:null must delete fx_usd_vnd")
	}
	if v, _ := metaVal(t, "fx_mode"); v != "manual" {
		t.Fatalf("mode must survive rate clear: %v", v)
	}
	_, st = getFx(t, "status")
	if st["rate"] != nil {
		t.Fatalf("manual mode without rate → unavailable: %v", st)
	}

	// manual → auto kicks a fetch (rate absent) and the fetched rate wins.
	code, res = postFx(t, "settings", map[string]any{"mode": "auto"})
	if code != 200 || res["ok"] != true {
		t.Fatalf("auto: %d %v", code, res)
	}
	waitFxIdle(t)
	_, st = getFx(t, "status")
	if num(st["rate"]) != 26300.5 || asStr(st["source"]) != fxSourceERAPI {
		t.Fatalf("auto rate must come from fetch: %v", st)
	}
}

// TestFxAutoFallbackToManual: auto mode without a fetched rate falls back to
// the manual rate — reported as source=manual, stale=true.
func TestFxAutoFallbackToManual(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "fxfb.sqlite"))
	stubFxFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		return nil, fmt.Errorf("%s down", source) // fetch never lands
	})
	code, res := postFx(t, "settings", map[string]any{"manual_rate": 24000})
	if code != 200 || res["ok"] != true {
		t.Fatalf("settings: %d", code)
	}
	waitFxIdle(t) // drain the kicked auto sync attempt
	_, st := getFx(t, "status")
	if num(st["rate"]) != 24000 || asStr(st["source"]) != fxSourceManual || st["stale"] != true {
		t.Fatalf("auto+manual fallback: %v", st)
	}
}

// ---- legacy pricing/settings compatibility ----

func TestFxLegacySettingsSemantics(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "fxl.sqlite"))
	stubFxFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		return nil, fmt.Errorf("%s down", source)
	})
	stubFetch(t, fixtureFetch(modelsDevFixture)) // keep pricing side hermetic too

	// Legacy POST pricing/settings {fx_usd_vnd} sets the manual rate AND
	// flips fx_mode to manual.
	postPricing(t, "settings", map[string]any{"fx_usd_vnd": 25100})
	if v, _ := metaVal(t, "fx_mode"); v != "manual" {
		t.Fatalf("legacy fx save must imply manual mode: %v", v)
	}
	_, st := getFx(t, "status")
	if num(st["rate"]) != 25100 || asStr(st["mode"]) != "manual" || num(st["manual_rate"]) != 25100 {
		t.Fatalf("status after legacy save: %v", st)
	}
	// pricingStatus.fx keeps the legacy keys and gains source/mode/stale.
	_, pst := getPricing(t, "status")
	fx, _ := pst["fx"].(map[string]any)
	if num(fx["rate"]) != 25100 || fx["updated_at"] == nil ||
		asStr(fx["source"]) != fxSourceManual || asStr(fx["mode"]) != "manual" {
		t.Fatalf("pricingStatus.fx: %v", pst["fx"])
	}

	// Legacy null clears the manual rate but keeps the mode.
	postPricing(t, "settings", map[string]any{"fx_usd_vnd": nil})
	if _, ok := metaVal(t, "fx_usd_vnd"); ok {
		t.Fatal("legacy null must delete fx_usd_vnd")
	}
	if v, _ := metaVal(t, "fx_mode"); v != "manual" {
		t.Fatalf("mode must survive legacy null: %v", v)
	}
	_, pst = getPricing(t, "status")
	if pst["fx"] != nil {
		t.Fatalf("fx must be null after legacy clear: %v", pst["fx"])
	}
	waitPricingIdle(t) // let the kicked pricing sync land before stubFetch restores
}

// TestFxInfoEffectiveRate: pricingStatus.fx follows mode resolution.
func TestFxInfoEffectiveRate(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "fxe.sqlite"))
	stubFetch(t, fixtureFetch(modelsDevFixture))
	stubFxFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		if source == fxSourceERAPI {
			return []byte(erAPIFixture), nil
		}
		return nil, fmt.Errorf("unused")
	})

	// auto + fetched rate → external source, fresh.
	if code, _ := postFx(t, "sync", nil); code != 200 {
		t.Fatal("sync")
	}
	_, pst := getPricing(t, "status")
	fx, _ := pst["fx"].(map[string]any)
	if num(fx["rate"]) != 26300.5 || asStr(fx["source"]) != fxSourceERAPI ||
		asStr(fx["mode"]) != "auto" || fx["stale"] != false ||
		asStr(fx["updated_at"]) != "2025-10-17T00:00:00Z" {
		t.Fatalf("auto fx: %v", fx)
	}

	// manual mode → manual rate wins even while a fetched rate is cached.
	postFx(t, "settings", map[string]any{"mode": "manual", "manual_rate": 24000})
	_, pst = getPricing(t, "status")
	fx, _ = pst["fx"].(map[string]any)
	if num(fx["rate"]) != 24000 || asStr(fx["source"]) != fxSourceManual {
		t.Fatalf("manual fx: %v", fx)
	}

	// manual mode without manual rate → fx null.
	postFx(t, "settings", map[string]any{"manual_rate": nil})
	_, pst = getPricing(t, "status")
	if pst["fx"] != nil {
		t.Fatalf("manual without rate must be null: %v", pst["fx"])
	}
	waitPricingIdle(t)
}

// ---- migration ----

// TestFxMigrationExistingManual: a DB with a valid legacy fx_usd_vnd and no
// fx_mode is migrated to manual mode, keeping the rate.
func TestFxMigrationExistingManual(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fxmig.sqlite")
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec("CREATE TABLE usage_events (id INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL, dedup TEXT UNIQUE, payload TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec("CREATE TABLE plugin_meta (k TEXT PRIMARY KEY, v TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec("INSERT INTO plugin_meta (k,v) VALUES ('fx_usd_vnd','24900'),('fx_usd_vnd_updated_at','2025-10-01T00:00:00Z')"); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()

	openDB(path)
	if v, ok := metaVal(t, "fx_mode"); !ok || v != "manual" {
		t.Fatalf("existing manual rate must migrate to manual mode: %v %v", v, ok)
	}
	waitFxIdle(t) // any queued kick resolves against this DB now
	_, st := getFx(t, "status")
	if num(st["rate"]) != 24900 || asStr(st["mode"]) != "manual" {
		t.Fatalf("migrated status: %v", st)
	}
	// The manual rate must NOT be synthesized into fx_state by migration.
	mu.Lock()
	var n int
	err = db.QueryRow("SELECT COUNT(*) FROM fx_state WHERE pair=? AND rate IS NOT NULL", fxPair).Scan(&n)
	mu.Unlock()
	if err != nil {
		t.Fatalf("fx_state query: %v", err)
	}
	if n != 0 {
		t.Fatal("migration must not copy the manual rate into fx_state")
	}
}

// TestFxMigrationFresh: no fx_usd_vnd → auto default, fx_mode stays absent.
func TestFxMigrationFresh(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "fxfresh.sqlite"))
	stubFxFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		return nil, fmt.Errorf("disabled")
	})
	if _, ok := metaVal(t, "fx_mode"); ok {
		t.Fatal("fresh install must not pin fx_mode")
	}
	waitFxIdle(t)
	code, st := getFx(t, "status")
	if code != 200 || asStr(st["mode"]) != "auto" {
		t.Fatalf("fresh install must be auto: %v", st)
	}
	// fx_state table exists.
	mu.Lock()
	var n int
	err := db.QueryRow("SELECT COUNT(*) FROM fx_state").Scan(&n)
	mu.Unlock()
	if err != nil {
		t.Fatalf("fx_state missing: %v", err)
	}
}

// ---- wire format ----

// TestFxWireBase64Body: the host delivers request bodies as base64 strings
// (ManagementRequest.Body []byte marshals to JSON string). Both POST
// endpoints must decode through normalizeBody.
func TestFxWireBase64Body(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "fxw.sqlite"))
	stubFxFetch(t, func(ctx context.Context, source string) ([]byte, error) {
		if source == fxSourceERAPI {
			return []byte(erAPIFixture), nil
		}
		return nil, fmt.Errorf("unused")
	})

	wire := func(name, method string, rawBody []byte) (int, map[string]any) {
		t.Helper()
		var body any
		if rawBody != nil {
			body = base64.StdEncoding.EncodeToString(rawBody)
		}
		req, _ := json.Marshal(map[string]any{
			"result": map[string]any{
				"Method":  method,
				"Path":    "/v0/management/plugins/cpa-token-analytics/api/fx/" + name,
				"Query":   map[string][]string{},
				"Headers": map[string][]string{},
				"Body":    body,
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

	code, res := wire("settings", "POST", []byte(`{"mode":"manual","manual_rate":25250}`))
	if code != 200 || res["ok"] != true {
		t.Fatalf("settings over wire: code=%d res=%v", code, res)
	}
	if v, _ := metaVal(t, "fx_usd_vnd"); v != "25250" {
		t.Fatalf("wire settings did not apply: %v", v)
	}

	code, res = wire("sync", "POST", nil)
	if code != 200 {
		t.Fatalf("sync over wire: code=%d res=%v", code, res)
	}
	if asStr(res["mode"]) != "manual" || num(res["rate"]) != 25250 {
		t.Fatalf("sync status over wire: %v", res)
	}

	if code, _ = wire("settings", "POST", []byte(`{"manual_rate":`)); code != 400 {
		t.Fatalf("malformed body must 400, got %d", code)
	}

	// Unknown fx route → 404, GET on POST routes → 404.
	if code, _ := wire("bogus", "GET", nil); code != 404 {
		t.Fatalf("unknown fx route must 404, got %d", code)
	}
	if code, _ := wire("sync", "GET", nil); code != 404 {
		t.Fatalf("GET sync must 404, got %d", code)
	}
}
