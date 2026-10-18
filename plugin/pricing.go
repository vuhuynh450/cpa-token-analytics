package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---- BE2 pricing engine ----
//
// External price sources (contract B):
//   - models.dev api.json: {provider: {models: {id: {cost:{input,output,
//     cache_read?,cache_write?}, canonical_model_id?}}}} — USD per 1M tokens.
//   - litellm fallback: {model_name: {input_cost_per_token, output_cost_per_token,
//     cache_read_input_token_cost, cache_creation_input_token_cost,
//     litellm_provider, ...}} — USD per token (×1e6). `sample_spec` is skipped;
//     vendor-prefixed keys keep their `vendor/` row AND the canonical row under
//     litellm_provider.
//
// All internal prices are USD per 1M tokens. Nullable components (cache_write
// is sparse) are stored as NULL so "missing price" never reads as zero.

const (
	pricingSourceModelsDev = "models.dev"
	pricingSourceLiteLLM   = "litellm"
	pricingSourceOverride  = "override"
	pricingMaxBody         = 32 << 20 // 32 MiB
	autoSyncInterval       = 24 * time.Hour
)

var pricingURLs = map[string]string{
	pricingSourceModelsDev: "https://models.dev/api.json",
	pricingSourceLiteLLM:   "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json",
}

var pricingHTTPClient = &http.Client{Timeout: 20 * time.Second}

// fetchPricingSource returns the raw JSON document for one source. Tests
// stub it — production code performs the guarded HTTP GET.
var fetchPricingSource = func(ctx context.Context, source string) ([]byte, error) {
	url := pricingURLs[source]
	if url == "" {
		return nil, fmt.Errorf("unknown pricing source %q", source)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := pricingHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: http %d", source, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, pricingMaxBody))
}

// priceRow is one catalog entry. nil means "no price for this component"
// (never conflated with 0 — a missing component price makes events partial).
// src carries the owning price source when the row came from the DB.
type priceRow struct {
	in  *float64
	out *float64
	cr  *float64
	cw  *float64
	src string
}

func (r priceRow) mapPrices() map[string]any {
	f := func(p *float64) any {
		if p == nil {
			return nil
		}
		return *p
	}
	return map[string]any{"input": f(r.in), "output": f(r.out), "cache_read": f(r.cr), "cache_write": f(r.cw)}
}

// ---- parsing ----

// priceField validates one raw price component per contract B2: absent/null →
// nil (sparse); non-numeric or negative/NaN/Inf → invalid (entry rejected).
// Null is "field absent", never "price zero".
func priceField(raw json.RawMessage) (v *float64, present, invalid bool) {
	if raw == nil {
		return nil, false, false
	}
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil, true, false
	}
	f, err := strconvParseFloat(s)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return nil, true, true
	}
	return &f, true, false
}

// strconvParseFloat accepts only JSON numbers (no quoted strings, bools, etc).
func strconvParseFloat(s string) (float64, error) {
	var f float64
	if err := json.Unmarshal([]byte(s), &f); err != nil {
		return 0, err
	}
	return f, nil
}

type modelsDevModel struct {
	Cost             json.RawMessage `json:"cost"`
	CanonicalModelID string          `json:"canonical_model_id"`
}

type modelsDevProvider struct {
	Models map[string]modelsDevModel `json:"models"`
}

// parseModelsDev parses the api.json document into (provider, model) → prices
// plus canonical_model_id aliases. Bad entries (invalid JSON member, missing
// id, negative/NaN/Inf/non-numeric prices) are skipped per contract B2 —
// malformed provider/model objects never take the whole source down; a
// top-level document that fails to parse is still a whole-source reject.
func parseModelsDev(data []byte) (map[string]map[string]priceRow, map[string][]string, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, nil, fmt.Errorf("models.dev: invalid json: %w", err)
	}
	out := map[string]map[string]priceRow{}
	aliases := map[string][]string{}
	for provider, rawP := range doc {
		if provider == "" {
			continue
		}
		var p modelsDevProvider
		if err := json.Unmarshal(rawP, &p); err != nil || p.Models == nil {
			continue // malformed provider entry → skip, keep the rest
		}
		for modelID, m := range p.Models {
			if modelID == "" || m.Cost == nil {
				continue
			}
			var cost map[string]json.RawMessage
			if err := json.Unmarshal(m.Cost, &cost); err != nil {
				continue
			}
			in, _, badIn := priceField(cost["input"])
			outp, _, badOut := priceField(cost["output"])
			cr, _, badCR := priceField(cost["cache_read"])
			cw, _, badCW := priceField(cost["cache_write"])
			if badIn || badOut || badCR || badCW || (in == nil && outp == nil) {
				continue // invalid, or no usable price at all → skip (one-sided
				// rows stay valid: the missing side prices as partial, B7)
			}
			row := priceRow{in: in, out: outp, cr: cr, cw: cw}
			pm := out[provider]
			if pm == nil {
				pm = map[string]priceRow{}
				out[provider] = pm
			}
			pm[modelID] = row
			if cid := strings.TrimSpace(m.CanonicalModelID); cid != "" {
				if i := strings.LastIndex(cid, "/"); i >= 0 && i+1 < len(cid) {
					cpm, cmodel := cid[:i], cid[i+1:]
					if cpm != "" && cmodel != "" {
						aliases[provider] = append(aliases[provider], cmodel)
						pm2 := out[cpm]
						if pm2 == nil {
							pm2 = map[string]priceRow{}
							out[cpm] = pm2
						}
						if _, dup := pm2[cmodel]; !dup {
							pm2[cmodel] = row
						}
					}
				}
			}
		}
	}
	return out, aliases, nil
}

// parseLiteLLM parses model_prices_and_context_window.json into per-1M rows.
// Bare keys land under their litellm_provider (e.g. gemini-2.5-pro →
// vertex_ai-language-models); `vendor/model` keys additionally keep the
// vendor-prefixed row. Entries with invalid price fields are skipped (B2).
func parseLiteLLM(data []byte) (map[string]map[string]priceRow, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("litellm: invalid json: %w", err)
	}
	out := map[string]map[string]priceRow{}
	add := func(provider, model string, r priceRow) {
		if provider == "" || model == "" {
			return
		}
		pm := out[provider]
		if pm == nil {
			pm = map[string]priceRow{}
			out[provider] = pm
		}
		if _, dup := pm[model]; !dup {
			pm[model] = r
		}
	}
	for key, rawM := range doc {
		if key == "" || key == "sample_spec" {
			continue
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(rawM, &m); err != nil || m == nil {
			continue // non-object entry → skip, keep the rest
		}
		in, _, badIn := priceField(m["input_cost_per_token"])
		outp, _, badOut := priceField(m["output_cost_per_token"])
		cr, _, badCR := priceField(m["cache_read_input_token_cost"])
		cw, _, badCW := priceField(m["cache_creation_input_token_cost"])
		if badIn || badOut || badCR || badCW || (in == nil && outp == nil) {
			continue
		}
		row := priceRow{
			in:  scaledPerM(in),
			out: scaledPerM(outp),
			cr:  scaledPerM(cr),
			cw:  scaledPerM(cw),
		}
		var provider string
		if raw := m["litellm_provider"]; raw != nil {
			_ = json.Unmarshal(raw, &provider) // non-string → "" → vendor fallback
		}
		model := key
		vendor := ""
		if i := strings.Index(key, "/"); i > 0 {
			vendor, model = key[:i], key[i+1:]
		}
		if provider == "" {
			provider = vendor
		}
		add(provider, model, row)
		if vendor != "" {
			add(vendor, model, row) // keep the vendor-prefixed variant too
		}
	}
	return out, nil
}

func scaledPerM(p *float64) *float64 {
	if p == nil {
		return nil
	}
	v := *p * 1e6
	return &v
}

func catalogRowCount(cat map[string]map[string]priceRow) int {
	n := 0
	for _, pm := range cat {
		n += len(pm)
	}
	return n
}

func fetchAndParse(ctx context.Context, source string) (map[string]map[string]priceRow, error) {
	data, err := fetchPricingSource(ctx, source)
	if err != nil {
		return nil, err
	}
	switch source {
	case pricingSourceModelsDev:
		cat, _, err := parseModelsDev(data)
		return cat, err
	default:
		return parseLiteLLM(data)
	}
}

// ---- plugin_meta ----

// metaGet/metaSet/metaDel read and write plugin_meta. Callers hold mu.
func metaGet(key string) *string {
	if db == nil {
		return nil
	}
	var v string
	if err := db.QueryRow("SELECT v FROM plugin_meta WHERE k=?", key).Scan(&v); err != nil {
		return nil
	}
	return &v
}

func metaSet(key, val string) {
	if db == nil {
		return
	}
	_, _ = db.Exec("INSERT OR REPLACE INTO plugin_meta (k,v) VALUES (?,?)", key, val)
}

func metaDel(key string) {
	if db == nil {
		return
	}
	_, _ = db.Exec("DELETE FROM plugin_meta WHERE k=?", key)
}

func pricingAutoSyncEnabled() bool {
	ensureDB()
	mu.Lock()
	v := metaGet("pricing_auto_sync")
	mu.Unlock()
	return v == nil || *v != "0" // default on
}

// ---- sync ----

var (
	syncMu         sync.Mutex // single-flight
	pricingSyncing bool
	pricingMu      sync.Mutex
	pricingMatches = map[string]map[string]any{}
	autoSyncOnce   sync.Once
	autoSyncCh     = make(chan struct{}, 1)
)

