package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---- automatic USD/VND exchange rate ----
//
// Sources (priority order):
//   - ExchangeRate-API: {"result":"success","base_code":"USD",
//     "rates":{"VND":N},"time_last_update_unix":T}
//   - Fawaz currency-api (jsDelivr CDN): {"date":"YYYY-MM-DD","usd":{"vnd":N}}
//
// Fetched rates live in fx_state (one row per pair). plugin_meta['fx_mode']
// selects "auto" (default) vs "manual" (legacy fx_usd_vnd keys). A failed
// sync never overwrites a good cached rate — only attempt bookkeeping is
// recorded.
const (
	fxPair         = "USD/VND"
	fxSourceERAPI  = "ExchangeRate-API"
	fxSourceFawaz  = "Fawaz Currency API"
	fxSourceManual = "manual"

	fxModeAuto   = "auto"
	fxModeManual = "manual"

	fxSyncInterval = 24 * time.Hour
	fxMaxBody      = 1 << 20 // 1 MiB — both payloads are a few KB
	fxSyncTimeout  = 30 * time.Second

	// Sanity band for fetched USD/VND rates: the pair has traded between
	// ~16k and ~27k for two decades. A wide 1k–1M window rejects corrupt
	// payloads (wrong base currency, inverted pairs, unit errors) without
	// pretending to know the market.
	fxMinRate = 1_000.0
	fxMaxRate = 1_000_000.0
)

var fxURLs = map[string]string{
	fxSourceERAPI: "https://open.er-api.com/v6/latest/USD",
	fxSourceFawaz: "https://cdn.jsdelivr.net/npm/@fawazahmed0/currency-api@latest/v1/currencies/usd.json",
}

// fxSourceOrder is the fetch priority: primary first, fallback second.
var fxSourceOrder = []string{fxSourceERAPI, fxSourceFawaz}

var fxHTTPClient = &http.Client{Timeout: 10 * time.Second}

// fetchFxSource returns the raw JSON document for one FX source. Tests stub
// it (same injection pattern as fetchPricingSource) — production code
// performs the guarded HTTP GET with no auth headers.
var fetchFxSource = func(ctx context.Context, source string) ([]byte, error) {
	url := fxURLs[source]
	if url == "" {
		return nil, fmt.Errorf("unknown fx source %q", source)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := fxHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: http %d", source, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, fxMaxBody))
}

// validFetchedRate bounds fetched rates to the sanity band — see fxMinRate.
func validFetchedRate(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0) && f >= fxMinRate && f <= fxMaxRate
}

// parseFxERAPI validates the ExchangeRate-API payload: result=="success",
// base_code=="USD" and rates.VND finite inside the sanity band. The provider
// timestamp comes from time_last_update_unix (not the fetch time).
func parseFxERAPI(data []byte) (rate float64, providerUpdatedAt string, err error) {
	var doc struct {
		Result   string             `json:"result"`
		BaseCode string             `json:"base_code"`
		Rates    map[string]float64 `json:"rates"`
		LastUpd  int64              `json:"time_last_update_unix"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return 0, "", fmt.Errorf("parse: %v", err)
	}
	if doc.Result != "success" {
		return 0, "", fmt.Errorf("result=%q", doc.Result)
	}
	if doc.BaseCode != "USD" {
		return 0, "", fmt.Errorf("base_code=%q", doc.BaseCode)
	}
	r, ok := doc.Rates["VND"]
	if !ok {
		return 0, "", fmt.Errorf("rates.VND missing")
	}
	if !validFetchedRate(r) {
		return 0, "", fmt.Errorf("rates.VND=%v out of range", r)
	}
	if doc.LastUpd > 0 {
		providerUpdatedAt = time.Unix(doc.LastUpd, 0).UTC().Format(time.RFC3339)
	}
	return r, providerUpdatedAt, nil
}

// parseFxFawaz validates the Fawaz currency-api payload: usd.vnd finite and
// inside the sanity band. The provider timestamp is the YYYY-MM-DD date.
func parseFxFawaz(data []byte) (rate float64, providerUpdatedAt string, err error) {
	var doc struct {
		Date string             `json:"date"`
		USD  map[string]float64 `json:"usd"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return 0, "", fmt.Errorf("parse: %v", err)
	}
	r, ok := doc.USD["vnd"]
	if !ok {
		return 0, "", fmt.Errorf("usd.vnd missing")
	}
	if !validFetchedRate(r) {
		return 0, "", fmt.Errorf("usd.vnd=%v out of range", r)
	}
	if d, err := time.Parse("2006-01-02", doc.Date); err == nil {
		providerUpdatedAt = d.UTC().Format(time.RFC3339)
	}
	return r, providerUpdatedAt, nil
}

