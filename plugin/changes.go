package main

// ---- GET /api/changes — lightweight revision endpoint for smart polling ----
//
// The frontend polls this to decide whether the heavier endpoints are worth
// re-fetching. It must stay cheap: one aggregate probe over the rowid B-tree
// plus one plugin_meta lookup — no payloads, no event decoding, nothing
// sensitive (no source/key material ever leaves this handler).
//
// Tuple semantics:
//   usage_revision — MAX(usage_events.id). INTEGER PRIMARY KEY, so MAX() is
//     served by the rowid B-tree (O(1)-ish seek, not a scan). INSERT OR IGNORE
//     + dedup means it only moves when a real insert happens: dedup hits and
//     failed writes never bump it. Empty table → 0.
//   count — COUNT(*): catches dataset changes that keep the same max id
//     (row deletions). Covering-index scan; measured ~0.2ms at 100k and ~6ms
//     at 1M rows (BenchmarkChangesQuery), well under the 50ms budget.
//   dataset_id — plugin_meta['dataset_id'], a random 16-byte hex minted once
//     by migrations. Swapping/replacing the DB file yields a different id even
//     when max(id) coincides, so the tuple can't read as "unchanged".
//   checked_at — RFC3339 UTC time of the probe.

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"
)

const datasetIDKey = "dataset_id"

// seedDatasetID mints plugin_meta['dataset_id'] once per DB file. Runs inside
// runMigrations against the raw connection (the global db is not yet
// assigned); INSERT OR IGNORE makes it idempotent and preserves the id across
// reopens of the same file.
func seedDatasetID(conn *sql.DB) error {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	_, err := conn.Exec("INSERT OR IGNORE INTO plugin_meta (k,v) VALUES (?,?)",
		datasetIDKey, hex.EncodeToString(b[:]))
	return err
}

// changesRevisionLocked probes MAX(id)+COUNT(*) as two independent
// subqueries — measured ~6ms on a 1M-row DB. The planner picks a rowid
// B-tree seek for MAX(id) (O(1)) plus the narrowest covering index for
// COUNT(*); a single `MAX(id), COUNT(*)` instead degrades to a ~77ms plan
// where the aggregate scan swallows the MAX optimization. Caller holds mu.
func changesRevisionLocked() (revision, count int64) {
	if db == nil {
		return 0, 0
	}
	var maxID sql.NullInt64
	err := db.QueryRow(`SELECT
		(SELECT MAX(id) FROM usage_events),
		(SELECT COUNT(*) FROM usage_events)`).Scan(&maxID, &count)
	if err != nil {
		return 0, 0
	}
	if maxID.Valid {
		revision = maxID.Int64
	}
	return revision, count
}

func changesResponse() ([]byte, error) {
	ensureDB()
	mu.Lock()
	revision, count := changesRevisionLocked()
	dataset := ""
	if v := metaGet(datasetIDKey); v != nil {
		dataset = *v
	}
	mu.Unlock()
	return json.Marshal(map[string]any{
		"usage_revision": revision,
		"count":          count,
		"dataset_id":     dataset,
		"checked_at":     time.Now().UTC().Format(time.RFC3339),
	})
}

// handleChanges serves GET /api/changes only; every other method falls
// through to 404 like the other management dispatchers.
func handleChanges(method string) ([]byte, error) {
	if method != http.MethodGet {
		return managementResponse(404, "text/plain; charset=utf-8", []byte("not found"))
	}
	body, err := changesResponse()
	if err != nil {
		b, _ := json.Marshal(map[string]any{"error": err.Error()})
		return managementResponse(400, "application/json; charset=utf-8", b)
	}
	return managementResponse(200, "application/json; charset=utf-8", body)
}