// syncPricing fetches BOTH sources independently (contract B2: per-source
// atomic replace — each source validated in isolation) and merges them by
// key: models.dev rows always win their (provider, model) key, litellm rows
// fill keys absent from models.dev. A failed source keeps its prior catalog
// rows; a total failure writes only pricing_last_error.
func syncPricing(ctx context.Context) map[string]any {
	if !syncMu.TryLock() {
		return map[string]any{"ok": false, "error": "already syncing"}
	}
	defer syncMu.Unlock()

	mu.Lock()
	pricingSyncing = true
	if db != nil {
		metaSet("pricing_syncing", "1")
	}
	mu.Unlock()
	defer func() {
		mu.Lock()
		pricingSyncing = false
		if db != nil {
			metaDel("pricing_syncing")
		}
		mu.Unlock()
	}()

	ensureDB()
	var errs []string
	var mdCat, llCat map[string]map[string]priceRow
	sources := []string{}
	for _, src := range []string{pricingSourceModelsDev, pricingSourceLiteLLM} {
		c, err := fetchAndParse(ctx, src)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		if catalogRowCount(c) == 0 {
			errs = append(errs, src+": empty catalog")
			continue
		}
		if src == pricingSourceModelsDev {
			mdCat = c
		} else {
			llCat = c
		}
		sources = append(sources, src)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	mu.Lock()
	defer mu.Unlock()
	if db == nil {
		return map[string]any{"ok": false, "error": "db unavailable"}
	}
	if len(sources) == 0 {
		msg := strings.Join(errs, "; ")
		if msg == "" {
			msg = "all pricing sources failed"
		}
		metaSet("pricing_last_error", msg)
		// Everything failed: the catalog is whatever remains from prior
		// syncs — report "cache" when there is no recorded source yet.
		if v := metaGet("pricing_last_source"); (v == nil || *v == "") && catCountLocked() > 0 {
			metaSet("pricing_last_source", "cache")
		}
		return map[string]any{"ok": false, "error": msg}
	}
	if err := commitCatalog(mdCat, llCat, now); err != nil {
		metaSet("pricing_last_error", err.Error())
		return map[string]any{"ok": false, "error": err.Error()}
	}
	source := strings.Join(sources, "+")
	metaSet("pricing_last_sync", now)
	metaSet("pricing_last_source", source)
	metaSet("pricing_last_error", strings.Join(errs, "; ")) // "" on full success
	// Persist the model→catalog match table for the status endpoint.
	updatePricingMatchesLocked()
	if raw, err := json.Marshal(pricingMatches); err == nil {
		metaSet("pricing_matches", string(raw))
	}
	return map[string]any{"ok": true, "source": source, "models": catCountLocked()}
}

func catCountLocked() int {
	if db == nil {
		return 0
	}
	var n int
	_ = db.QueryRow("SELECT COUNT(*) FROM price_catalog").Scan(&n)
	return n
}

// commitCatalog applies per-source atomic replaces in ONE transaction
// (contract B2): each provided source replaces only its own rows — a source
// absent/failed upstream keeps its catalog rows untouched. Merge rule:
// models.dev wins a contested (provider, model) key; litellm fills the rest
// and its rows at models.dev-occupied keys are discarded (never field-merged).
// Override-mirrored rows are kept; a snapshot is appended for every key
// whose effective external price changed (baseline = prior effective row,
// or the latest snapshot when an override occupies the PK).
// Caller must hold mu.
func commitCatalog(mdCat, llCat map[string]map[string]priceRow, syncedAt string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	prev := map[string]priceRow{}
	overridden := map[string]bool{}
	rows, err := tx.Query("SELECT provider, model, source, input_per_m, output_per_m, cache_read_per_m, cache_write_per_m FROM price_catalog")
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	for rows.Next() {
		var p, m, s string
		var in, out, cr, cw sql.NullFloat64
		if err := rows.Scan(&p, &m, &s, &in, &out, &cr, &cw); err != nil {
			continue
		}
		if s == pricingSourceOverride {
			overridden[p+"\x00"+m] = true
			continue
		}
		prev[p+"\x00"+m] = priceRow{in: nf(in), out: nf(out), cr: nf(cr), cw: nf(cw)}
	}
	rows.Close()
	// For overridden keys the external baseline lives in price_snapshots:
	// seed prev with each key's latest snapshot row.
	if len(overridden) > 0 {
		srows, err := tx.Query(`SELECT s.provider, s.model, s.input_per_m, s.output_per_m, s.cache_read_per_m, s.cache_write_per_m
			FROM price_snapshots s
			JOIN (SELECT provider, model, MAX(effective_from) mx FROM price_snapshots GROUP BY provider, model) l
			  ON s.provider=l.provider AND s.model=l.model AND s.effective_from=l.mx`)
		if err == nil {
			for srows.Next() {
				var p, m string
				var in, out, cr, cw sql.NullFloat64
				if err := srows.Scan(&p, &m, &in, &out, &cr, &cw); err != nil {
					continue
				}
				k := p + "\x00" + m
				if overridden[k] {
					prev[k] = priceRow{in: nf(in), out: nf(out), cr: nf(cr), cw: nf(cw)}
				}
			}
			srows.Close()
		}
	}
	snap, err := tx.Prepare(`INSERT OR IGNORE INTO price_snapshots
		(model, provider, source, input_per_m, output_per_m, cache_read_per_m, cache_write_per_m, effective_from)
		VALUES (?,?,?,?,?,?,?,?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	apply := func(cat map[string]map[string]priceRow, source string) error {
		if cat == nil {
			return nil // source failed → preserve its prior rows untouched
		}
		if _, err := tx.Exec("DELETE FROM price_catalog WHERE source = ?", source); err != nil {
			return err
		}
		providers := make([]string, 0, len(cat))
		for p := range cat {
			providers = append(providers, p)
		}
		sort.Strings(providers)
		for _, provider := range providers {
			models := make([]string, 0, len(cat[provider]))
			for m := range cat[provider] {
				models = append(models, m)
			}
			sort.Strings(models)
			for _, model := range models {
				r := cat[provider][model]
				key := provider + "\x00" + model
				// Displace a non-override row owned by another source at this
				// key — but never let a litellm row overwrite a models.dev
				// row (models.dev wins the merge).
				cond := "NOT IN ('override','models.dev')"
				if source == pricingSourceModelsDev {
					cond = "!= 'override'"
				}
				if _, err := tx.Exec("DELETE FROM price_catalog WHERE provider=? AND model=? AND source "+cond, provider, model); err != nil {
					return err
				}
				ires, err := tx.Exec(`INSERT OR IGNORE INTO price_catalog
					(model, provider, source, input_per_m, output_per_m, cache_read_per_m, cache_write_per_m, currency, synced_at)
					VALUES (?,?,?,?,?,?,?,'USD',?)`,
					model, provider, source, ptr(r.in), ptr(r.out), ptr(r.cr), ptr(r.cw), syncedAt)
				if err != nil {
					return err
				}
				if n, _ := ires.RowsAffected(); n == 0 {
					// The insert was discarded — another row still owns the
					// key. Only an override owner keeps the external→history
					// mirror semantics (its baseline lives in snapshots); a
					// discarded source row must NEVER write a snapshot, or
					// events would resolve against a price the catalog does
					// not carry (violates "one source per model", B2).
					var owner string
					if err := tx.QueryRow(`SELECT source FROM price_catalog
						WHERE provider=? AND model=?`, provider, model).Scan(&owner); err != nil || owner != pricingSourceOverride {
						continue
					}
				}
				old, seen := prev[key]
				if !seen && overridden[key] {
					continue // override row present but no external history: keep mirror, no snapshot
				}
				if !seen || !samePrices(old, r) {
					if _, err := snap.Exec(model, provider, source, ptr(r.in), ptr(r.out), ptr(r.cr), ptr(r.cw), syncedAt); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	// models.dev first so litellm rows at contested keys are discarded.
	for _, step := range []struct {
		cat    map[string]map[string]priceRow
		source string
	}{{mdCat, pricingSourceModelsDev}, {llCat, pricingSourceLiteLLM}} {
		if err := apply(step.cat, step.source); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Catalog rows changed: drop the in-memory snapshot so the next read
	// reloads (the trigger bump catches it either way — this makes the
	// invalidation explicit and immediate for same-request readers).
	pricingTablesCache.Store(nil)
	return nil
}

func nf(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	f := v.Float64
	return &f
}

func ptr(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

func samePrices(a, b priceRow) bool {
	eq := func(x, y *float64) bool {
		if x == nil || y == nil {
			return x == nil && y == nil
		}
		return *x == *y
	}
	return eq(a.in, b.in) && eq(a.out, b.out) && eq(a.cr, b.cr) && eq(a.cw, b.cw)
}

// ---- pricing tables cache (issue 6: no per-event SQLite reads) ----
//
// price_catalog + price_overrides are read through one immutable in-memory
// snapshot so a request never re-scans the catalog per event. The snapshot
// is swapped atomically; readers take the pointer while holding mu and then
// use the maps without further locking (they are never mutated in place).
//
// Staleness detection is generation-based: AFTER INSERT/UPDATE/DELETE
// triggers on both tables bump plugin_meta['pricing_gen'], so ANY writer —
// commitCatalog, the override endpoint, or a direct db.Exec in tests — is
// observed on the next acquire (one PK lookup, cheaper than even one old
// override probe). Successful commitCatalog/override writes also drop the
// cached pointer explicitly so the next request always sees fresh rows.
//
// price_snapshots is NOT part of the snapshot: rows are appended by every
// sync, so it stays lazily per-scope cached inside statusResolver only.

// pricingTables is one consistent view of the catalog and override tables.
type pricingTables struct {
	db        *sql.DB             // owning connection; a different db pointer forces rebuild
	gen       string              // plugin_meta['pricing_gen'] at load time
	cat       map[string]priceRow // "provider\x00model" → row (all sources)
	overrides map[string]priceRow // "provider\x00model" → override row
}

// pricingTablesCache holds the current snapshot; nil means "reload".
var pricingTablesCache atomic.Pointer[pricingTables]

// pricingGenLocked reads the write-generation counter. Caller holds mu.
func pricingGenLocked() string {
	var v string
	if err := db.QueryRow("SELECT v FROM plugin_meta WHERE k='pricing_gen'").Scan(&v); err != nil {
		return ""
	}
	return v
}

// ensurePricingTriggersLocked installs the write counters once per database.
// Cheap to call repeatedly (IF NOT EXISTS). Caller holds mu.
func ensurePricingTriggersLocked() {
	// Counter row must exist before any write so UPDATE always lands.
	_, _ = db.Exec("INSERT OR IGNORE INTO plugin_meta (k,v) VALUES ('pricing_gen','0')")
	for _, tbl := range []string{"price_catalog", "price_overrides"} {
		for _, ev := range []string{"INSERT", "UPDATE", "DELETE"} {
			_, _ = db.Exec(fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS pricing_gen_%s_%s
				AFTER %s ON %s BEGIN
				UPDATE plugin_meta SET v = CAST(CAST(v AS INTEGER) + 1 AS TEXT) WHERE k='pricing_gen';
				END`, strings.ToLower(ev), tbl, ev, tbl))
		}
	}
}

// rebuildPricingTablesLocked loads both tables under mu and publishes the
// new snapshot atomically. Caller holds mu.
func rebuildPricingTablesLocked() *pricingTables {
	t := &pricingTables{db: db}
	if db == nil {
		pricingTablesCache.Store(t)
		return t
	}
	ensurePricingTriggersLocked()
	t.cat = loadCatalogRowsLocked()
	t.overrides = map[string]priceRow{}
	if rows, err := db.Query(`SELECT provider, model, input_per_m, output_per_m, cache_read_per_m, cache_write_per_m
		FROM price_overrides`); err == nil {
		for rows.Next() {
			var p, m string
			var in, outp, cr, cw sql.NullFloat64
			if err := rows.Scan(&p, &m, &in, &outp, &cr, &cw); err != nil {
				continue
			}
			t.overrides[p+"\x00"+m] = priceRow{in: nf(in), out: nf(outp), cr: nf(cr), cw: nf(cw), src: pricingSourceOverride}
		}
		rows.Close()
	}
	// Generation is sampled AFTER the loads: a write that slipped in earlier
	// (possible only before trigger install on first build) is still caught
	// because its bump is already counted.
	t.gen = pricingGenLocked()
	pricingTablesCache.Store(t)
	return t
}

