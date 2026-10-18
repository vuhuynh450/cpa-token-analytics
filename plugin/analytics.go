package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// costForEvent returns the USD cost of a normalized event. It stays nil in
// this lane; the pricing lane (BE2) installs a hook and any code path that
// consumes cost must treat "hook unset" or ok=false as unpriced.
var costForEvent func(e eventRow) (usd float64, ok bool)

// ---- migrations ----

// runMigrations applies the additive, idempotent schema upgrades on a
// freshly opened connection. It tolerates partially-migrated databases:
// every statement is safe to re-run and the ts_unix backfill only touches
// rows where it is still NULL (sentinel -1 marks already-decided junk).
func runMigrations(conn *sql.DB) error {
	if _, err := conn.Exec("ALTER TABLE usage_events ADD COLUMN ts_unix REAL"); err != nil {
		if !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	stmts := []string{
		"CREATE INDEX IF NOT EXISTS idx_usage_events_ts_unix ON usage_events(ts_unix)",
		`CREATE TABLE IF NOT EXISTS plugin_meta (k TEXT PRIMARY KEY, v TEXT)`,
		`CREATE TABLE IF NOT EXISTS price_catalog (
			model TEXT NOT NULL, provider TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL,
			input_per_m REAL, output_per_m REAL, cache_read_per_m REAL, cache_write_per_m REAL,
			currency TEXT NOT NULL DEFAULT 'USD',
			synced_at TEXT NOT NULL,
			PRIMARY KEY (provider, model))`,
		`CREATE TABLE IF NOT EXISTS price_snapshots (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			model TEXT NOT NULL, provider TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL, input_per_m REAL, output_per_m REAL,
			cache_read_per_m REAL, cache_write_per_m REAL,
			effective_from TEXT NOT NULL,
			UNIQUE(provider, model, effective_from))`,
		`CREATE TABLE IF NOT EXISTS price_overrides (
			provider TEXT NOT NULL DEFAULT '', model TEXT NOT NULL,
			input_per_m REAL, output_per_m REAL, cache_read_per_m REAL, cache_write_per_m REAL,
			updated_at TEXT NOT NULL, PRIMARY KEY (provider, model))`,
		`CREATE TABLE IF NOT EXISTS cost_estimates (
			event_id INTEGER PRIMARY KEY REFERENCES usage_events(id),
			model TEXT NOT NULL, provider TEXT NOT NULL DEFAULT '',
			matched_model TEXT, matched_provider TEXT,
			source TEXT, snapshot_id INTEGER,
			usd REAL, input_usd REAL, output_usd REAL, cache_read_usd REAL, cache_write_usd REAL,
			status TEXT NOT NULL,
			calculated_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS fx_state (
			pair TEXT PRIMARY KEY,
			rate REAL, source TEXT,
			provider_updated_at TEXT, fetched_at TEXT,
			last_success_at TEXT, last_attempt_at TEXT,
			last_error TEXT)`,
	}
	for _, s := range stmts {
		if _, err := conn.Exec(s); err != nil {
			return err
		}
	}
	if err := seedFxMode(conn); err != nil {
		return err
	}
	if err := seedDatasetID(conn); err != nil {
		return err
	}
	return backfillTsUnix(conn)
}

// backfillTsUnix fills ts_unix in batches using the same parseAnyTime
// precedence as reads: payload.timestamp first, stored ts text as fallback.
// Per contract B2/R3 every scanned row leaves ts_unix IS NULL in exactly one
// call: parseable → unix seconds; unparseable/empty/malformed → -1 sentinel
// (excluded from every range and never rescanned). The loop terminates
// because each batch either writes ≥1 row (NULL set shrinks) or proves no
// progress is possible (empty/scan-failed batch → done).
func backfillTsUnix(conn *sql.DB) error {
	const batch = 500
	for {
		rows, err := conn.Query("SELECT id, ts, payload FROM usage_events WHERE ts_unix IS NULL ORDER BY id LIMIT ?", batch)
		if err != nil {
			return err
		}
		type pending struct {
			id int64
			ts float64
		}
		upd := make([]pending, 0, batch)
		for rows.Next() {
			var id int64
			var storedTs string
			var payload sql.NullString
			if err := rows.Scan(&id, &storedTs, &payload); err != nil {
				continue // id failed to scan → cannot target an UPDATE anyway
			}
			tsStr := storedTs
			var e dbEvent
			if err := json.Unmarshal([]byte(payload.String), &e); err == nil && e.Timestamp != "" {
				tsStr = e.Timestamp
			}
			ts, err := parseAnyTime(tsStr)
			if err != nil {
				ts = -1 // sentinel: unparseable/empty timestamps leave NULL once
			}
			upd = append(upd, pending{id: id, ts: ts})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(upd) == 0 {
			// Nothing was scanned: fully backfilled, or every remaining NULL
			// row failed to scan (a no-op commit would re-select them forever).
			return nil
		}
		tx, err := conn.Begin()
		if err != nil {
			return err
		}
		for _, u := range upd {
			if _, err := tx.Exec("UPDATE usage_events SET ts_unix = ? WHERE id = ?", u.ts, u.id); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
}

// ---- shared event loading ----

type eventRow struct {
	id         int64 // usage_events.id (0 for rows not read from the table)
	ts         float64
	model      string
	key        string
	input      int64
	output     int64
	cache      int64
	cacheWrite int64
	reasoning  int64
	failed     bool
	latency    float64
	ttft       float64
	statusCode int
	authType   string
	provider   string
}

// tsUnixIndexUsableLocked reports whether any row carries ts_unix, i.e. the
// indexed prefilter is meaningful. Caller must hold mu.
func tsUnixIndexUsableLocked() bool {
	var n int
	return db.QueryRow("SELECT EXISTS(SELECT 1 FROM usage_events WHERE ts_unix IS NOT NULL)").Scan(&n) == nil && n > 0
}

// queryUsageRows returns events in [lo, hi) unix seconds, ordered by id.
// When the ts_unix index is usable it prefilters through
// `id IN (range-scan UNION ALL is-null)`; the plain
// `(ts_unix>=? AND ts_unix<?) OR ts_unix IS NULL` form was verified via
// EXPLAIN QUERY PLAN to degrade to a full table SCAN (SQLite does not apply
// the multi-index OR optimization under an ORDER BY the index cannot serve),
// while the UNION ALL form keeps a covering-index range scan plus a
// covering-index NULL probe (ts_unix=?) and the outer rowid lookups still
// come back under the explicit ORDER BY id. The two sets are disjoint on
// `ts_unix IS NULL`, so dedup is impossible. Rows at sentinel ts_unix=-1
// match neither branch. NULL-ts_unix rows (not yet processed by the
// backfill) still reach the caller, where the payload-parse + range check
// acts as safety net; otherwise it runs the legacy full scan. Caller holds mu.
func queryUsageRows(lo, hi float64) (*sql.Rows, error) {
	if db == nil {
		return nil, nil
	}
	if tsUnixIndexUsableLocked() {
		return db.Query(`SELECT id, ts, payload FROM usage_events WHERE id IN (
			SELECT id FROM usage_events WHERE ts_unix >= ? AND ts_unix < ?
			UNION ALL
			SELECT id FROM usage_events WHERE ts_unix IS NULL
		) ORDER BY id`, lo, hi)
	}
	return db.Query("SELECT id, ts, payload FROM usage_events ORDER BY id")
}

// countUsageRowsLocked estimates the row count queryUsageRows would produce
// — used only as an allocation hint, so NULL rows may inflate it slightly.
// Caller must hold mu.
func countUsageRowsLocked(lo, hi float64) int {
	if db == nil || !tsUnixIndexUsableLocked() {
		return 0
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM usage_events WHERE id IN (
		SELECT id FROM usage_events WHERE ts_unix >= ? AND ts_unix < ?
		UNION ALL
		SELECT id FROM usage_events WHERE ts_unix IS NULL
	)`, lo, hi).Scan(&n); err != nil {
		return 0
	}
	return n
}

// normalizeRow applies the same token/model/key normalization as the usage
// endpoint to one decoded payload. ok=false means the row is unusable.
func normalizeRow(e dbEvent, storedTs string) (row eventRow, ok bool) {
	tsStr := e.Timestamp
	if tsStr == "" {
		tsStr = storedTs
	}
	ts, err := parseAnyTime(tsStr)
	if err != nil {
		return row, false
	}
	row.ts = ts
	row.model = e.Model
	if row.model == "" {
		row.model = e.Alias
	}
	if row.model == "" {
		row.model = "unknown"
	}
	source := e.Source
	if source == "" {
		source = e.APIKey
	}
	row.key = anonKey(source)
	if t := e.Tokens; t != nil {
		row.input = toI64(t["input_tokens"])
		row.output = toI64(t["output_tokens"])
		read := toI64(t["cache_read_tokens"])
		cached := toI64(t["cached_tokens"])
		creation := toI64(t["cache_creation_tokens"])
		row.cache = read
		if row.cache == 0 && cached > 0 && creation == 0 {
			row.cache = cached
		}
		row.cacheWrite = creation
		row.reasoning = toI64(t["reasoning_tokens"])
		if addInputCache(e, row.input, row.output, row.cache, creation, row.reasoning) {
			row.input += row.cache + creation
		}
	}
	row.failed = e.Failed
	row.latency = e.LatencyMs
	row.ttft = e.TTFTMs
	row.statusCode = e.StatusCode
	row.authType = e.AuthType
	row.provider = e.Provider
	return row, true
}

// authTypeSummary collapses the distinct auth_type values seen on events
// into the top-level contract: no value → "unknown", one → that value,
// more than one → "mixed".
func authTypeSummary(set map[string]bool) string {
	switch len(set) {
	case 0:
		return "unknown"
	case 1:
		for v := range set {
			return v
		}
	}
	return "mixed"
}

func bucketLabel(t time.Time, bucket string) string {
	switch bucket {
	case "hour":
		return t.Format("2006-01-02 15") + ":00"
	case "month":
		return t.Format("2006-01")
	default:
		return t.Format("2006-01-02")
	}
}

// analyticsRange resolves start/end query params into half-open unix bounds
// [lo, hi) in local time; mirrors the /api/usage defaults (last 7 days).
func analyticsRange(q map[string][]string) (float64, float64, error) {
	get := func(k string) string {
		if v, ok := q[k]; ok && len(v) > 0 {
			return v[0]
		}
		return ""
	}
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	var a, b time.Time
	if s := get("start"); s != "" {
		var err error
		a, err = time.ParseInLocation("2006-01-02", s, time.Local)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid start date")
		}
	} else {
		a = today.AddDate(0, 0, -6)
	}
	if s := get("end"); s != "" {
		var err error
		b, err = time.ParseInLocation("2006-01-02", s, time.Local)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid end date")
		}
	} else {
		b = today
	}
	if a.After(b) {
		return 0, 0, fmt.Errorf("start date must not be after end date")
	}
	return float64(a.Unix()), float64(b.AddDate(0, 0, 1).Unix()), nil
}

// loadEvents returns normalized events in the requested range, honoring the
// optional model/key filters. skipped counts rows dropped by parsing.
func loadEvents(q map[string][]string) ([]eventRow, int, error) {
	lo, hi, err := analyticsRange(q)
	if err != nil {
		return nil, 0, err
	}
	model, key := "", ""
	if v := q["model"]; len(v) > 0 {
		model = v[0]
	}
	if v := q["key"]; len(v) > 0 {
		key = v[0]
	}
	return loadEventsRange(lo, hi, model, key)
}

func loadEventsRange(lo, hi float64, model, key string) ([]eventRow, int, error) {
	ensureDB()
	mu.Lock()
	var rows *sql.Rows
	var err error
	hint := 0
	if db != nil {
		// Exact allocation hint up front: the covering-index COUNT is cheap
		// relative to the realloc+memcpy chain a large result would pay.
		hint = countUsageRowsLocked(lo, hi)
		rows, err = queryUsageRows(lo, hi)
	}
	mu.Unlock()
	if err != nil {
		return nil, 0, err
	}
	events := make([]eventRow, 0, hint)
	skipped := 0
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var id int64
			var storedTs, payload string
			if err := rows.Scan(&id, &storedTs, &payload); err != nil {
				skipped++ // same drop class as unmarshal/normalize failures below
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
			if model != "" && row.model != model {
				continue
			}
			if key != "" && row.key != key {
				continue
			}
			events = append(events, row)
		}
		if err := rows.Err(); err != nil {
			return nil, skipped, err
		}
	}
	return events, skipped, nil
}

// ---- stats ----

func fptr(v float64) *float64 { return &v }

// nearestRank returns the nearest-rank percentile of sorted values.
func nearestRank(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// summarizeDist computes avg/p50/p95 over valid (>0) samples; n is the
// sample count. All stats are null when no valid sample exists.
func summarizeDist(vals []float64) map[string]any {
	valid := make([]float64, 0, len(vals))
	for _, v := range vals {
		if v > 0 {
			valid = append(valid, v)
		}
	}
	sort.Float64s(valid)
	res := map[string]any{"avg": nil, "p50": nil, "p95": nil, "n": len(valid)}
	if len(valid) == 0 {
		return res
	}
	sum := 0.0
	for _, v := range valid {
		sum += v
	}
	res["avg"] = sum / float64(len(valid))
	res["p50"] = nearestRank(valid, 50)
	res["p95"] = nearestRank(valid, 95)
	return res
}

// tpsAvgOrNil renders the tok_per_sec avg: nil until a valid sample exists.
func tpsAvgOrNil(sum float64, n int) any {
	if n > 0 {
		return sum / float64(n)
	}
	return nil
}

func eventCost(e eventRow) (float64, bool) {
	if costForEvent == nil {
		return 0, false
	}
	return costForEvent(e)
}

// sumCosts resolves per-event estimates (contract B2: stored cost_estimates
// first, compute+persist when missing) and sums every matched event —
// including partial rows, which carry the USD of their priced components —
// mirroring totals.priced_usd, heatmap cost_usd and costRangeBlock.usd.
// ok reports "any event resolved to a price", so a period that only has
// partial-priced events still surfaces cost_usd instead of null.
func sumCosts(events []eventRow) (float64, bool) {
	infos, _ := resolveEstimates(events, false)
	total := 0.0
	ok := false
	for _, c := range infos {
		if c.matched {
			total += c.usd
			ok = true
		}
	}
	return total, ok
}

// ---- management dispatch ----

// handleAnalytics routes /api/analytics/* management requests by suffix.
func handleAnalytics(path string, q map[string][]string) ([]byte, error) {
	name := strings.TrimSuffix(strings.SplitN(path[strings.Index(path, "/api/analytics/")+len("/api/analytics/"):], "?", 2)[0], "/")
	var body []byte
	var err error
	switch name {
	case "overview":
		body, err = analyticsOverview(q)
	case "performance":
		body, err = analyticsPerformance(q)
	case "comparison":
		body, err = analyticsComparison(q)
	case "heatmap":
		body, err = analyticsHeatmap(q)
	case "cost":
		body, err = analyticsCostResponse(q)
	default:
		return managementResponse(404, "text/plain; charset=utf-8", []byte("not found"))
	}
	if err != nil {
		b, _ := json.Marshal(map[string]interface{}{"error": err.Error()})
		return managementResponse(400, "application/json; charset=utf-8", b)
	}
	return managementResponse(200, "application/json; charset=utf-8", body)
}

// ---- GET /api/analytics/overview ----

func analyticsOverview(q map[string][]string) ([]byte, error) {
	events, skipped, err := loadEvents(q)
	if err != nil {
		return nil, err
	}
	var input, output, cache, cacheWrite, reasoning int64
	var failed int64
	latVals := make([]float64, 0, len(events))
	ttftVals := make([]float64, 0, len(events))
	authSet := map[string]bool{}
	tpsSum := 0.0
	tpsN := 0
	for _, e := range events {
		input += e.input
		output += e.output
		cache += e.cache
		cacheWrite += e.cacheWrite
		reasoning += e.reasoning
		if e.failed {
			failed++
		}
		latVals = append(latVals, e.latency)
		ttftVals = append(ttftVals, e.ttft)
		if e.latency > 0 && e.output > 0 {
			tpsSum += float64(e.output) / (e.latency / 1000)
			tpsN++
		}
		if e.authType != "" {
			authSet[e.authType] = true
		}
	}
	requests := int64(len(events))
	totals := map[string]any{
		"requests":       requests,
		"failed":         failed,
		"succeeded":      requests - failed,
		"input":          input,
		"output":         output,
		"cache":          cache,
		"cache_write":    cacheWrite,
		"reasoning":      reasoning,
		"total":          input + output,
		"success_rate":   nil,
		"cache_hit_rate": nil,
	}
	if requests > 0 {
		totals["success_rate"] = float64(requests-failed) / float64(requests)
	}
	if input > 0 {
		totals["cache_hit_rate"] = float64(cache) / float64(input)
	}
	lat := summarizeDist(latVals)
	ttft := summarizeDist(ttftVals)
	resp := map[string]any{
		"totals":      totals,
		"latency":     lat,
		"ttft":        ttft,
		"tok_per_sec": map[string]any{"avg": tpsAvgOrNil(tpsSum, tpsN), "n": tpsN},
		"coverage": map[string]any{
			"events_with_latency": lat["n"],
			"events_with_ttft":    ttft["n"],
		},
		"skipped":      skipped,
		"collected_at": collectedAt(),
		"timezone":     time.Now().Format("MST"),
		"auth_type":    authTypeSummary(authSet),
	}
	return json.Marshal(resp)
}

// ---- GET /api/analytics/performance ----

func analyticsPerformance(q map[string][]string) ([]byte, error) {
	get := func(k string) string {
		if v, ok := q[k]; ok && len(v) > 0 {
			return v[0]
		}
		return ""
	}
	bucket := get("bucket")
	if bucket == "" {
		bucket = "day"
	}
	if bucket != "hour" && bucket != "day" && bucket != "month" {
		return nil, fmt.Errorf("invalid bucket")
	}
	events, _, err := loadEvents(q)
	if err != nil {
		return nil, err
	}
	// Per-model aggregation keeps only scalars + distribution samples —
	// events are not retained a second time per model.
	type modelAgg struct {
		requests, failed, outSum int64
		latVals, ttftVals        []float64
		tpsSum                   float64
		tpsN                     int
	}
	perModel := map[string]*modelAgg{}
	type trendAgg struct {
		requests, failed  int64
		latVals, ttftVals []float64
	}
	perTrend := map[string]*trendAgg{}
	trendOrder := []string{}
	for _, e := range events {
		ma := perModel[e.model]
		if ma == nil {
			ma = &modelAgg{}
			perModel[e.model] = ma
		}
		ma.requests++
		if e.failed {
			ma.failed++
		}
		ma.outSum += e.output
		ma.latVals = append(ma.latVals, e.latency)
		ma.ttftVals = append(ma.ttftVals, e.ttft)
		if e.latency > 0 && e.output > 0 {
			ma.tpsSum += float64(e.output) / (e.latency / 1000)
			ma.tpsN++
		}
		label := bucketLabel(time.Unix(0, int64(e.ts*1e9)), bucket)
		ta := perTrend[label]
		if ta == nil {
			ta = &trendAgg{}
			perTrend[label] = ta
			trendOrder = append(trendOrder, label)
		}
		ta.requests++
		if e.failed {
			ta.failed++
		}
		ta.latVals = append(ta.latVals, e.latency)
		ta.ttftVals = append(ta.ttftVals, e.ttft)
	}
	models := make([]string, 0, len(perModel))
	for m := range perModel {
		models = append(models, m)
	}
	sort.Slice(models, func(i, j int) bool {
		a, b := perModel[models[i]], perModel[models[j]]
		if a.requests != b.requests {
			return a.requests > b.requests
		}
		return models[i] < models[j]
	})
	perModelOut := []map[string]any{}
	for _, m := range models {
		ma := perModel[m]
		entry := map[string]any{
			"model":        m,
			"requests":     ma.requests,
			"failed":       ma.failed,
			"success_rate": nil,
			"latency":      summarizeDist(ma.latVals),
			"ttft":         summarizeDist(ma.ttftVals),
			"avg_output":   float64(ma.outSum) / float64(ma.requests),
			"tok_per_sec":  map[string]any{"avg": tpsAvgOrNil(ma.tpsSum, ma.tpsN), "n": ma.tpsN},
		}
		if ma.requests > 0 {
			entry["success_rate"] = float64(ma.requests-ma.failed) / float64(ma.requests)
		}
		perModelOut = append(perModelOut, entry)
	}
	sort.Strings(trendOrder)
	trend := []map[string]any{}
	for _, label := range trendOrder {
		ta := perTrend[label]
		lat := summarizeDist(ta.latVals)
		ttft := summarizeDist(ta.ttftVals)
		trend = append(trend, map[string]any{
			"label":        label,
			"avg_latency":  lat["avg"],
			"p95_latency":  lat["p95"],
			"avg_ttft":     ttft["avg"],
			"failure_rate": float64(ta.failed) / float64(ta.requests),
			"requests":     ta.requests,
		})
	}
	return json.Marshal(map[string]any{
		"per_model": perModelOut,
		"trend":     trend,
		"timezone":  time.Now().Format("MST"),
	})
}

// ---- GET /api/analytics/comparison ----

type period struct {
	start time.Time
	end   time.Time // exclusive
	label string
}

func (p period) MarshalMap() map[string]any {
	return map[string]any{
		"start": p.start.Format(time.RFC3339),
		"end":   p.end.Format(time.RFC3339),
		"label": p.label,
	}
}

func rangeLabel(s, e time.Time) string {
	if e == s.AddDate(0, 0, 1) {
		return s.Format("2006-01-02")
	}
	return s.Format("2006-01-02") + ".." + e.Add(-time.Second).Format("2006-01-02")
}

// comparisonPeriods resolves preset+mode into current/previous half-open
// local ranges. mode=elapsed compares the elapsed portion: previous is
// start-aligned (starts at cs-span) and has length elapsed=min(now-cs, span)
// — e.g. preset=today at 09:00 yields prev [yesterday 00:00, yesterday 09:00).
// Once the current period has fully passed (elapsed >= span) previous is the
// complete preceding period, same as mode=full.
// preset=month is calendar-based (contract B2): previous is the previous
// calendar month — [prev-month-start, this-month-start) under mode=full, or
// [prev-month-start, prev-month-start+elapsed) capped at the month boundary
// under mode=elapsed (handles 28–31-day months, leap years, year rollover).
func comparisonPeriods(preset, mode, customStart, customEnd string, now time.Time) (period, period, error) {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	var cs, ce time.Time
	switch preset {
	case "today":
		cs, ce = today, today.AddDate(0, 0, 1)
	case "yesterday":
		cs, ce = today.AddDate(0, 0, -1), today
	case "7d":
		cs, ce = today.AddDate(0, 0, -6), today.AddDate(0, 0, 1)
	case "30d":
		cs, ce = today.AddDate(0, 0, -29), today.AddDate(0, 0, 1)
	case "month":
		cs, ce = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.Local), today.AddDate(0, 0, 1)
	case "custom":
		var err error
		cs, err = time.ParseInLocation("2006-01-02", customStart, time.Local)
		if err != nil {
			return period{}, period{}, fmt.Errorf("invalid custom_start")
		}
		e, err := time.ParseInLocation("2006-01-02", customEnd, time.Local)
		if err != nil {
			return period{}, period{}, fmt.Errorf("invalid custom_end")
		}
		ce = e.AddDate(0, 0, 1)
		if !ce.After(cs) {
			return period{}, period{}, fmt.Errorf("custom_end must not be before custom_start")
		}
	default:
		return period{}, period{}, fmt.Errorf("invalid preset")
	}
	span := ce.Sub(cs)
	var ps, pe time.Time
	if preset == "month" {
		// Calendar semantics: the previous period is the previous calendar
		// month, not span-seconds before the current start.
		ps = cs.AddDate(0, -1, 0)
		pe = cs
		if mode == "elapsed" {
			// elapsed is a real duration (now-cs); the window ends at
			// ps+elapsed, never past the previous month's end (= cs).
			if elapsed := now.Sub(cs); elapsed < cs.Sub(ps) {
				pe = ps.Add(max(elapsed, 0))
			}
		}
	} else {
		ps, pe = cs.Add(-span), cs
		if mode == "elapsed" {
			if elapsed := now.Sub(cs); elapsed < span {
				// elapsed < 0 (a future custom range) degenerates to an empty
				// previous window [ps, ps) — never a negative-length range.
				pe = ps.Add(max(elapsed, 0))
			}
		}
	}
	return period{cs, ce, rangeLabel(cs, ce)}, period{ps, pe, rangeLabel(ps, pe)}, nil
}

// comparisonMetrics aggregates one period into the contract metric keys.
// Values are float64; nil marks a null metric (undefined or unpriced).
func comparisonMetrics(events []eventRow) map[string]*float64 {
	var input, output, cache int64
	var failed int64
	latVals := []float64{}
	costSum, costOK := sumCosts(events)
	for _, e := range events {
		input += e.input
		output += e.output
		cache += e.cache
		latVals = append(latVals, e.latency)
		if e.failed {
			failed++
		}
	}
	reqs := int64(len(events))
	m := map[string]*float64{
		"total_tokens":   fptr(float64(input + output)),
		"input":          fptr(float64(input)),
		"output":         fptr(float64(output)),
		"cache":          fptr(float64(cache)),
		"requests":       fptr(float64(reqs)),
		"failed":         fptr(float64(failed)),
		"succeeded":      fptr(float64(reqs - failed)),
		"avg_latency":    nil,
		"success_rate":   nil,
		"cache_hit_rate": nil,
	}
	if v, ok := summarizeDist(latVals)["avg"].(float64); ok {
		m["avg_latency"] = fptr(v)
	}
	if reqs > 0 {
		m["success_rate"] = fptr(float64(reqs-failed) / float64(reqs))
	}
	if input > 0 {
		m["cache_hit_rate"] = fptr(float64(cache) / float64(input))
	}
	if costOK {
		m["cost_usd"] = fptr(costSum)
	}
	return m
}

func analyticsComparison(q map[string][]string) ([]byte, error) {
	get := func(k string) string {
		if v, ok := q[k]; ok && len(v) > 0 {
			return v[0]
		}
		return ""
	}
	preset := get("preset")
	if preset == "" {
		preset = "today"
	}
	mode := get("mode")
	if mode == "" {
		mode = "elapsed"
	}
	if mode != "elapsed" && mode != "full" {
		return nil, fmt.Errorf("invalid mode")
	}
	model, key := get("model"), get("key")
	cur, prev, err := comparisonPeriods(preset, mode, get("custom_start"), get("custom_end"), time.Now())
	if err != nil {
		return nil, err
	}
	curEvents, _, err := loadEventsRange(float64(cur.start.Unix()), float64(cur.end.Unix()), model, key)
	if err != nil {
		return nil, err
	}
	prevEvents, _, err := loadEventsRange(float64(prev.start.Unix()), float64(prev.end.Unix()), model, key)
	if err != nil {
		return nil, err
	}
	curM := comparisonMetrics(curEvents)
	prevM := comparisonMetrics(prevEvents)
	metrics := map[string]any{}
	for _, k := range []string{"total_tokens", "input", "output", "cache", "requests", "succeeded", "failed", "success_rate", "cost_usd", "avg_latency", "cache_hit_rate"} {
		c, p := curM[k], prevM[k]
		entry := map[string]any{"current": c, "previous": p, "delta": nil, "pct_change": nil}
		if c != nil && p != nil {
			entry["delta"] = *c - *p
			if *p != 0 {
				entry["pct_change"] = (*c - *p) / *p * 100
			}
		}
		metrics[k] = entry
	}
	return json.Marshal(map[string]any{
		"preset":   preset,
		"mode":     mode,
		"current":  cur.MarshalMap(),
		"previous": prev.MarshalMap(),
		"metrics":  metrics,
		"units": map[string]any{
			"total_tokens": "tokens", "input": "tokens", "output": "tokens", "cache": "tokens",
			"requests": "count", "succeeded": "count", "failed": "count",
			"success_rate": "ratio", "cache_hit_rate": "ratio",
			"cost_usd": "usd", "avg_latency": "ms",
			"delta": "same_as_metric", "pct_change": "percent",
		},
		"timezone": time.Now().Format("MST"),
	})
}

// ---- GET /api/analytics/heatmap ----

func analyticsHeatmap(q map[string][]string) ([]byte, error) {
	lo, hi, err := analyticsRange(q)
	if err != nil {
		return nil, err
	}
	model, key := "", ""
	if v := q["model"]; len(v) > 0 {
		model = v[0]
	}
	if v := q["key"]; len(v) > 0 {
		key = v[0]
	}
	events, _, err := loadEventsRange(lo, hi, model, key)
	if err != nil {
		return nil, err
	}
	requests := make([][]float64, 7)
	tokens := make([][]float64, 7)
	cost := make([][]float64, 7)
	for i := range requests {
		requests[i] = make([]float64, 24)
		tokens[i] = make([]float64, 24)
		cost[i] = make([]float64, 24)
	}
	mx := map[string]float64{"requests": 0, "tokens": 0, "cost_usd": 0}
	infos, _ := resolveEstimates(events, false)
	for i, e := range events {
		t := time.Unix(0, int64(e.ts*1e9)).Local()
		dow := (int(t.Weekday()) + 6) % 7 // Mon=0
		h := t.Hour()
		tok := float64(e.input + e.output)
		usd := infos[i].usd
		requests[dow][h]++
		tokens[dow][h] += tok
		cost[dow][h] += usd
		if requests[dow][h] > mx["requests"] {
			mx["requests"] = requests[dow][h]
		}
		if tokens[dow][h] > mx["tokens"] {
			mx["tokens"] = tokens[dow][h]
		}
		if cost[dow][h] > mx["cost_usd"] {
			mx["cost_usd"] = cost[dow][h]
		}
	}
	return json.Marshal(map[string]any{
		"dow_hours": map[string]any{
			"requests": requests,
			"tokens":   tokens,
			"cost_usd": cost,
		},
		"max":      mx,
		"timezone": time.Now().Format("MST"),
		"range": map[string]any{
			"start": time.Unix(int64(lo), 0).Format(time.RFC3339),
			"end":   time.Unix(int64(hi), 0).Format(time.RFC3339),
		},
	})
}

// ---- GET /api/analytics/cost — today/month calendar blocks (contract B2) ----

// costRangeBlock aggregates one independent range into the contract B2 block
// shape {usd, priced_events, total_events}: usd mirrors totals.priced_usd
// semantics (sum of every matched estimate, including partial) and is null
// when nothing priced; the counts are always present — priced_events counts
// fully-priced events only, same as totals.priced_events.
func costRangeBlock(events []eventRow) map[string]any {
	infos, mrs := resolveEstimates(events, false)
	usd := 0.0
	var usdOK bool
	var pricedN int64
	for i, e := range events {
		recordMatch(e.provider, e.model, mrs[i])
		c := infos[i]
		if c.matched {
			usd += c.usd
			usdOK = true
			if c.complete {
				pricedN++
			}
		}
	}
	var usdVal any
	if usdOK {
		usdVal = usd
	}
	return map[string]any{
		"usd":           usdVal,
		"priced_events": pricedN,
		"total_events":  int64(len(events)),
	}
}

// analyticsCostResponse wraps analyticsCost (pricing.go, another lane) and
// patches in the calendar-boundary today/month blocks required by contract
// B2: each block is computed over its own independent range — [start of
// local day, +1d) and [start of local calendar month, +1mo) in time.Local —
// honoring the request's model/key filters but never its start/end range.
// Estimate resolution reuses resolveEstimates (stored rows win, missing
// computed+persisted); recalc stays scoped to the selected range, so these
// blocks always resolve with stored semantics. The legacy scalars
// today_usd/month_usd are kept and mirror the blocks' usd (null when
// unpriced — an unknown price must never read as $0).
// decodePreservingNumbers unmarshals a JSON object while keeping numeric
// literals exact: Decoder.UseNumber lands every number as json.Number, which
// re-marshals byte-identical — a plain Unmarshal into map[string]any would
// round integers > 2^53 through float64 and lose digits.
func decodePreservingNumbers(b []byte) (map[string]any, error) {
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

func analyticsCostResponse(q map[string][]string) ([]byte, error) {
	base, err := analyticsCost(q)
	if err != nil {
		return nil, err
	}
	model, key := "", ""
	if v := q["model"]; len(v) > 0 {
		model = v[0]
	}
	if v := q["key"]; len(v) > 0 {
		key = v[0]
	}
	now := time.Now()
	today0 := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	month0 := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.Local)
	todayEnd := float64(today0.AddDate(0, 0, 1).Unix())
	// today ⊂ month: one range scan for the calendar month, then the today
	// block reuses its in-range tail instead of a second DB round-trip.
	monthEvents, _, err := loadEventsRange(float64(month0.Unix()), float64(month0.AddDate(0, 1, 0).Unix()), model, key)
	if err != nil {
		return nil, err
	}
	var todayEvents []eventRow
	for _, e := range monthEvents {
		if e.ts >= float64(today0.Unix()) && e.ts < todayEnd {
			todayEvents = append(todayEvents, e)
		}
	}
	today := costRangeBlock(todayEvents)
	month := costRangeBlock(monthEvents)
	resp, err := decodePreservingNumbers(base)
	if err != nil {
		return nil, err
	}
	resp["today"] = today
	resp["month"] = month
	resp["today_usd"] = today["usd"]
	resp["month_usd"] = month["usd"]
	return json.Marshal(resp)
}
