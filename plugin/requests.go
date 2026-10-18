package main

// ---- Advanced Request Inspector ----
//
// GET /api/requests       — paginated, filterable request list
// GET /api/requests/{id}  — single-request detail (explicit allowlist)
//
// Query strategy: the date range pushes down to the ts_unix prefilter (same
// id IN (range UNION ALL is-null) shape as queryUsageRows); when the model /
// status / min_latency_ms params are present they additionally push down as
// json_extract predicates guarded by json_valid, so one malformed payload
// cannot fail the whole list. Candidate rows are then streamed through
// rows.Next() — full payloads are never retained, only the compact
// eventRow plus (when cost filtering/sorting is requested) its resolved
// eventCostInfo.
//
// Sorting happens Go-side over the candidate set because several sort keys
// are derived (normalized totals, resolved cost) and legacy NULL-ts_unix
// rows can only be ordered after payload decode. Trade-off (documented):
// within the requested [start,end] window all candidates are materialized
// as compact rows before paging — O(window) memory like the other
// analytics endpoints, but the response page itself is the only thing
// re-parsed for output.
//
// Cursors are keyset cursors for EVERY sort (incl. derived ones): since the
// sort runs in memory, locating the position after the last emitted row is
// a bounded scan and stays stable under concurrent inserts. The spec's
// offset fallback ({off}) is reserved for sorts that cannot produce a
// keyset cheaply — here every sort has a total order ending in an id
// tie-break, so {sv:[key values..., id]} is always used. next_cursor=null
// when the page reaches the end.

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

const (
	requestDefaultLimit = 20
	requestMaxLimit     = 100
)

// requestFilters is the parsed /api/requests query.
type requestFilters struct {
	lo, hi        float64
	model         string
	key           string
	status        string // all|success|failed
	minLatency    float64
	hasMinLatency bool
	minTokens     int64
	hasMinTokens  bool
	minCost       float64
	hasMinCost    bool
	pricingStatus string // "" | priced|partial|unpriced|ambiguous
	search        string
	searchLower   string
	searchID      int64 // >0 when search is all digits
	limit         int
	sort          string
	cursor        string
}

var requestSortKeys = []string{
	"newest", "oldest", "most_tokens", "least_tokens",
	"highest_latency", "highest_cost", "failed_first",
}

// isDigits reports whether s is a non-empty all-digits string.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func firstParam(q map[string][]string, k string) string {
	if v, ok := q[k]; ok && len(v) > 0 {
		return v[0]
	}
	return ""
}

// parseRequestFilters validates the query into requestFilters. Every
// failure is a 400 with a clear message.
func parseRequestFilters(q map[string][]string) (requestFilters, error) {
	var f requestFilters
	lo, hi, err := analyticsRange(q)
	if err != nil {
		return f, err
	}
	f.lo, f.hi = lo, hi
	f.model = firstParam(q, "model")
	f.key = firstParam(q, "key")
	f.status = firstParam(q, "status")
	if f.status == "" {
		f.status = "all"
	}
	if f.status != "all" && f.status != "success" && f.status != "failed" {
		return f, fmt.Errorf("invalid status: %s", f.status)
	}
	if s := firstParam(q, "min_latency_ms"); s != "" {
		f.minLatency, err = strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(f.minLatency) || math.IsInf(f.minLatency, 0) {
			return f, fmt.Errorf("invalid min_latency_ms: %s", s)
		}
		f.hasMinLatency = true
	}
	if s := firstParam(q, "min_total_tokens"); s != "" {
		f.minTokens, err = strconv.ParseInt(s, 10, 64)
		if err != nil {
			return f, fmt.Errorf("invalid min_total_tokens: %s", s)
		}
		f.hasMinTokens = true
	}
	if s := firstParam(q, "min_cost_usd"); s != "" {
		f.minCost, err = strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(f.minCost) || math.IsInf(f.minCost, 0) {
			return f, fmt.Errorf("invalid min_cost_usd: %s", s)
		}
		f.hasMinCost = true
	}
	f.pricingStatus = firstParam(q, "pricing_status")
	switch f.pricingStatus {
	case "", "priced", "partial", "unpriced", "ambiguous":
	default:
		return f, fmt.Errorf("invalid pricing_status: %s", f.pricingStatus)
	}
	f.search = firstParam(q, "search")
	if f.search != "" {
		f.searchLower = strings.ToLower(f.search)
		if isDigits(f.search) {
			if id, err := strconv.ParseInt(f.search, 10, 64); err == nil {
				f.searchID = id
			}
		}
	}
	f.sort = firstParam(q, "sort")
	if f.sort == "" {
		f.sort = "newest"
	}
	ok := false
	for _, s := range requestSortKeys {
		if f.sort == s {
			ok = true
			break
		}
	}
	if !ok {
		return f, fmt.Errorf("invalid sort: %s (allowed: %s)", f.sort, strings.Join(requestSortKeys, ", "))
	}
	f.limit = requestDefaultLimit
	if s := firstParam(q, "limit"); s != "" {
		n, perr := strconv.Atoi(s)
		if perr != nil {
			return f, fmt.Errorf("invalid limit: %s", s)
		}
		if n < 1 {
			return f, fmt.Errorf("invalid limit: %d", n)
		}
		if n > requestMaxLimit {
			n = requestMaxLimit // clamp, never error
		}
		f.limit = n
	}
	f.cursor = firstParam(q, "cursor")
	return f, nil
}