// catalogStateLocked returns the cached snapshot, rebuilding it when the
// connection changed or the generation moved. Caller must hold mu.
func catalogStateLocked() *pricingTables {
	if t := pricingTablesCache.Load(); t != nil && t.db == db && (t.db == nil || t.gen == pricingGenLocked()) {
		return t
	}
	return rebuildPricingTablesLocked()
}

// ---- matching (contract B.4: deterministic, ordered; never fuzzy) ----

// providerAliases maps proxy-side provider labels onto catalog providers.
var providerAliases = map[string]string{
	"codex":                     "openai",
	"openai-codex":              "openai",
	"claude":                    "anthropic",
	"gemini":                    "google",
	"google-vertex":             "google",
	"vertex_ai-language-models": "google",
	"vertex-ai-language-models": "google",
	"azure":                     "openai",
	"openai-compatibility":      "",
	"openai-compatible":         "",
}

func providerAlias(provider string) (string, bool) {
	v, ok := providerAliases[strings.ToLower(strings.TrimSpace(provider))]
	return v, ok
}

var (
	reDateISO = regexp.MustCompile(`-\d{4}-\d{2}-\d{2}$`)
	reDateCmp = regexp.MustCompile(`-\d{8}$`)
	reSep     = regexp.MustCompile(`[_\s]+`)
	reVendor  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*/`)
)

// normalizeModelID lowercases, strips a leading `vendor/` prefix and unifies
// `_`/spaces to `-`. Date suffixes are stripped only by the caller after the
// exact dated variant fails to match.
func normalizeModelID(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	m = reVendor.ReplaceAllString(m, "")
	m = reSep.ReplaceAllString(m, "-")
	return m
}

func stripDateSuffix(m string) string {
	if reDateISO.MatchString(m) {
		return reDateISO.ReplaceAllString(m, "")
	}
	if reDateCmp.MatchString(m) {
		return reDateCmp.ReplaceAllString(m, "")
	}
	return m
}

type matchResult struct {
	ok            bool
	provider      string   // matched catalog provider (or event provider when unmatched)
	model         string   // matched catalog model (or event model when unmatched)
	row           priceRow // contract metadata `prices`
	method        string   // exact|model_exact|provider_alias|normalized|canonical|ambiguous|unmatched
	status        string   // exact|alias|normalized|ambiguous|unmatched (contract B2)
	candidates    []string // providers that carry the id when ambiguous
	aliasProvider string
}

func splitKey(k string) (string, string) {
	if i := strings.Index(k, "\x00"); i >= 0 {
		return k[:i], k[i+1:]
	}
	return "", k
}

// loadCatalogRowsLocked snapshots price_catalog into a key→row lookup map.
// Caller holds mu; used by rebuildPricingTablesLocked (the cached copy is
// what readers see — nothing else should load the catalog per call).
func loadCatalogRowsLocked() map[string]priceRow {
	out := map[string]priceRow{}
	if db == nil {
		return out
	}
	rows, err := db.Query("SELECT provider, model, source, input_per_m, output_per_m, cache_read_per_m, cache_write_per_m FROM price_catalog")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var p, m, s string
		var pin, pout, pcr, pcw sql.NullFloat64
		if err := rows.Scan(&p, &m, &s, &pin, &pout, &pcr, &pcw); err != nil {
			continue
		}
		out[p+"\x00"+m] = priceRow{in: nf(pin), out: nf(pout), cr: nf(pcr), cw: nf(pcw), src: s}
	}
	return out
}

// matchPrice resolves (provider, model) to a catalog row per contract B2:
// provider-aware, deterministic, never fuzzy. Cross-provider id reuse is a
// match only when unambiguous (single provider row or identical price
// tuples); differing tuples → status ambiguous (unpriced).
func matchPrice(provider, model string) matchResult {
	ensureDB()
	mu.Lock()
	tabs := catalogStateLocked()
	mu.Unlock()
	return matchInCatalog(tabs.cat, provider, model)
}

// matchPriceLocked is matchPrice with mu already held.
func matchPriceLocked(provider, model string) matchResult {
	return matchInCatalog(catalogStateLocked().cat, provider, model)
}

func newMatchResult(provider, model string) matchResult {
	res := matchResult{provider: provider, model: model, method: "unmatched", status: "unmatched", aliasProvider: provider}
	if ap, ok := providerAlias(provider); ok {
		res.aliasProvider = ap
	}
	return res
}

// matchInCatalog is the shared ordered matcher over a key→row map.
//
// Order (contract B + B2):
//  1. exact provider+model
//  2. bare model id across providers — the event provider's own row or a
//     provider-alias row wins (status exact/alias); remaining providers
//     resolve only when unambiguous (one row, or all rows share identical
//     price tuples → exact), otherwise the id is AMBIGUOUS, deferred as the
//     final verdict
//  3. normalized id (lowercase, separator unify, vendor/ strip) within the
//     provider scope [provider, alias, vendor-prefix segments], then the
//     same cross-provider ambiguity rule
//  4. date-suffix strip of the normalized id — scope providers ONLY when
//     the dated variant is absent (cross-provider date strip forbidden)
//  5. canonical tail after the last '/' — scope, then ambiguity rule
func matchInCatalog(cat map[string]priceRow, provider, model string) matchResult {
	provider = strings.ToLower(strings.TrimSpace(provider)) // F3: catalog
	// providers are lowercase ids; callers may send "OpenAI ".
	res := newMatchResult(provider, model)
	if len(cat) == 0 {
		return res
	}
	// scopeProviders returns the ordered "same provider" scope for candidate
	// id m: the event provider, its alias, then each vendor prefix segment
	// embedded in the model id itself (a "vendor/model" id declares its
	// provider — matching under it is same-provider, not cross-provider).
	scopeProviders := func(orig string) []string {
		out := []string{provider}
		seen := map[string]bool{provider: true}
		if res.aliasProvider != provider && res.aliasProvider != "" && !seen[res.aliasProvider] {
			seen[res.aliasProvider] = true
			out = append(out, res.aliasProvider)
		}
		if segs := strings.Split(orig, "/"); len(segs) > 1 {
			for _, seg := range segs[:len(segs)-1] {
				seg = strings.ToLower(strings.TrimSpace(seg))
				if seg != "" && !seen[seg] {
					seen[seg] = true
					out = append(out, seg)
				}
			}
		}
		if provider != "" && !seen[""] {
			// '' catalog rows (unlabeled) remain reachable for scoped matches.
			seen[""] = true
			out = append(out, "")
		}
		return out
	}
	// providersWith returns the sorted provider list carrying id m.
	providersWith := func(m string) []string {
		var ps []string
		for k := range cat {
			if strings.HasSuffix(k, "\x00"+m) {
				ps = append(ps, k[:len(k)-len(m)-1])
			}
		}
		sort.Strings(ps)
		return ps
	}
	// tryScoped checks candidate id m against the scope only.
	tryScoped := func(orig, m, method string) (matchResult, bool) {
		for _, sp := range scopeProviders(orig) {
			if r, ok := cat[sp+"\x00"+m]; ok {
				mr := res
				mr.candidates = nil
				mr.ok, mr.row, mr.method = true, r, method
				mr.provider, mr.model = sp, m
				switch {
				case method == "model_exact" && sp == provider:
					mr.status = "exact"
				case method == "model_exact" && sp == res.aliasProvider:
					mr.status = "alias"
				case method == "model_exact":
					mr.status = "normalized" // unlabeled '' or vendor-seg row
				default:
					mr.status = "normalized"
				}
				return mr, true
			}
		}
		return res, false
	}
	// tryAny applies the B2 cross-provider rule to candidate id m after the
	// scope misses: unique provider row or identical tuples → match; else
	// ambiguous (ok=false keeps scanning — the verdict is recorded).
	tryAny := func(m, method string) (matchResult, bool) {
		ps := providersWith(m)
		if len(ps) == 0 {
			return res, false
		}
		identical := true
		for _, q := range ps[1:] {
			if !samePrices(cat[ps[0]+"\x00"+m], cat[q+"\x00"+m]) {
				identical = false
				break
			}
		}
		if !identical {
			res.method, res.status, res.candidates = "ambiguous", "ambiguous", ps
			return res, false
		}
		mr := res
		mr.candidates = nil
		mr.ok, mr.row = true, cat[ps[0]+"\x00"+m]
		mr.provider, mr.model, mr.method = ps[0], m, method
		if method == "model_exact" {
			mr.status = "exact"
		} else {
			mr.status = "normalized"
		}
		return mr, true
	}

	// 1. exact provider+model
	if r, ok := cat[provider+"\x00"+model]; ok {
		res.ok, res.row, res.method, res.status = true, r, "exact", "exact"
		return res
	}
	// 2. bare model id
	if mr, hit := tryScoped(model, model, "model_exact"); hit {
		return mr
	}
	ambiguous := false
	if mr, hit := tryAny(model, "model_exact"); hit {
		return mr
	}
	ambiguous = res.status == "ambiguous"
	// 3. normalized id
	norm := normalizeModelID(model)
	if norm != "" {
		if mr, hit := tryScoped(model, norm, "normalized"); hit {
			return mr
		}
		if mr, hit := tryAny(norm, "normalized"); hit {
			return mr
		}
		if res.status == "ambiguous" {
			ambiguous = true
		}
	}
	// 4. date strip — scope only, only when the dated variant is absent.
	// Reaching here means the dated id matched nothing in scope, so it is
	// absent by construction; stripping stays same-provider.
	if stripped := stripDateSuffix(norm); stripped != "" && stripped != norm {
		if mr, hit := tryScoped(model, stripped, "normalized"); hit {
			return mr
		}
	}
	// 5. canonical tail
	if i := strings.LastIndex(model, "/"); i >= 0 && i+1 < len(model) {
		tail := normalizeModelID(model[i+1:])
		if tail != "" && tail != norm && tail != model {
			if mr, hit := tryScoped(model, tail, "canonical"); hit {
				return mr
			}
			if mr, hit := tryAny(tail, "canonical"); hit {
				return mr
			}
			if res.status == "ambiguous" {
				ambiguous = true
			}
		}
	}
	if ambiguous {
		res.method, res.status = "ambiguous", "ambiguous"
		res.provider, res.model = provider, model
	}
	return res
}

// ---- effective pricing ----

type effectivePrice struct {
	row          priceRow
	snapshot     string // override | historical | latest
	matchedModel string
	matchedProv  string
	method       string   // match method, 'override', or 'snapshot-legacy'
	status       string   // match status verbatim; override|historical for non-catalog paths
	candidates   []string // providers carrying the id when status == ambiguous
	snapID       *int64   // price_snapshots.id when snapshot == "historical"
	source       string   // price source that produced row
}

// priceBasis is the ts-independent resolution of (provider, model): an
// override row, a catalog match, or a no-price verdict (unmatched/ambiguous)
// that may still be rescued by an event-scoped snapshot.
type priceBasis struct {
	override bool
	ovrRow   priceRow
	mr       matchResult
}

// priceBasisForLocked resolves the ts-independent part: overrides win, then
// catalog matching. tabs supplies the (cached) catalog + overrides; when nil
// or db==nil the lookup degenerates to a plain catalog match. Caller must
// hold mu only if tabs was fetched under mu — the maps are immutable.
func priceBasisForLocked(provider, model string, tabs *pricingTables) priceBasis {
	provider = strings.ToLower(strings.TrimSpace(provider)) // F3
	var b priceBasis
	if tabs != nil {
		if r, ok := tabs.overrides[provider+"\x00"+model]; ok {
			b.override, b.ovrRow = true, r
			return b
		}
		// F4: provider="" override rows are GLOBAL — they win over any
		// provider's catalog row for the model (RULING).
		if provider != "" {
			if r, ok := tabs.overrides["\x00"+model]; ok {
				b.override, b.ovrRow = true, r
				return b
			}
		}
	}
	if tabs != nil {
		b.mr = matchInCatalog(tabs.cat, provider, model)
	} else {
		b.mr = matchInCatalog(nil, provider, model)
	}
	return b
}

// priceFromBasisLocked applies the time axis to a resolved basis: newest
// snapshot effective_from <= ts (unless recalc), then the current catalog
// row labeled "latest". Ambiguous matches may only fall back to the event's
// OWN (provider, model) snapshot — never another provider's price.
// Caller must hold mu.
func priceFromBasisLocked(b priceBasis, provider, model string, tsUnix float64, recalc bool) (effectivePrice, bool) {
	provider = strings.ToLower(strings.TrimSpace(provider)) // F3: keep the
	// snapshot-fallback probe on the same normalized provider as matching.
	var out effectivePrice
	if b.override {
		out.row = b.ovrRow
		out.snapshot, out.matchedModel, out.matchedProv = "override", model, provider
		out.method, out.status, out.source = "override", "override", pricingSourceOverride
		return out, true
	}
	mr := b.mr
	if !mr.ok {
		// The model may have been dropped from the upstream catalog: an old
		// snapshot still prices historical events (contract B.6). Ambiguous
		// ids never borrow another provider's snapshot.
		if ep, ok := snapshotFallbackLocked(provider, model, tsUnix, mr.status == "ambiguous"); ok {
			return ep, true
		}
		out.method, out.status, out.candidates = mr.method, mr.status, mr.candidates
		if out.method == "" {
			out.method = "unmatched"
			out.status = "unmatched"
		}
		return out, false
	}
	out.matchedModel, out.matchedProv, out.method, out.status = mr.model, mr.provider, mr.method, mr.status
	out.candidates = mr.candidates
	if mr.row.src == pricingSourceOverride {
		// An override-mirrored catalog row wins by construction; it reports
		// as an override, never as "latest"/"historical" (F4: the same rule
		// lets provider='' global override mirrors dominate).
		out.row = mr.row
		out.snapshot = "override"
		out.status = "override"
		out.source = pricingSourceOverride
		return out, true
	}
	if !recalc {
		var sid int64
		var src string
		var ein, eout, ecr, ecw sql.NullFloat64
		err := db.QueryRow(`SELECT id, source, input_per_m, output_per_m, cache_read_per_m, cache_write_per_m
			FROM price_snapshots WHERE provider=? AND model=? AND effective_from<=?
			ORDER BY effective_from DESC LIMIT 1`,
			mr.provider, mr.model, time.Unix(int64(tsUnix), 0).UTC().Format(time.RFC3339)).
			Scan(&sid, &src, &ein, &eout, &ecr, &ecw)
		if err == nil {
			out.row = priceRow{in: nf(ein), out: nf(eout), cr: nf(ecr), cw: nf(ecw), src: src}
			out.snapshot = "historical"
			out.status = "historical" // B2: snapshot-resolved ⇒ status historical
			out.snapID = &sid
			out.source = src
			return out, true
		}
	}
	out.row = mr.row
	out.snapshot = "latest"
	out.source = mr.row.src
	return out, true
}

// priceFor returns the effective price for (provider, model) at event time
// tsUnix: overrides win, then the newest snapshot effective_from <= ts
// (unless recalc), then the current catalog row labeled "latest".
func priceFor(provider, model string, tsUnix float64, recalc bool) (effectivePrice, bool) {
	var out effectivePrice
	ensureDB()
	mu.Lock()
	defer mu.Unlock()
	if db == nil {
		return out, false
	}
	b := priceBasisForLocked(provider, model, catalogStateLocked())
	return priceFromBasisLocked(b, provider, model, tsUnix, recalc)
}

// snapshotFallbackLocked returns the newest price_snapshots row effective at
// or before tsUnix for the event's own (provider, model). An empty provider
// matches snapshots under any provider (newest first) — but ONLY for
// unmatched events; providerOnly=true (ambiguous) forbids cross-provider
// borrowing. It runs when catalog matching failed, so historical events keep
// their price after a model disappears upstream. Caller must hold mu.
func snapshotFallbackLocked(provider, model string, tsUnix float64, providerOnly bool) (effectivePrice, bool) {
	provider = strings.ToLower(strings.TrimSpace(provider)) // F3
	var out effectivePrice
	if db == nil {
		return out, false
	}
	ts := time.Unix(int64(tsUnix), 0).UTC().Format(time.RFC3339)
	var sid int64
	var sp, sm, src string
	var in, outp, cr, cw sql.NullFloat64
	var err error
	if provider == "" && !providerOnly {
		err = db.QueryRow(`SELECT id, provider, model, source, input_per_m, output_per_m, cache_read_per_m, cache_write_per_m
			FROM price_snapshots WHERE model=? AND effective_from<=?
			ORDER BY effective_from DESC, provider LIMIT 1`, model, ts).
			Scan(&sid, &sp, &sm, &src, &in, &outp, &cr, &cw)
	} else if provider != "" {
		err = db.QueryRow(`SELECT id, provider, model, source, input_per_m, output_per_m, cache_read_per_m, cache_write_per_m
			FROM price_snapshots WHERE provider=? AND model=? AND effective_from<=?
			ORDER BY effective_from DESC LIMIT 1`, provider, model, ts).
			Scan(&sid, &sp, &sm, &src, &in, &outp, &cr, &cw)
	} else {
		return out, false // ambiguous with empty provider: no safe scope
	}
	if err != nil {
		return out, false
	}
	out.row = priceRow{in: nf(in), out: nf(outp), cr: nf(cr), cw: nf(cw), src: src}
	out.snapshot = "historical"
	out.matchedModel, out.matchedProv = sm, sp
	out.method = "snapshot-legacy"
	out.status = "historical"
	out.snapID = &sid
	out.source = src
	return out, true
}

// ---- event costing ----

type eventCostInfo struct {
	usd      float64
	inCost   float64
	outCost  float64
	crCost   float64
	cwCost   float64
	matched  bool
	complete bool
	status   string // priced|partial|unpriced|ambiguous (contract B2)
	ep       effectivePrice
	okIn     bool // component was priced (or unused)
	okOut    bool
	okCR     bool
	okCW     bool
}

// computeCost derives an eventCostInfo from an already-resolved price.
func computeCost(e eventRow, ep effectivePrice, priced bool) eventCostInfo {
	var c eventCostInfo
	c.ep = ep
	if !priced {
		if ep.status == "ambiguous" {
			c.status = "ambiguous"
		} else {
			c.status = "unpriced"
		}
		return c
	}
	c.matched = true
	comp := func(tokens int64, price *float64) (float64, bool) {
		if tokens <= 0 {
			return 0, true // unused component needs no price
		}
		if price == nil {
			return 0, false
		}
		return float64(tokens) * (*price) / 1e6, true
	}
	baseIn := e.input - e.cache - e.cacheWrite
	if baseIn < 0 {
		baseIn = 0
	}
	c.inCost, c.okIn = comp(baseIn, ep.row.in)
	c.outCost, c.okOut = comp(e.output, ep.row.out)
	c.crCost, c.okCR = comp(e.cache, ep.row.cr)
	c.cwCost, c.okCW = comp(e.cacheWrite, ep.row.cw)
	c.usd = c.inCost + c.outCost + c.crCost + c.cwCost
	c.complete = c.okIn && c.okOut && c.okCR && c.okCW
	if c.complete {
		c.status = "priced"
	} else {
		c.status = "partial"
	}
	return c
}

// costEvent computes the USD cost for one normalized event per contract B.7:
// input_cost = max(0, input-cache_read-cache_write)*p_in + cache_read*p_cr +
// cache_write*p_cw + output*p_out, each /1e6. A used component without a
// price → complete=false (partial); no match → matched=false (unpriced).
// Reasoning tokens are never priced separately (already inside output).
func costEvent(e eventRow, recalc bool) eventCostInfo {
	ep, ok := priceFor(e.provider, e.model, e.ts, recalc)
	return computeCost(e, ep, ok)
}

// priceForEvent backs the costForEvent hook: (usd, priced) where priced is
// true only for fully-priced events.
func priceForEvent(e eventRow) (float64, bool) {
	c := costEvent(e, false)
	return c.usd, c.matched && c.complete
}

func init() {
	costForEvent = func(e eventRow) (float64, bool) { return priceForEvent(e) }
}

// matchResultFor converts one event's resolution into the reporting
// matchResult (model_matches / unmatched buckets). Non-catalog paths keep
// their own status verbatim: override | historical (snapshot-resolved) |
// ambiguous | unmatched. Unpriced rows fall back to the event pair so the
// report still names the model.
func matchResultFor(e eventRow, ep effectivePrice, ok bool) matchResult {
	mr := matchResult{
		ok:         ok,
		provider:   ep.matchedProv,
		model:      ep.matchedModel,
		method:     ep.method,
		status:     ep.status,
		candidates: ep.candidates,
	}
	if mr.status == "" {
		mr.status = "unmatched"
	}
	if mr.method == "" {
		mr.method = mr.status
	}
	if mr.provider == "" {
		mr.provider = e.provider
	}
	if mr.model == "" {
		mr.model = e.model
	}
	return mr
}

// matchRecord converts a matchResult into the model_matches entry shape
// (contract B2 adds `status`; `candidates` lists providers on ambiguity).
func matchRecord(mr matchResult) map[string]any {
	m := map[string]any{
		"matched_model":    mr.model,
		"matched_provider": mr.provider,
		"provider":         mr.provider,
		"method":           mr.method,
		"status":           mr.status,
	}
	if mr.status == "ambiguous" {
		m["candidates"] = mr.candidates
	}
	return m
}

// recordMatch tracks the last match provenance per (provider, model).
func recordMatch(provider, model string, mr matchResult) {
	pricingMu.Lock()
	defer pricingMu.Unlock()
	pricingMatches[provider+"\x00"+model] = matchRecord(mr)
}

// updatePricingMatchesLocked refreshes pricingMatches for every distinct
// event model in the DB. Caller must hold mu.
func updatePricingMatchesLocked() {
	if db == nil {
		return
	}
	rows, err := db.Query("SELECT payload FROM usage_events")
	if err != nil {
		return
	}
	type pair struct{ provider, model string }
	seen := map[pair]bool{}
	var list []pair
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			continue
		}
		var e dbEvent
		if err := json.Unmarshal([]byte(payload), &e); err != nil {
			continue
		}
		m := e.Model
		if m == "" {
			m = e.Alias
		}
		if m == "" {
			m = "unknown"
		}
		p := pair{e.Provider, m}
		if !seen[p] {
			seen[p] = true
			list = append(list, p)
		}
	}
	rows.Close()
	cat := catalogStateLocked().cat
	pricingMu.Lock()
	defer pricingMu.Unlock()
	for _, p := range list {
		mr := matchInCatalog(cat, p.provider, p.model)
		pricingMatches[p.provider+"\x00"+p.model] = matchRecord(mr)
	}
}

// ---- stable cost estimates (contract B2, issue 3) ----

// estimateResolver resolves events to eventCostInfo with stable storage:
// a stored cost_estimates row always wins; a missing row is computed with
// snapshot semantics and INSERT OR IGNORE'd (never updates); recalc=1 is
// the only path that recomputes from the latest catalog and REPLACES rows.
type estimateResolver struct {
	res     *statusResolver // shared basis/snapshot caches + recalc flag
	stored  map[int64]eventCostInfo
	exists  map[int64]bool
	now     string
	write   bool // false = read-only resolution (request inspector): never queues persists
	pending []estRow
}

type estRow struct {
	e eventRow
	c eventCostInfo
}

// resolveEstimates returns one eventCostInfo per event plus the per-event
// matchResult for reporting (match status drives model_matches and the
// ambiguous/unmatched buckets even when a stored estimate carries the USD).
// Missing estimates are persisted (INSERT OR IGNORE); recalc=1 replaces.
func resolveEstimates(events []eventRow, recalc bool) ([]eventCostInfo, []matchResult) {
	return resolveEstimatesMode(events, recalc, true)
}

// resolveEstimatesReadOnly resolves estimates without persisting: stored
// rows still win, but computed results stay in memory. The request-inspector
// endpoints are read-only — only the analyticsCost recalc flow may write
// cost_estimates, so this lane never materializes rows as a read side
// effect.
func resolveEstimatesReadOnly(events []eventRow) ([]eventCostInfo, []matchResult) {
	return resolveEstimatesMode(events, false, false)
}

func resolveEstimatesMode(events []eventRow, recalc, write bool) ([]eventCostInfo, []matchResult) {
	r := &estimateResolver{
		stored: map[int64]eventCostInfo{},
		exists: map[int64]bool{},
		now:    time.Now().UTC().Format(time.RFC3339),
		write:  write,
	}
	ensureDB()
	mu.Lock()
	r.res = newStatusResolver(recalc)
	ids := make([]int64, 0, len(events))
	for _, e := range events {
		if e.id > 0 {
			ids = append(ids, e.id)
		}
	}
	r.loadStoredLocked(ids)
	needs := make([]bool, len(events))
	for i, e := range events {
		// Compute unless a stored estimate covers the event — same predicate
		// as before: !recalc && e.id > 0 && exists ⇒ use the stored row.
		needs[i] = recalc || e.id <= 0 || !r.exists[e.id]
	}
	// Warm pass: resolve every needed event once under mu so all basis and
	// snapshot-list caches are populated; the real pass below then runs
	// purely in memory (persist re-locks mu on its own).
	for i, e := range events {
		if needs[i] {
			_, _ = r.res.resolveEvent(e)
		}
	}
	mu.Unlock()
	infos := make([]eventCostInfo, len(events))
	for i, e := range events {
		if !needs[i] {
			infos[i] = r.stored[e.id]
			continue
		}
		infos[i] = r.computeEvent(e)
	}
	r.persist(r.pending) // estimate writes re-acquire mu internally
	mrs := make([]matchResult, len(events))
	for i, e := range events {
		mrs[i] = r.matchFor(e, infos[i])
	}
	return infos, mrs
}

// loadStoredLocked reads existing cost_estimates rows for the given event
// ids in chunked IN() queries. Caller holds mu.
func (r *estimateResolver) loadStoredLocked(ids []int64) {
	if db == nil || len(ids) == 0 {
		return
	}
	seen := map[int64]bool{}
	uniq := make([]int64, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	for i := 0; i < len(uniq); i += 400 {
		end := i + 400
		if end > len(uniq) {
			end = len(uniq)
		}
		chunk := uniq[i:end]
		ph := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		args := make([]any, len(chunk))
		for j, id := range chunk {
			args[j] = id
		}
		rows, err := db.Query(`SELECT event_id, matched_model, matched_provider, source, snapshot_id,
			usd, input_usd, output_usd, cache_read_usd, cache_write_usd, status
			FROM cost_estimates WHERE event_id IN (`+ph+`)`, args...)
		if err != nil {
			return
		}
		for rows.Next() {
			var id int64
			var st string
			var mmo, mpo, srco sql.NullString
			var sid sql.NullInt64
			var usd, inU, outU, crU, cwU sql.NullFloat64
			if err := rows.Scan(&id, &mmo, &mpo, &srco, &sid, &usd, &inU, &outU, &crU, &cwU, &st); err != nil {
				continue
			}
			var c eventCostInfo
			c.status = st
			c.matched = st == "priced" || st == "partial"
			c.complete = st == "priced"
			c.usd = usd.Float64
			c.inCost = inU.Float64
			c.outCost = outU.Float64
			c.crCost = crU.Float64
			c.cwCost = cwU.Float64
			c.okIn, c.okOut, c.okCR, c.okCW = inU.Valid, outU.Valid, crU.Valid, cwU.Valid
			c.ep.matchedModel, c.ep.matchedProv, c.ep.source = mmo.String, mpo.String, srco.String
			c.ep.method = "stored"
			if sid.Valid {
				v := sid.Int64
				c.ep.snapID = &v
				c.ep.snapshot = "historical"
			} else if srco.String == pricingSourceOverride {
				c.ep.snapshot = "override"
			} else {
				c.ep.snapshot = "latest"
			}
			r.stored[id] = c
			r.exists[id] = true
		}
		rows.Close()
	}
}

// computeEvent computes one event's estimate under snapshot (or latest when
// recalc) semantics and queues its row for INSERT OR IGNORE / REPLACE. Runs
// without mu: the warm pass already loaded every needed snapshot scope, so
// resolveEvent is pure in-memory here.
func (r *estimateResolver) computeEvent(e eventRow) eventCostInfo {
	ep, priced := r.res.resolveEvent(e)
	c := computeCost(e, ep, priced)
	if e.id > 0 && r.write {
		r.pending = append(r.pending, estRow{e: e, c: c})
	}
	return c
}

// matchFor returns the reporting matchResult for one resolved event. Computed
// events report the live match status; stored estimates report method
// "stored", the persisted matched pair, and the CURRENT catalog match status
// (so status stays inside the match-status enum even when the stored row was
// priced before a catalog change).
func (r *estimateResolver) matchFor(e eventRow, c eventCostInfo) matchResult {
	mr := matchResultFor(e, c.ep, c.matched)
	if c.ep.method == "stored" {
		live := matchInCatalog(r.res.tabs.cat, e.provider, e.model)
		mr.status = live.status
		mr.candidates = live.candidates
	}
	if mr.status == "" {
		mr.status = "unmatched"
	}
	if mr.method == "" {
		mr.method = mr.status
	}
	return mr
}

// persist writes queued estimates: INSERT OR REPLACE on recalc (the only
// mutating path), INSERT OR IGNORE otherwise — stored rows are never
// rewritten by later syncs or overrides.
func (r *estimateResolver) persist(rows []estRow) {
	if len(rows) == 0 {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	if db == nil {
		return
	}
	verb := "INSERT OR IGNORE"
	if r.res.recalc {
		verb = "INSERT OR REPLACE"
	}
	stmt, err := db.Prepare(verb + ` INTO cost_estimates
		(event_id, model, provider, matched_model, matched_provider, source, snapshot_id,
		 usd, input_usd, output_usd, cache_read_usd, cache_write_usd, status, calculated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return
	}
	defer stmt.Close()
	f := func(v float64, ok bool) any {
		if !ok {
			return nil
		}
		return v
	}
	ns := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	for _, row := range rows {
		e, c := row.e, row.c
		var usd any
		if c.matched {
			usd = c.usd // missing must never read as zero
		}
		var sid any
		if c.ep.snapID != nil {
			sid = *c.ep.snapID
		}
		_, _ = stmt.Exec(e.id, e.model, e.provider,
			ns(c.ep.matchedModel), ns(c.ep.matchedProv), ns(c.ep.source), sid,
			usd, f(c.inCost, c.okIn), f(c.outCost, c.okOut), f(c.crCost, c.okCR), f(c.cwCost, c.okCW),
			c.status, r.now)
	}
}