// ---- state ----

// fxState is the fx_state row for one pair; hasRate separates "rate zero"
// from "no rate ever fetched" (or a corrupted stored rate).
type fxState struct {
	rate              float64
	hasRate           bool
	source            string
	providerUpdatedAt string
	fetchedAt         string
	lastSuccessAt     string
	lastAttemptAt     string
	lastError         string
}

// loadFxStateLocked reads the USD/VND row. Callers hold mu.
func loadFxStateLocked() fxState {
	var st fxState
	if db == nil {
		return st
	}
	var rate sql.NullFloat64
	var src, provUpd, fetched, succ, attempt, lastErr sql.NullString
	err := db.QueryRow(`SELECT rate, source, provider_updated_at, fetched_at,
		last_success_at, last_attempt_at, last_error FROM fx_state WHERE pair=?`, fxPair).
		Scan(&rate, &src, &provUpd, &fetched, &succ, &attempt, &lastErr)
	if err != nil {
		return st
	}
	st.rate = rate.Float64
	st.hasRate = rate.Valid && validFetchedRate(rate.Float64)
	st.source = src.String
	st.providerUpdatedAt = provUpd.String
	st.fetchedAt = fetched.String
	st.lastSuccessAt = succ.String
	st.lastAttemptAt = attempt.String
	st.lastError = lastErr.String
	return st
}

// fxModeLocked reads plugin_meta['fx_mode']; absent → "auto". Callers hold mu.
func fxModeLocked() string {
	if v := metaGet("fx_mode"); v != nil && *v == fxModeManual {
		return fxModeManual
	}
	return fxModeAuto
}

// manualFxRateLocked reads the legacy manual rate (plugin_meta fx_usd_vnd +
// fx_usd_vnd_updated_at). Manual rates keep the legacy validation — finite
// and >0, no sanity band: they are the operator's explicit choice. Callers
// hold mu.
func manualFxRateLocked() (rate float64, updatedAt string, ok bool) {
	v := metaGet("fx_usd_vnd")
	if v == nil || *v == "" {
		return 0, "", false
	}
	f, err := strconvParseFloat(*v)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
		return 0, "", false
	}
	if u := metaGet("fx_usd_vnd_updated_at"); u != nil {
		updatedAt = *u
	}
	return f, updatedAt, true
}

// fxStaleLocked reports whether an auto-mode cached rate is stale: fetched_at
// missing/unparseable or older than fxSyncInterval.
func fxStale(st fxState) bool {
	if t, err := time.Parse(time.RFC3339, st.fetchedAt); err == nil {
		return time.Since(t) > fxSyncInterval
	}
	return true
}

// seedFxMode migrates pre-FX installs: a valid fx_usd_vnd without an fx_mode
// decision means the operator set a manual rate before auto-fetch existed →
// keep it manual. Fresh installs leave fx_mode absent → "auto". Runs inside
// runMigrations on the opening connection (idempotent).
func seedFxMode(conn *sql.DB) error {
	var mode string
	if err := conn.QueryRow("SELECT v FROM plugin_meta WHERE k='fx_mode'").Scan(&mode); err == nil && mode != "" {
		return nil // already decided
	}
	var v string
	if err := conn.QueryRow("SELECT v FROM plugin_meta WHERE k='fx_usd_vnd'").Scan(&v); err != nil {
		return nil // no manual rate → auto default
	}
	var f float64
	if err := json.Unmarshal([]byte(v), &f); err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
		return nil // junk manual value → auto default
	}
	_, err := conn.Exec("INSERT OR IGNORE INTO plugin_meta (k,v) VALUES ('fx_mode','manual')")
	return err
}

// ---- sync ----

var (
	fxSyncOnce sync.Once
	fxSyncCh   = make(chan struct{}, 1)
	fxSyncing  atomic.Bool
)