// requestCandidate is the compact per-row state kept while scanning.
// The payload is released as soon as normalizeRow returns.
type requestCandidate struct {
	row  eventRow
	cost eventCostInfo
}

// sortKey returns the ordered key tuple for the active sort. The id
// tie-break is always the last element so the order is total and the cursor
// position unambiguous.
func (f *requestFilters) sortKey(c requestCandidate) []float64 {
	e := c.row
	switch f.sort {
	case "oldest":
		return []float64{e.ts, float64(e.id)}
	case "most_tokens":
		return []float64{float64(e.input + e.output), float64(e.id)}
	case "least_tokens":
		return []float64{float64(e.input + e.output), float64(e.id)}
	case "highest_latency":
		return []float64{e.latency, float64(e.id)}
	case "highest_cost":
		matched := 0.0
		if c.cost.matched {
			matched = 1
		}
		return []float64{matched, c.cost.usd, float64(e.id)}
	case "failed_first":
		failed := 0.0
		if e.failed {
			failed = 1
		}
		return []float64{failed, e.ts, float64(e.id)}
	default: // newest
		return []float64{e.ts, float64(e.id)}
	}
}

// after reports whether key `a` strictly follows key `b` in the current
// sort order (descending sorts compare reversed).
func (f *requestFilters) after(a, b []float64) bool {
	asc := f.sort == "oldest" || f.sort == "least_tokens"
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] == b[i] {
			continue
		}
		if asc {
			return a[i] > b[i]
		}
		return a[i] < b[i]
	}
	return len(a) > len(b)
}

// matchGoFilters applies the conditions that cannot (or cheaply cannot)
// push down to SQL. The SQL prefilter already guarantees model / status /
// min_latency_ms, but the checks are repeated here — json_extract types do
// not always equal the Go decode semantics (e.g. failed:"true"), so the Go
// check is the contract source of truth.
func (f *requestFilters) matchGoFilters(e eventRow) bool {
	if f.model != "" && e.model != f.model {
		return false
	}
	if f.key != "" && e.key != f.key {
		return false
	}
	switch f.status {
	case "success":
		if e.failed {
			return false
		}
	case "failed":
		if !e.failed {
			return false
		}
	}
	if f.hasMinLatency && !(e.latency > 0 && e.latency >= f.minLatency) {
		// stored-0/absent latency never satisfies the threshold
		return false
	}
	if f.hasMinTokens && e.input+e.output < f.minTokens {
		return false
	}
	if f.search != "" {
		if !requestSearchMatch(e, f.searchLower, f.searchID) {
			return false
		}
	}
	return true
}