// ---- auto sync (lazy; never blocks the usage path) ----

// maybeAutoSync performs the 24h staleness check and kicks a background sync.
// First-run is automatic too: with auto_sync on and no last_sync anchor, an
// empty catalog triggers an immediate sync so fresh installs price events
// without a manual POST /sync.
func maybeAutoSync() {
	autoSyncOnce.Do(func() {
		go func() {
			for range autoSyncCh {
				syncPricing(context.Background())
			}
		}()
	})
	if !pricingAutoSyncEnabled() {
		return
	}
	// Drop any signal queued by an earlier call: the staleness decision is
	// re-evaluated now, so one fresh send is enough and a stale queued tick
	// can't trigger an extra sync later.
	select {
	case <-autoSyncCh:
	default:
	}
	mu.Lock()
	anchor := metaGet("pricing_last_sync")
	var catN int
	if db != nil {
		_ = db.QueryRow("SELECT COUNT(*) FROM price_catalog").Scan(&catN)
	}
	mu.Unlock()
	if anchor == nil || *anchor == "" {
		if catN != 0 {
			return // populated catalog without anchor: leave the first sync manual
		}
		// first-run: empty catalog with auto_sync on falls through to the send
	} else {
		t, err := time.Parse(time.RFC3339, *anchor)
		if err != nil {
			return
		}
		if time.Since(t) < autoSyncInterval {
			return
		}
	}
	select {
	case autoSyncCh <- struct{}{}:
	default:
	}
}