// syncFx fetches sources in priority order and stores the first valid rate.
// last_attempt_at is written on every run; fetched_at/last_success_at only
// on success. A total failure keeps the cached rate untouched. Returns true
// when this call actually ran the sync (false = one was already in flight).
func syncFx(ctx context.Context) bool {
	if !fxSyncing.CompareAndSwap(false, true) {
		return false
	}
	defer fxSyncing.Store(false)
	ensureDB()
	now := time.Now().UTC().Format(time.RFC3339)
	var lastErr error
	for _, src := range fxSourceOrder {
		data, ferr := fetchFxSource(ctx, src)
		if ferr != nil {
			lastErr = fmt.Errorf("%s: %v", src, ferr)
			continue
		}
		var rate float64
		var providerTs string
		var perr error
		switch src {
		case fxSourceERAPI:
			rate, providerTs, perr = parseFxERAPI(data)
		default:
			rate, providerTs, perr = parseFxFawaz(data)
		}
		if perr != nil {
			lastErr = fmt.Errorf("%s: %v", src, perr)
			continue
		}
		var pany any
		if providerTs != "" {
			pany = providerTs
		}
		mu.Lock()
		if db != nil {
			_, _ = db.Exec("INSERT OR IGNORE INTO fx_state (pair) VALUES (?)", fxPair)
			_, _ = db.Exec(`UPDATE fx_state SET rate=?, source=?, provider_updated_at=?,
				fetched_at=?, last_success_at=?, last_attempt_at=?, last_error=NULL WHERE pair=?`,
				rate, src, pany, now, now, now, fxPair)
		}
		mu.Unlock()
		return true
	}
	errStr := "no fx source configured"
	if lastErr != nil {
		errStr = lastErr.Error()
	}
	mu.Lock()
	if db != nil {
		_, _ = db.Exec("INSERT OR IGNORE INTO fx_state (pair) VALUES (?)", fxPair)
		_, _ = db.Exec("UPDATE fx_state SET last_attempt_at=?, last_error=? WHERE pair=?",
			now, errStr, fxPair)
	}
	mu.Unlock()
	return true
}

// maybeFxSync mirrors maybeAutoSync: a lazy staleness check that hands work
// to a background worker — status readers never block on the fetch. Fires
// only in auto mode when there is no cached rate or it is older than
// fxSyncInterval. Never called from the usage.handle path.
func maybeFxSync() {
	fxSyncOnce.Do(func() {
		go func() {
			for range fxSyncCh {
				syncFx(context.Background())
			}
		}()
	})
	ensureDB()
	mu.Lock()
	auto := fxModeLocked() == fxModeAuto
	var st fxState
	if db != nil {
		st = loadFxStateLocked()
	}
	mu.Unlock()
	if !auto {
		return
	}
	if st.hasRate && !fxStale(st) {
		return
	}
	if fxSyncing.Load() {
		return // a sync is already in flight — let it land first
	}
	select {
	case fxSyncCh <- struct{}{}:
	default:
	}
}

// fxInfo is the embedded "fx" object inside pricingStatus and analyticsCost
// responses. It resolves the effective rate for the current mode — auto
// falls back to a valid manual rate when nothing has been fetched yet.
// Keys rate + updated_at keep the legacy shape; source/mode/stale are
// additive. Unavailable → nil ("fx": null).
func fxInfo() any {
	mu.Lock()
	defer mu.Unlock()
	if db == nil {
		return nil
	}
	mode := fxModeLocked()
	mRate, mUpdated, mOk := manualFxRateLocked()
	if mode == fxModeManual {
		if !mOk {
			return nil
		}
		return map[string]any{"rate": mRate, "updated_at": nilIfEmpty(mUpdated),
			"source": fxSourceManual, "mode": mode, "stale": false}
	}
	st := loadFxStateLocked()
	if st.hasRate {
		var updatedAt any
		if st.providerUpdatedAt != "" {
			updatedAt = st.providerUpdatedAt
		}
		return map[string]any{"rate": st.rate, "updated_at": updatedAt,
			"source": st.source, "mode": mode, "stale": fxStale(st)}
	}
	if mOk {
		return map[string]any{"rate": mRate, "updated_at": nilIfEmpty(mUpdated),
			"source": fxSourceManual, "mode": mode, "stale": true}
	}
	return nil
}

// ---- status ----