func requestSearchMatch(e eventRow, lower string, id int64) bool {
	// All-digits searches mean "find this request id", not "substring". The
	// contract gives the numeric input exactly one semantics: exact id.
	if id > 0 {
		return e.id == id
	}
	return strings.Contains(strings.ToLower(e.model), lower) ||
		strings.Contains(strings.ToLower(e.provider), lower) ||
		strings.Contains(strings.ToLower(e.key), lower)
}

// needsCost reports whether the candidate loop must resolve estimates for
// every row (cost filter/sort) instead of only for the emitted page.
func (f *requestFilters) needsCost() bool {
	return f.hasMinCost || f.pricingStatus != "" || f.sort == "highest_cost"
}

// sqlConds builds the payload-level json_extract prefilters for the params
// that translate to SQL exactly. The json_valid guard is only added (and
// paid for) when at least one json predicate exists.
func (f *requestFilters) sqlConds() (string, []any) {
	var conds []string
	var args []any
	if f.model != "" {
		conds = append(conds,
			`COALESCE(NULLIF(json_extract(payload,'$.model'),''),NULLIF(json_extract(payload,'$.alias'),''),'unknown') = ?`)
		args = append(args, f.model)
	}
	switch f.status {
	case "success":
		conds = append(conds, `COALESCE(json_extract(payload,'$.failed'),0) = 0`)
	case "failed":
		conds = append(conds, `COALESCE(json_extract(payload,'$.failed'),0) = 1`)
	}
	if f.hasMinLatency {
		// json must contain a positive latency at or above the threshold;
		// absent/stored-0 rows are excluded here AND by the Go check.
		conds = append(conds, `json_extract(payload,'$.latency_ms') > 0`,
			`json_extract(payload,'$.latency_ms') >= ?`)
		args = append(args, f.minLatency)
	}
	if len(conds) == 0 {
		return "", nil
	}
	// json_extract on malformed JSON aborts the query; the guard keeps one
	// corrupt payload from taking down the list.
	return " AND json_valid(payload) AND " + strings.Join(conds, " AND "), args
}

// queryRequestRows returns usage_events rows in [lo,hi) matching the
// SQL-pushable filters, ordered by id. Same dual-branch strategy as
// queryUsageRows: index range scan ∪ NULL safety net, or a legacy full
// scan when ts_unix is unusable. Caller holds mu.
func queryRequestRows(lo, hi float64, conds string, args []any) (*sql.Rows, error) {
	if db == nil {
		return nil, nil
	}
	if tsUnixIndexUsableLocked() {
		q := `SELECT id, ts, payload FROM usage_events WHERE id IN (
			SELECT id FROM usage_events WHERE ts_unix >= ? AND ts_unix < ?
			UNION ALL
			SELECT id FROM usage_events WHERE ts_unix IS NULL
		)` + conds + " ORDER BY id"
		full := append([]any{lo, hi}, args...)
		return db.Query(q, full...)
	}
	return db.Query("SELECT id, ts, payload FROM usage_events WHERE 1=1"+conds+" ORDER BY id", args...)
}