// ---- fx ----

// The USD/VND exchange-rate engine lives in exchange_rate.go; fxInfo()
// there resolves the effective rate (auto fetch cache → manual fallback)
// and is embedded in pricingStatus and analyticsCost responses.

// ---- handlers ----

// handlePricing routes /api/pricing/* requests; method is the HTTP verb.
func handlePricing(path, method string, body any) ([]byte, error) {
	i := strings.Index(path, "/api/pricing/")
	if i < 0 {
		return managementResponse(404, "text/plain; charset=utf-8", []byte("not found"))
	}
	name := strings.TrimSuffix(strings.SplitN(path[i+len("/api/pricing/"):], "?", 2)[0], "/")
	var out []byte
	var err error
	switch {
	case name == "status" && method == http.MethodGet:
		out, err = pricingStatus()
	case name == "models" && method == http.MethodGet:
		out, err = pricingModels()
	case name == "sync" && method == http.MethodPost:
		out, err = json.Marshal(syncPricing(context.Background()))
	case name == "override" && method == http.MethodPost:
		out, err = pricingOverrideEndpoint(body)
	case name == "settings" && method == http.MethodPost:
		out, err = pricingSettingsEndpoint(body)
	default:
		return managementResponse(404, "text/plain; charset=utf-8", []byte("not found"))
	}
	if err != nil {
		if errors.Is(err, errInvalidPrice) {
			b, _ := json.Marshal(map[string]any{"ok": false, "error": "invalid_price"})
			return managementResponse(400, "application/json; charset=utf-8", b)
		}
		b, _ := json.Marshal(map[string]any{"error": err.Error()})
		return managementResponse(400, "application/json; charset=utf-8", b)
	}
	return managementResponse(200, "application/json; charset=utf-8", out)
}