// fxStatus builds the GET /api/fx/status payload. Readers always get the
// full shape; absent values serialize as null.
func fxStatus() ([]byte, error) {
	ensureDB()
	maybeFxSync()
	mu.Lock()
	defer mu.Unlock()
	mode := fxModeLocked()
	var st fxState
	if db != nil {
		st = loadFxStateLocked()
	}
	mRate, _, mOk := manualFxRateLocked()
	resp := map[string]any{
		"pair":                fxPair,
		"mode":                mode,
		"rate":                nil,
		"source":              nil,
		"provider_updated_at": nil,
		"fetched_at":          nil,
		"last_attempt_at":     nil,
		"stale":               false,
		"syncing":             fxSyncing.Load(),
		"last_error":          nil,
		"manual_rate":         nil,
		"attribution":         nil,
	}
	if mOk {
		resp["manual_rate"] = mRate
	}
	if st.lastAttemptAt != "" {
		resp["last_attempt_at"] = st.lastAttemptAt
	}
	if st.lastError != "" {
		resp["last_error"] = st.lastError
	}
	if mode == fxModeManual {
		if mOk {
			resp["rate"] = mRate
			resp["source"] = fxSourceManual
		}
	} else if st.hasRate {
		resp["rate"] = st.rate
		resp["source"] = st.source
		resp["attribution"] = st.source
		resp["stale"] = fxStale(st)
		if st.providerUpdatedAt != "" {
			resp["provider_updated_at"] = st.providerUpdatedAt
		}
		if st.fetchedAt != "" {
			resp["fetched_at"] = st.fetchedAt
		}
	} else if mOk {
		// Auto mode with no fetched rate yet: the explicit manual rate is
		// the fallback and marks the pair stale until a fetch lands.
		resp["rate"] = mRate
		resp["source"] = fxSourceManual
		resp["stale"] = true
	}
	return json.Marshal(resp)
}

// ---- endpoints ----

// handleFx routes /api/fx/* requests; mirrors handlePricing.
func handleFx(path, method string, body any) ([]byte, error) {
	i := strings.Index(path, "/api/fx/")
	if i < 0 {
		return managementResponse(404, "text/plain; charset=utf-8", []byte("not found"))
	}
	name := strings.TrimSuffix(strings.SplitN(path[i+len("/api/fx/"):], "?", 2)[0], "/")
	var out []byte
	var err error
	switch {
	case name == "status" && method == http.MethodGet:
		out, err = fxStatus()
	case name == "sync" && method == http.MethodPost:
		// Synchronous sync, bounded. If one is already running the call is a
		// no-op and the status below reports syncing=true — no stacking.
		ctx, cancel := context.WithTimeout(context.Background(), fxSyncTimeout)
		syncFx(ctx)
		cancel()
		out, err = fxStatus()
	case name == "settings" && method == http.MethodPost:
		out, err = fxSettingsEndpoint(body)
	default:
		return managementResponse(404, "text/plain; charset=utf-8", []byte("not found"))
	}
	if err != nil {
		b, _ := json.Marshal(map[string]any{"error": err.Error()})
		return managementResponse(400, "application/json; charset=utf-8", b)
	}
	return managementResponse(200, "application/json; charset=utf-8", out)
}

// fxSettingsEndpoint implements POST /api/fx/settings: {mode?, manual_rate?}.
// manual_rate:null deletes the manual rate; a positive finite number stores
// it (stamping fx_usd_vnd_updated_at). Switching to auto kicks a sync when
// the cache is missing or stale.
func fxSettingsEndpoint(body any) ([]byte, error) {
	raw, err := normalizeBody(body)
	if err != nil {
		return nil, fmt.Errorf("invalid body")
	}
	var req struct {
		Mode       string          `json:"mode"`
		ManualRate json.RawMessage `json:"manual_rate"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, fmt.Errorf("invalid body: %v", err)
		}
	}
	req.Mode = strings.TrimSpace(req.Mode)
	if req.Mode != "" && req.Mode != fxModeAuto && req.Mode != fxModeManual {
		return nil, fmt.Errorf("mode must be \"auto\" or \"manual\"")
	}
	// Validate manual_rate before any write so a bad field aborts the
	// request — same semantics as the pricing override endpoint.
	var rate float64
	manualSet := false
	if req.ManualRate != nil {
		s := strings.TrimSpace(string(req.ManualRate))
		if s != "null" && s != "" {
			if err := json.Unmarshal(req.ManualRate, &rate); err != nil ||
				math.IsNaN(rate) || math.IsInf(rate, 0) || rate <= 0 {
				return nil, fmt.Errorf("manual_rate must be a positive number or null")
			}
			manualSet = true
		}
	}
	ensureDB()
	mu.Lock()
	if db == nil {
		mu.Unlock()
		return nil, fmt.Errorf("db unavailable")
	}
	if req.ManualRate != nil { // key present (incl. explicit null → delete)
		if manualSet {
			metaSet("fx_usd_vnd", fmt.Sprintf("%g", rate))
			metaSet("fx_usd_vnd_updated_at", time.Now().UTC().Format(time.RFC3339))
		} else {
			metaDel("fx_usd_vnd")
			metaDel("fx_usd_vnd_updated_at")
		}
	}
	if req.Mode != "" {
		metaSet("fx_mode", req.Mode)
	}
	mu.Unlock()
	if req.Mode == fxModeAuto {
		maybeFxSync() // refresh now if the cache is stale or empty
	}
	return json.Marshal(map[string]any{"ok": true})
}