// scanRequestCandidates streams the prefiltered rows, decodes each payload,
// applies the range and Go-side filters, and keeps only compact candidates.
// No full slice of payloads is ever built.
func scanRequestCandidates(f *requestFilters) ([]requestCandidate, error) {
	conds, args := f.sqlConds()
	ensureDB()
	mu.Lock()
	rows, err := queryRequestRows(f.lo, f.hi, conds, args)
	mu.Unlock()
	if err != nil {
		return nil, err
	}
	out := []requestCandidate{}
	if rows == nil {
		return out, nil
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var storedTs, payload string
		if err := rows.Scan(&id, &storedTs, &payload); err != nil {
			continue
		}
		var e dbEvent
		if err := json.Unmarshal([]byte(payload), &e); err != nil {
			continue
		}
		row, ok := normalizeRow(e, storedTs)
		if !ok {
			continue
		}
		row.id = id
		if row.ts < f.lo || row.ts >= f.hi {
			continue // NULL-ts_unix safety net + parse discrepancies
		}
		if !f.matchGoFilters(row) {
			continue
		}
		out = append(out, requestCandidate{row: row})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ---- cursor ----

type requestCursor struct {
	Sort string    `json:"s"`
	Vals []float64 `json:"v"`
}

func encodeRequestCursor(f *requestFilters, c requestCandidate) string {
	raw, err := json.Marshal(requestCursor{Sort: f.sort, Vals: f.sortKey(c)})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodeRequestCursor parses and validates the opaque cursor; the encoded
// sort must equal the request's sort or the cursor is meaningless.
func decodeRequestCursor(f *requestFilters) (requestCursor, error) {
	var c requestCursor
	if f.cursor == "" {
		return c, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(f.cursor)
	if err != nil {
		if raw, err = base64.StdEncoding.DecodeString(f.cursor); err != nil {
			return c, fmt.Errorf("invalid cursor")
		}
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("invalid cursor")
	}
	if c.Sort != f.sort {
		return c, fmt.Errorf("cursor sort mismatch: cursor is for sort=%s, request has sort=%s", c.Sort, f.sort)
	}
	if len(c.Vals) != len(f.sortKey(requestCandidate{})) {
		return c, fmt.Errorf("invalid cursor")
	}
	for _, v := range c.Vals {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return c, fmt.Errorf("invalid cursor")
		}
	}
	return c, nil
}

// ---- list ----

func posOrNil(v float64) any {
	if v > 0 {
		return v
	}
	return nil
}

func intOrNil(v int) any {
	if v > 0 {
		return v
	}
	return nil
}

// requestListItem is the contract shape emitted for each list row. Cost
// fields come from the (read-only) estimate resolution; unpriced/ambiguous
// events report null usd and their status verbatim.
func requestListItem(c requestCandidate) map[string]any {
	e := c.row
	var costUSD any
	if c.cost.matched {
		costUSD = c.cost.usd
	}
	status := c.cost.status
	if status == "" {
		status = "unpriced"
	}
	return map[string]any{
		"id":                 e.id,
		"ts":                 e.ts,
		"model":              e.model,
		"provider":           e.provider,
		"key":                e.key,
		"failed":             e.failed,
		"status_code":        intOrNil(e.statusCode),
		"input":              e.input,
		"cache":              e.cache,
		"cache_write":        e.cacheWrite,
		"output":             e.output,
		"reasoning":          e.reasoning,
		"total":              e.input + e.output,
		"latency_ms":         posOrNil(e.latency),
		"ttft_ms":            posOrNil(e.ttft),
		"estimated_cost_usd": costUSD,
		"pricing_status":     status,
	}
}

// requestList implements GET /api/requests.
func requestList(q map[string][]string) (int, []byte) {
	f, err := parseRequestFilters(q)
	if err != nil {
		b, _ := json.Marshal(map[string]any{"error": err.Error()})
		return 400, b
	}
	cur, err := decodeRequestCursor(&f)
	if err != nil {
		b, _ := json.Marshal(map[string]any{"error": err.Error()})
		return 400, b
	}
	cands, err := scanRequestCandidates(&f)
	if err != nil {
		b, _ := json.Marshal(map[string]any{"error": err.Error()})
		return 400, b
	}

	if f.needsCost() {
		rows := make([]eventRow, len(cands))
		for i := range cands {
			rows[i] = cands[i].row
		}
		infos, _ := resolveEstimatesReadOnly(rows)
		kept := cands[:0]
		for i := range cands {
			c := cands[i]
			c.cost = infos[i]
			if f.hasMinCost && !(c.cost.matched && c.cost.usd >= f.minCost) {
				continue
			}
			if f.pricingStatus != "" && c.cost.status != f.pricingStatus {
				continue
			}
			kept = append(kept, c)
		}
		cands = kept
	}

	// Total order: sort key with id tie-break. sort.SliceStable is
	// deterministic because sortKey already contains the unique id.
	sort.SliceStable(cands, func(i, j int) bool {
		return f.after(f.sortKey(cands[j]), f.sortKey(cands[i]))
	})

	start := 0
	if f.cursor != "" {
		// Keyset resume: first candidate strictly after the cursor's key.
		start = sort.Search(len(cands), func(i int) bool {
			return f.after(f.sortKey(cands[i]), cur.Vals)
		})
	}
	end := start + f.limit
	if end > len(cands) {
		end = len(cands)
	}
	page := cands[start:end]
	hasMore := end < len(cands)

	if !f.needsCost() {
		// Page-scoped cost resolution: only the emitted rows are priced
		// (read-only — no cost_estimates writes).
		rows := make([]eventRow, len(page))
		for i := range page {
			rows[i] = page[i].row
		}
		infos, _ := resolveEstimatesReadOnly(rows)
		for i := range page {
			page[i].cost = infos[i]
		}
	}

	items := make([]map[string]any, 0, len(page))
	for _, c := range page {
		items = append(items, requestListItem(c))
	}
	var next any
	if hasMore {
		next = encodeRequestCursor(&f, page[len(page)-1])
	}
	resp, err := json.Marshal(map[string]any{
		"items":       items,
		"next_cursor": next,
		"has_more":    hasMore,
		"limit":       f.limit,
	})
	if err != nil {
		b, _ := json.Marshal(map[string]any{"error": err.Error()})
		return 400, b
	}
	return 200, resp
}

// ---- detail ----

// requestDetail implements GET /api/requests/{id} with an explicit output
// allowlist — the raw payload (source/api_key/executor_type) is never
// serialized back.
func requestDetail(id int64) (int, []byte) {
	ensureDB()
	maybeAutoSync()
	maybeFxSync()
	mu.Lock()
	var storedTs string
	var payload sql.NullString
	err := sql.ErrNoRows
	if db != nil {
		err = db.QueryRow("SELECT ts, payload FROM usage_events WHERE id=?", id).Scan(&storedTs, &payload)
	}
	mu.Unlock()
	if err == sql.ErrNoRows {
		b, _ := json.Marshal(map[string]any{"error": "request not found"})
		return 404, b
	}
	if err != nil {
		b, _ := json.Marshal(map[string]any{"error": err.Error()})
		return 400, b
	}
	var e dbEvent
	if !payload.Valid || json.Unmarshal([]byte(payload.String), &e) != nil {
		b, _ := json.Marshal(map[string]any{"error": "request not found"})
		return 404, b
	}
	row, ok := normalizeRow(e, storedTs)
	if !ok {
		b, _ := json.Marshal(map[string]any{"error": "request not found"})
		return 404, b
	}
	row.id = id

	infos, _ := resolveEstimatesReadOnly([]eventRow{row})
	c := infos[0]

	baseInput := row.input - row.cache - row.cacheWrite
	if baseInput < 0 {
		baseInput = 0
	}
	// tokens_note flags the one normalization a user would otherwise misread:
	// input absorbed cache tokens (addInputCache folded read+create into
	// input), so base_input is the pre-cache remainder.
	var tokensNote any
	if row.input != toI64(e.Tokens["input_tokens"]) {
		tokensNote = "input includes cache read/write tokens per provider convention"
	}

	var outputPerSec any
	if row.output > 0 && row.latency > 0 {
		outputPerSec = float64(row.output) / (row.latency / 1000)
	}

	// Cost block: every field follows resolveEstimates semantics — stored
	// estimates win, computed estimates are never persisted by this path.
	var usd, inU, outU, crU, cwU any
	if c.matched {
		usd = c.usd
		// Component costs only surface when the component both priced AND
		// was used — a used-but-unpriced component is null (never reads as
		// $0) and feeds partial_reasons below.
		if c.okIn && baseInput > 0 {
			inU = c.inCost
		}
		if c.okOut && row.output > 0 {
			outU = c.outCost
		}
		if c.okCR && row.cache > 0 {
			crU = c.crCost
		}
		if c.okCW && row.cacheWrite > 0 {
			cwU = c.cwCost
		}
	}
	partialReasons := []string{}
	if c.status == "partial" {
		if !c.okIn && baseInput > 0 {
			partialReasons = append(partialReasons, "input")
		}
		if !c.okOut && row.output > 0 {
			partialReasons = append(partialReasons, "output")
		}
		if !c.okCR && row.cache > 0 {
			partialReasons = append(partialReasons, "cache_read")
		}
		if !c.okCW && row.cacheWrite > 0 {
			partialReasons = append(partialReasons, "cache_write")
		}
	}
	var snapID any
	if c.ep.snapID != nil {
		snapID = *c.ep.snapID
	}
	status := c.status
	if status == "" {
		status = "unpriced"
	}
	cost := map[string]any{
		"usd":              usd,
		"input_usd":        inU,
		"output_usd":       outU,
		"cache_read_usd":   crU,
		"cache_write_usd":  cwU,
		"status":           status,
		"source":           nilIfEmpty(c.ep.source),
		"matched_model":    nilIfEmpty(c.ep.matchedModel),
		"matched_provider": nilIfEmpty(c.ep.matchedProv),
		"snapshot_id":      snapID,
		"estimate":         true,
		"partial_reasons":  partialReasons,
	}

	resp := map[string]any{
		"id":          row.id,
		"ts":          row.ts,
		"model":       row.model,
		"provider":    row.provider,
		"key":         row.key,
		"failed":      row.failed,
		"status_code": intOrNil(row.statusCode),
		"auth_type":   nilIfEmpty(row.authType),
		"tokens": map[string]any{
			"input":       row.input,
			"output":      row.output,
			"cache_read":  row.cache,
			"cache_write": row.cacheWrite,
			"reasoning":   row.reasoning,
			"total":       row.input + row.output,
			"base_input":  baseInput,
		},
		"tokens_note": tokensNote,
		"performance": map[string]any{
			"latency_ms":              posOrNil(row.latency),
			"ttft_ms":                 posOrNil(row.ttft),
			"output_per_sec_estimate": outputPerSec,
		},
		"cost": cost,
		"fx":   fxInfo(),
	}
	out, err := json.Marshal(resp)
	if err != nil {
		b, _ := json.Marshal(map[string]any{"error": err.Error()})
		return 400, b
	}
	return 200, out
}

// ---- dispatch ----

// handleRequests routes /api/requests* management requests. Mirrors
// handleFx/handlePricing dispatch conventions.
func handleRequests(path, method string, q map[string][]string) ([]byte, error) {
	i := strings.Index(path, "/api/requests")
	if i < 0 {
		return managementResponse(404, "text/plain; charset=utf-8", []byte("not found"))
	}
	rest := strings.SplitN(path[i+len("/api/requests"):], "?", 2)[0]
	rest = strings.TrimSuffix(rest, "/")
	var status int
	var out []byte
	switch {
	case rest == "" && method == http.MethodGet:
		status, out = requestList(q)
	case len(rest) > 1 && rest[0] == '/' && !strings.Contains(rest[1:], "/") && method == http.MethodGet:
		seg := rest[1:]
		if !isDigits(seg) {
			// Single non-digit segment → malformed id, not missing.
			b, _ := json.Marshal(map[string]any{"error": "invalid request id"})
			return managementResponse(400, "application/json; charset=utf-8", b)
		}
		id, err := strconv.ParseInt(seg, 10, 64)
		if err != nil {
			b, _ := json.Marshal(map[string]any{"error": "invalid request id"})
			return managementResponse(400, "application/json; charset=utf-8", b)
		}
		status, out = requestDetail(id)
	default:
		return managementResponse(404, "text/plain; charset=utf-8", []byte("not found"))
	}
	return managementResponse(status, "application/json; charset=utf-8", out)
}