// normalizeBody accepts whatever the host sent: a JSON object (map), a
// JSON-encoded string, a []byte, or nil.
//
// Wire caveat: the host marshals ManagementRequest.Body ([]byte) into JSON,
// so a JSON body arrives here as a base64 STRING — not the raw text. Detect
// that shape and decode; non-base64 strings pass through unchanged.
func normalizeBody(body any) ([]byte, error) {
	switch b := body.(type) {
	case nil:
		return nil, nil
	case string:
		s := strings.TrimSpace(b)
		if s == "" {
			return nil, nil
		}
		if json.Valid([]byte(s)) {
			return []byte(s), nil
		}
		// Host-wire base64 (ManagementRequest.Body []byte → JSON string).
		if dec, err := base64.StdEncoding.DecodeString(s); err == nil && json.Valid(dec) {
			return dec, nil
		}
		return []byte(s), nil // let the caller's unmarshal report the syntax error
	case []byte:
		return b, nil
	case json.RawMessage:
		return b, nil
	default:
		return json.Marshal(b)
	}
}

// ---- status resolution (priceFor-equivalent, read-only) ----

// snapRow is one price_snapshots entry in memory (the resolver keeps
// per-scope lists so the event loop never hits SQLite per event).
type snapRow struct {
	id                                     int64
	provider, model, source, effectiveFrom string
	in, outp, cr, cw                       *float64
}

// statusResolver resolves events the same way priceFor does (override →
// catalog → newest snapshot ≤ ts → snapshot fallback) but read-only, over
// the shared cached pricing tables plus per-pair basis and per-scope
// snapshot-list caches. pricingStatus uses it so coverage agrees with
// analyticsCost (contract B2: an event priced via snapshot fallback or
// override counts as priced); estimateResolver reuses it so estimate
// resolution performs no per-event DB work either.
//
// SnapList lazily queries price_snapshots — call it under mu; once a scope
// is loaded it stays memoized, so resolve calls are pure in-memory after a
// warm pass (see resolveEstimates).
type statusResolver struct {
	tabs   *pricingTables
	recalc bool // skip the historical-snapshot probe for matched rows
	basis  map[string]priceBasis
	snaps  map[string][]snapRow
	seen   map[string]bool
}

func newStatusResolver(recalc bool) *statusResolver {
	return &statusResolver{
		tabs:   catalogStateLocked(),
		recalc: recalc,
		basis:  map[string]priceBasis{},
		snaps:  map[string][]snapRow{},
		seen:   map[string]bool{},
	}
}

// basisFor memoizes the ts-independent basis per (normalized provider,
// model) — basis is recalc-independent (it only decides override vs catalog
// match vs unmatched/ambiguous), so one key per pair is correct.
func (s *statusResolver) basisFor(provider, model string) priceBasis {
	key := provider + "\x00" + model
	b, ok := s.basis[key]
	if !ok {
		b = priceBasisForLocked(provider, model, s.tabs)
		s.basis[key] = b
	}
	return b
}

// snapList loads and memoizes the snapshot candidates for one probe scope:
// (provider, model) scoped, or (anyProvider, model) for the empty-provider
// unmatched scan — same ordering as the equivalent price_snapshots queries.
func (s *statusResolver) snapList(anyProvider bool, provider, model string) []snapRow {
	key := "\x00" + provider + "\x00" + model
	if anyProvider {
		key = "\x01" + model
	}
	if s.seen[key] {
		return s.snaps[key]
	}
	s.seen[key] = true
	var list []snapRow
	if db != nil {
		var rows *sql.Rows
		var err error
		if anyProvider {
			rows, err = db.Query(`SELECT id, provider, model, source, input_per_m, output_per_m, cache_read_per_m, cache_write_per_m, effective_from
				FROM price_snapshots WHERE model=? ORDER BY effective_from DESC, provider`, model)
		} else {
			rows, err = db.Query(`SELECT id, provider, model, source, input_per_m, output_per_m, cache_read_per_m, cache_write_per_m, effective_from
				FROM price_snapshots WHERE provider=? AND model=? ORDER BY effective_from DESC`, provider, model)
		}
		if err == nil {
			for rows.Next() {
				var r snapRow
				var in, outp, cr, cw sql.NullFloat64
				if err := rows.Scan(&r.id, &r.provider, &r.model, &r.source, &in, &outp, &cr, &cw, &r.effectiveFrom); err != nil {
					continue
				}
				r.in, r.outp, r.cr, r.cw = nf(in), nf(outp), nf(cr), nf(cw)
				list = append(list, r)
			}
			rows.Close()
		}
	}
	s.snaps[key] = list
	return list
}

// snapAt returns the first list entry effective at or before tsUnix
// (effectiveFrom is RFC3339 UTC, so string compare works like the SQL).
func snapAt(list []snapRow, tsUnix float64) *snapRow {
	ts := time.Unix(int64(tsUnix), 0).UTC().Format(time.RFC3339)
	for i := range list {
		if list[i].effectiveFrom <= ts {
			return &list[i]
		}
	}
	return nil
}

// fallbackSnap mirrors snapshotFallbackLocked through the memoized lists:
// unmatched events with an empty provider may scan any provider's snapshots;
// ambiguous ids only ever use their own provider (and empty+ambiguous has
// no safe scope). provider must already be normalized.
func (s *statusResolver) fallbackSnap(provider, model string, tsUnix float64, providerOnly bool) *snapRow {
	switch {
	case provider == "" && providerOnly:
		return nil
	case provider != "":
		return snapAt(s.snapList(false, provider, model), tsUnix)
	default:
		return snapAt(s.snapList(true, provider, model), tsUnix)
	}
}

// resolveEvent mirrors priceFromBasisLocked exactly (same fields, same
// statuses) over memoized data instead of per-event queries. On an unpriced
// miss matchedModel/matchedProv stay empty so matchResultFor falls back to
// the raw event pair — the same contract priceFromBasisLocked follows.
func (s *statusResolver) resolveEvent(e eventRow) (effectivePrice, bool) {
	provider := strings.ToLower(strings.TrimSpace(e.provider)) // F3
	b := s.basisFor(provider, e.model)
	var out effectivePrice
	if b.override {
		out.row = b.ovrRow
		out.snapshot, out.matchedModel, out.matchedProv = "override", e.model, provider
		out.method, out.status, out.source = "override", "override", pricingSourceOverride
		return out, true
	}
	mr := b.mr
	if !mr.ok {
		if sp := s.fallbackSnap(provider, e.model, e.ts, mr.status == "ambiguous"); sp != nil {
			out.row = priceRow{in: sp.in, out: sp.outp, cr: sp.cr, cw: sp.cw, src: sp.source}
			out.snapshot = "historical"
			out.matchedModel, out.matchedProv = sp.model, sp.provider
			out.method = "snapshot-legacy"
			out.status = "historical"
			out.snapID = &sp.id
			out.source = sp.source
			return out, true
		}
		out.method, out.status, out.candidates = mr.method, mr.status, mr.candidates
		if out.method == "" {
			out.method = "unmatched"
			out.status = "unmatched"
		}
		return out, false
	}
	out.matchedModel, out.matchedProv, out.method, out.status = mr.model, mr.provider, mr.method, mr.status
	out.candidates = mr.candidates
	if mr.row.src == pricingSourceOverride {
		// An override-mirrored catalog row reports as an override, never as
		// "latest"/"historical" (same rule as priceFromBasisLocked).
		out.row = mr.row
		out.snapshot = "override"
		out.status = "override"
		out.source = pricingSourceOverride
		return out, true
	}
	if !s.recalc {
		if sp := snapAt(s.snapList(false, mr.provider, mr.model), e.ts); sp != nil {
			out.row = priceRow{in: sp.in, out: sp.outp, cr: sp.cr, cw: sp.cw, src: sp.source}
			out.snapshot = "historical"
			out.status = "historical"
			out.snapID = &sp.id
			out.source = sp.source
			return out, true
		}
	}
	out.row = mr.row
	out.snapshot = "latest"
	out.source = mr.row.src
	return out, true
}

// resolve is the pricingStatus flavour: identical resolution plus the
// report-only detail of echoing the live match pair (normalized provider)
// on a miss, which pricingStatus has emitted since the F2 rework.
func (s *statusResolver) resolve(e eventRow) (effectivePrice, bool) {
	ep, ok := s.resolveEvent(e)
	if !ok {
		if mr := s.basisFor(strings.ToLower(strings.TrimSpace(e.provider)), e.model).mr; !mr.ok {
			ep.matchedModel, ep.matchedProv = mr.model, mr.provider
		}
	}
	return ep, ok
}

// pricingStatus implements GET /api/pricing/status (contract E + B2:
// ambiguous_models surfaced separately from unmatched_models; coverage is
// computed at priceFor-equivalent level so it agrees with analyticsCost).
func pricingStatus() ([]byte, error) {
	ensureDB()
	maybeAutoSync()
	maybeFxSync()
	events, _, _ := loadEventsRange(0, 8.8e9, "", "")
	authSet := map[string]bool{}
	type pairState struct {
		matched      bool // some event resolved to a price (incl. partial)
		sawUnmatched bool // some event unpriced (non-ambiguous)
		sawAmbiguous bool // some event ambiguous
		recPriced    bool // the recorded model_matches entry is resolved
	}
	pairSt := map[string]*pairState{}
	unmatchedSet := map[string]map[string]any{}
	ambiguousSet := map[string]map[string]any{}
	matchedEvents := 0
	mu.Lock()
	res := newStatusResolver(false)
	for _, e := range events {
		if e.authType != "" {
			authSet[e.authType] = true
		}
		k := e.provider + "\x00" + e.model
		st := pairSt[k]
		if st == nil {
			st = &pairState{}
			pairSt[k] = st
		}
		ep, priced := res.resolve(e)
		c := computeCost(e, ep, priced)
		mr := matchResultFor(e, ep, priced)
		if !st.recPriced {
			// Record the best resolution seen so far: an unpriced verdict is
			// upgraded the first time an event of the pair resolves.
			recordMatch(e.provider, e.model, mr)
			st.recPriced = priced
		}
		switch {
		case c.matched:
			st.matched = true
			if c.complete {
				matchedEvents++
			}
		case c.status == "ambiguous":
			st.sawAmbiguous = true
		default:
			st.sawUnmatched = true
		}
		if st.sawAmbiguous {
			ambiguousSet[k] = map[string]any{"model": e.model, "provider": e.provider, "status": "ambiguous"}
		}
		if st.sawUnmatched {
			unmatchedSet[k] = map[string]any{"model": e.model, "provider": e.provider, "status": "unmatched"}
		}
	}
	matchedPairs := 0
	for _, st := range pairSt {
		if st.matched {
			matchedPairs++
		}
	}
	unmatched := make([]map[string]any, 0, len(unmatchedSet))
	for _, v := range unmatchedSet {
		unmatched = append(unmatched, v)
	}
	ambiguous := make([]map[string]any, 0, len(ambiguousSet))
	for _, v := range ambiguousSet {
		ambiguous = append(ambiguous, v)
	}
	sort.Slice(unmatched, func(i, j int) bool {
		if unmatched[i]["model"] != unmatched[j]["model"] {
			return unmatched[i]["model"].(string) < unmatched[j]["model"].(string)
		}
		return unmatched[i]["provider"].(string) < unmatched[j]["provider"].(string)
	})
	sort.Slice(ambiguous, func(i, j int) bool {
		if ambiguous[i]["model"] != ambiguous[j]["model"] {
			return ambiguous[i]["model"].(string) < ambiguous[j]["model"].(string)
		}
		return ambiguous[i]["provider"].(string) < ambiguous[j]["provider"].(string)
	})
	total := len(events)
	coverage := 0.0
	if total > 0 {
		coverage = float64(matchedEvents) / float64(total) * 100
	}
	var lastSync, lastSource, lastError any
	if v := metaGet("pricing_last_sync"); v != nil && *v != "" {
		lastSync = *v
	}
	if v := metaGet("pricing_last_source"); v != nil && *v != "" {
		lastSource = *v
	}
	if v := metaGet("pricing_last_error"); v != nil && *v != "" {
		lastError = *v
	}
	syncing := pricingSyncing
	var catN int
	if db != nil {
		_ = db.QueryRow("SELECT COUNT(*) FROM price_catalog").Scan(&catN)
	}
	mu.Unlock()
	// Contract B.4: emit the per-model match table. Keys are (provider,model)
	// pairs internally; the response maps each event model to its match —
	// sorted iteration keeps the winner deterministic on name collisions.
	pricingMu.Lock()
	mKeys := make([]string, 0, len(pricingMatches))
	for k := range pricingMatches {
		mKeys = append(mKeys, k)
	}
	sort.Strings(mKeys)
	modelMatches := map[string]any{}
	for _, k := range mKeys {
		_, m := splitKey(k)
		modelMatches[m] = pricingMatches[k]
	}
	pricingMu.Unlock()
	resp := map[string]any{
		"enabled":          pricingAutoSyncEnabled(),
		"last_sync":        lastSync,
		"last_source":      lastSource,
		"last_error":       lastError,
		"syncing":          syncing,
		"catalog_count":    catN,
		"matched_count":    matchedPairs,
		"unmatched_models": unmatched,
		"ambiguous_models": ambiguous,
		"coverage_pct":     coverage,
		"model_matches":    modelMatches,
		"source_used":      lastSource,
		"auth_type":        authTypeSummary(authSet),
		"fx":               fxInfo(),
	}
	return json.Marshal(resp)
}

// pricingModels implements GET /api/pricing/models (contract E).
func pricingModels() ([]byte, error) {
	ensureDB()
	mu.Lock()
	tabs := catalogStateLocked()
	type row struct {
		model, provider, source string
		r                       priceRow
	}
	var list []row
	for k, r := range tabs.cat {
		p, m := splitKey(k)
		list = append(list, row{m, p, r.src, r})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].provider != list[j].provider {
			return list[i].provider < list[j].provider
		}
		return list[i].model < list[j].model
	})
	mu.Unlock()
	events, _, _ := loadEventsRange(0, 8.8e9, "", "")
	usageByPair := map[string]int{}
	methodOfKey := map[string]string{}
	matchedKeyOfEvent := map[string]string{}
	for _, e := range events {
		evKey := e.provider + "\x00" + e.model
		usageByPair[evKey]++
		mr := matchPrice(e.provider, e.model)
		if mr.ok {
			catKey := mr.provider + "\x00" + mr.model
			methodOfKey[catKey] = mr.method
			matchedKeyOfEvent[evKey] = catKey
		}
	}
	usageByRow := map[string]int{}
	for evKey, catKey := range matchedKeyOfEvent {
		usageByRow[catKey] += usageByPair[evKey]
	}
	out := []map[string]any{}
	for _, r := range list {
		k := r.provider + "\x00" + r.model
		out = append(out, map[string]any{
			"model":          r.model,
			"provider":       r.provider,
			"source":         r.source,
			"prices":         r.r.mapPrices(),
			"matched_by":     nilIfEmpty(methodOfKey[k]),
			"usage_requests": usageByRow[k],
		})
	}
	return json.Marshal(map[string]any{"models": out})
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// errInvalidPrice marks override payloads whose price fields are
// non-numeric, negative, NaN or ±Inf (contract B2: 400 {ok:false,
// error:'invalid_price'}). Fields absent or explicitly null are skipped.
var errInvalidPrice = errors.New("invalid_price")

// pricingOverrideEndpoint implements POST /api/pricing/override (contract E).
// delete:true runs before validation; upsert price fields are validated
// BEFORE any write (an invalid field aborts the whole request, no partial
// save); null clears/skips that field only.
func pricingOverrideEndpoint(body any) ([]byte, error) {
	raw, err := normalizeBody(body)
	if err != nil {
		return nil, fmt.Errorf("invalid body")
	}
	var req struct {
		Model      string          `json:"model"`
		Provider   string          `json:"provider"`
		Input      json.RawMessage `json:"input"`
		Output     json.RawMessage `json:"output"`
		CacheRead  json.RawMessage `json:"cache_read"`
		CacheWrite json.RawMessage `json:"cache_write"`
		Delete     bool            `json:"delete"`
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("body required")
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("invalid body: %v", err)
	}
	req.Model = strings.TrimSpace(req.Model)
	if req.Model == "" {
		return nil, fmt.Errorf("model required")
	}
	req.Provider = strings.ToLower(strings.TrimSpace(req.Provider)) // F3:
	// providers normalize to lowercase ids so "OpenAI" and "openai" share
	// the same override row.
	ensureDB()
	mu.Lock()
	defer mu.Unlock()
	if db == nil {
		return nil, fmt.Errorf("db unavailable")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if req.Delete {
		// F7d: delete runs BEFORE price validation — a delete payload must
		// never 400 on a garbage price field it is about to ignore anyway.
		if _, err := db.Exec("DELETE FROM price_overrides WHERE provider=? AND model=?", req.Provider, req.Model); err != nil {
			return nil, err
		}
		if _, err := db.Exec("DELETE FROM price_catalog WHERE provider=? AND model=? AND source='override'", req.Provider, req.Model); err != nil {
			return nil, err
		}
		// The override mirror shared the (provider,model) PK, so the external
		// row was never re-inserted by syncs — restore it from the newest
		// snapshot when one exists; the next sync heals it otherwise.
		if _, err := db.Exec(`INSERT INTO price_catalog
			(model, provider, source, input_per_m, output_per_m, cache_read_per_m, cache_write_per_m, currency, synced_at)
			SELECT model, provider, source, input_per_m, output_per_m, cache_read_per_m, cache_write_per_m, 'USD', effective_from
			FROM price_snapshots WHERE provider=? AND model=?
			ORDER BY effective_from DESC LIMIT 1`, req.Provider, req.Model); err != nil {
			return nil, err
		}
		pricingTablesCache.Store(nil) // catalog+overrides changed
		return json.Marshal(map[string]any{"ok": true})
	}
	// Price validation applies to the upsert path only.
	parseField := func(raw json.RawMessage) (*float64, error) {
		v, present, bad := priceField(raw)
		if !present {
			return nil, nil
		}
		if bad {
			return nil, errInvalidPrice
		}
		return v, nil
	}
	var perr error
	reqIn, err := parseField(req.Input)
	if err != nil {
		perr = err
	}
	reqOut, err := parseField(req.Output)
	if err != nil {
		perr = err
	}
	reqCR, err := parseField(req.CacheRead)
	if err != nil {
		perr = err
	}
	reqCW, err := parseField(req.CacheWrite)
	if err != nil {
		perr = err
	}
	if perr != nil {
		return nil, perr
	}
	if reqIn == nil && reqOut == nil && reqCR == nil && reqCW == nil {
		return nil, fmt.Errorf("at least one price field required")
	}
	if _, err := db.Exec(`INSERT OR REPLACE INTO price_overrides
		(provider, model, input_per_m, output_per_m, cache_read_per_m, cache_write_per_m, updated_at)
		VALUES (?,?,?,?,?,?,?)`,
		req.Provider, req.Model, ptr(reqIn), ptr(reqOut), ptr(reqCR), ptr(reqCW), now); err != nil {
		return nil, err
	}
	// Mirror into the catalog so matching and list endpoints see it.
	if _, err := db.Exec(`INSERT OR REPLACE INTO price_catalog
		(model, provider, source, input_per_m, output_per_m, cache_read_per_m, cache_write_per_m, currency, synced_at)
		VALUES (?,?,'override',?,?,?,?,'USD',?)`,
		req.Model, req.Provider, ptr(reqIn), ptr(reqOut), ptr(reqCR), ptr(reqCW), now); err != nil {
		return nil, err
	}
	pricingTablesCache.Store(nil) // catalog+overrides changed
	return json.Marshal(map[string]any{"ok": true})
}

// pricingSettingsEndpoint implements POST /api/pricing/settings (contract E).
func pricingSettingsEndpoint(body any) ([]byte, error) {
	raw, err := normalizeBody(body)
	if err != nil {
		return nil, fmt.Errorf("invalid body")
	}
	var req struct {
		AutoSync *bool           `json:"auto_sync"`
		Fx       json.RawMessage `json:"fx_usd_vnd"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, fmt.Errorf("invalid body: %v", err)
		}
	}
	ensureDB()
	mu.Lock()
	defer mu.Unlock()
	if db == nil {
		return nil, fmt.Errorf("db unavailable")
	}
	if req.AutoSync != nil {
		if *req.AutoSync {
			metaSet("pricing_auto_sync", "1")
		} else {
			metaSet("pricing_auto_sync", "0")
		}
	}
	if req.Fx != nil { // key present (incl. explicit null → delete)
		s := strings.TrimSpace(string(req.Fx))
		if s == "null" || s == "" {
			// Legacy clear: drop the manual rate only; fx_mode is left as
			// it is (auto keeps auto, manual just loses its rate).
			metaDel("fx_usd_vnd")
			metaDel("fx_usd_vnd_updated_at")
		} else {
			var rate float64
			if err := json.Unmarshal(req.Fx, &rate); err != nil ||
				math.IsNaN(rate) || math.IsInf(rate, 0) || rate <= 0 {
				return nil, fmt.Errorf("fx_usd_vnd must be a positive number or null")
			}
			// Writing a manual rate through the legacy endpoint implies
			// manual mode — the operator is overriding the fetched rate.
			metaSet("fx_usd_vnd", fmt.Sprintf("%g", rate))
			metaSet("fx_usd_vnd_updated_at", time.Now().UTC().Format(time.RFC3339))
			metaSet("fx_mode", fxModeManual)
		}
	}
	return json.Marshal(map[string]any{"ok": true})
}

// ---- GET /api/analytics/cost (contract D.5 + B2 stable estimates) ----

func analyticsCost(q map[string][]string) ([]byte, error) {
	ensureDB()
	maybeAutoSync()
	maybeFxSync()
	recalc := false
	if v := q["recalc"]; len(v) > 0 && (v[0] == "1" || v[0] == "true") {
		recalc = true
	}
	events, _, err := loadEvents(q)
	if err != nil {
		return nil, err
	}
	costs, mrs := resolveEstimates(events, recalc)
	now := time.Now()
	var pricedUSD float64
	var compIn, compOut, compCR, compCW float64
	var pricedN, unpricedN int64
	type modelAgg struct {
		usd       float64
		requests  int64
		priced    int64
		partial   bool
		unpriced  bool
		ambiguous bool
	}
	perModel := map[string]*modelAgg{}
	perKey := map[string]*modelAgg{}
	type dayAgg struct {
		usd      float64
		unpriced int64
	}
	perDay := map[string]*dayAgg{}
	unmatchedSet := map[string]map[string]any{}
	ambiguousSet := map[string]map[string]any{}
	authSet := map[string]bool{}
	for i, e := range events {
		if e.authType != "" {
			authSet[e.authType] = true
		}
		c := costs[i]
		recordMatch(e.provider, e.model, mrs[i])
		if c.matched {
			pricedUSD += c.usd
			compIn += c.inCost
			compOut += c.outCost
			compCR += c.crCost
			compCW += c.cwCost
			// Stored estimates keep counting their USD under stored
			// semantics, but the pair is still listed by its CURRENT catalog
			// match status (matchFor overlays the live status onto stored
			// rows): ambiguous → ambiguous_models, unmatched →
			// unmatched_models — the same buckets model_matches already
			// reports via mrs[i].status.
			switch mrs[i].status {
			case "ambiguous":
				ambiguousSet[e.provider+"\x00"+e.model] = map[string]any{"model": e.model, "provider": e.provider, "status": "ambiguous"}
			case "unmatched":
				unmatchedSet[e.provider+"\x00"+e.model] = map[string]any{"model": e.model, "provider": e.provider, "status": "unmatched"}
			}
		} else if c.status == "ambiguous" {
			ambiguousSet[e.provider+"\x00"+e.model] = map[string]any{"model": e.model, "provider": e.provider, "status": "ambiguous"}
		} else {
			unmatchedSet[e.provider+"\x00"+e.model] = map[string]any{"model": e.model, "provider": e.provider, "status": "unmatched"}
		}
		if c.matched && c.complete {
			pricedN++
		} else {
			unpricedN++
		}
		ma := perModel[e.model]
		if ma == nil {
			ma = &modelAgg{}
			perModel[e.model] = ma
		}
		ma.requests++
		ma.usd += c.usd
		if c.matched && c.complete {
			ma.priced++
		} else if c.matched {
			ma.partial = true
		} else if c.status == "ambiguous" {
			ma.ambiguous = true
		} else {
			ma.unpriced = true
		}
		ka := perKey[e.key]
		if ka == nil {
			ka = &modelAgg{}
			perKey[e.key] = ka
		}
		ka.requests++
		ka.usd += c.usd
		if c.matched && c.complete {
			ka.priced++
		}
		label := bucketLabel(time.Unix(0, int64(e.ts*1e9)), "day")
		da := perDay[label]
		if da == nil {
			da = &dayAgg{}
			perDay[label] = da
		}
		da.usd += c.usd
		if !c.matched || !c.complete {
			da.unpriced++
		}
	}
	modelNames := make([]string, 0, len(perModel))
	for m := range perModel {
		modelNames = append(modelNames, m)
	}
	sort.Slice(modelNames, func(i, j int) bool {
		a, b := perModel[modelNames[i]], perModel[modelNames[j]]
		if a.usd != b.usd {
			return a.usd > b.usd
		}
		if a.requests != b.requests {
			return a.requests > b.requests
		}
		return modelNames[i] < modelNames[j]
	})
	perModelOut := []map[string]any{}
	for _, m := range modelNames {
		a := perModel[m]
		st := "priced"
		if a.unpriced {
			st = "unpriced"
		} else if a.ambiguous {
			st = "ambiguous"
		} else if a.partial {
			st = "partial"
		}
		perModelOut = append(perModelOut, map[string]any{
			"model": m, "usd": a.usd, "requests": a.requests,
			"priced": a.priced, "price_status": st,
		})
	}
	keyNames := make([]string, 0, len(perKey))
	for k := range perKey {
		keyNames = append(keyNames, k)
	}
	sort.Slice(keyNames, func(i, j int) bool {
		a, b := perKey[keyNames[i]], perKey[keyNames[j]]
		if a.usd != b.usd {
			return a.usd > b.usd
		}
		return keyNames[i] < keyNames[j]
	})
	perKeyOut := []map[string]any{}
	for _, k := range keyNames {
		a := perKey[k]
		perKeyOut = append(perKeyOut, map[string]any{"key": k, "usd": a.usd, "requests": a.requests, "priced": a.priced})
	}
	days := make([]string, 0, len(perDay))
	for d := range perDay {
		days = append(days, d)
	}
	sort.Strings(days)
	daily := []map[string]any{}
	for _, d := range days {
		daily = append(daily, map[string]any{"label": d, "usd": perDay[d].usd, "unpriced_requests": perDay[d].unpriced})
	}
	unmatched := []map[string]any{}
	for _, v := range unmatchedSet {
		unmatched = append(unmatched, v)
	}
	sort.Slice(unmatched, func(i, j int) bool {
		if unmatched[i]["model"] != unmatched[j]["model"] {
			return unmatched[i]["model"].(string) < unmatched[j]["model"].(string)
		}
		return unmatched[i]["provider"].(string) < unmatched[j]["provider"].(string)
	})
	ambiguous := []map[string]any{}
	for _, v := range ambiguousSet {
		ambiguous = append(ambiguous, v)
	}
	sort.Slice(ambiguous, func(i, j int) bool {
		if ambiguous[i]["model"] != ambiguous[j]["model"] {
			return ambiguous[i]["model"].(string) < ambiguous[j]["model"].(string)
		}
		return ambiguous[i]["provider"].(string) < ambiguous[j]["provider"].(string)
	})
	total := pricedN + unpricedN
	coverage := 0.0
	if total > 0 {
		coverage = float64(pricedN) / float64(total) * 100
	}
	snapshot := "historical"
	estimateBasis := "stored"
	if recalc {
		snapshot = "latest"
		estimateBasis = "latest"
	}
	resp := map[string]any{
		"currency": "USD",
		"totals": map[string]any{
			"priced_usd":      pricedUSD,
			"unpriced_events": unpricedN,
			"priced_events":   pricedN,
			"by_component":    map[string]any{"input": compIn, "output": compOut, "cache_read": compCR, "cache_write": compCW},
		},
		"today_usd":        nil, // overwritten by analyticsCostResponse's calendar blocks
		"month_usd":        nil, // (contract B2: independent today/month ranges)
		"per_model":        perModelOut,
		"per_key":          perKeyOut,
		"daily":            daily,
		"coverage":         map[string]any{"pct": coverage, "priced_requests": pricedN, "total_requests": total},
		"unmatched_models": unmatched,
		"ambiguous_models": ambiguous,
		"auth_type":        authTypeSummary(authSet),
		"billing_note":     "api_equivalent",
		"snapshot":         snapshot,
		"estimate_basis":   estimateBasis,
		"fx":               fxInfo(),
		"timezone":         now.Format("MST"),
	}
	return json.Marshal(resp)
}
