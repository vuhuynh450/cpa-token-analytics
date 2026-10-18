package main

// Tests for GET /api/changes — the revision/polling probe.
// Covers: empty-DB zero revision, insert/dedup/failed-write bump rules,
// dataset_id mint + persistence across reopen + distinctness across files,
// GET-only dispatch, response hygiene (no sensitive fields), concurrent
// seed+poll monotonicity, and the revision query benchmark (100k / 1M rows).

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// getChanges calls the real management dispatch for /api/changes.
func getChanges(t *testing.T, method string) (int, map[string]any) {
	t.Helper()
	status, body := callMgmtQuery(t, "/v0/management/plugins/cpa-token-analytics/api/changes", method, nil)
	var out map[string]any
	if status == 200 {
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode changes: %v body=%s", err, body)
		}
	}
	return status, out
}

func TestChangesEmptyDB(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "empty.sqlite"))
	status, out := getChanges(t, "GET")
	if status != 200 {
		t.Fatalf("status: %d", status)
	}
	if got := num(out["usage_revision"]); got != 0 {
		t.Fatalf("empty DB revision must be 0, got %v", got)
	}
	if got := num(out["count"]); got != 0 {
		t.Fatalf("empty DB count must be 0, got %v", got)
	}
	if id, _ := out["dataset_id"].(string); len(id) != 32 {
		t.Fatalf("dataset_id must be 32 hex chars, got %q", id)
	}
	ts, _ := out["checked_at"].(string)
	if _, err := time.Parse(time.RFC3339, ts); err != nil || !strings.HasSuffix(ts, "Z") {
		t.Fatalf("checked_at must be RFC3339 UTC, got %q (%v)", ts, err)
	}
}

func TestChangesRevisionBumps(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "bump.sqlite"))

	_, first := getChanges(t, "GET")
	baseRev, baseCount := num(first["usage_revision"]), num(first["count"])

	seedRecord(t, time.Date(2026, 10, 18, 8, 0, 0, 0, time.UTC), map[string]any{"Model": "m-a"})
	_, out := getChanges(t, "GET")
	rev1 := num(out["usage_revision"])
	if rev1 <= baseRev {
		t.Fatalf("revision after insert: got %v want > %v", rev1, baseRev)
	}
	if num(out["count"]) != baseCount+1 {
		t.Fatalf("count after insert: got %v want %v", out["count"], baseCount+1)
	}

	// Same payload again → dedup IGNORE → no new row: MAX(id) and count
	// stay put (AUTOINCREMENT burns sqlite_sequence on the ignored row, so
	// later real ids are non-contiguous — monotonic is the contract).
	seedRecord(t, time.Date(2026, 10, 18, 8, 0, 0, 0, time.UTC), map[string]any{"Model": "m-a"})
	_, dup := getChanges(t, "GET")
	if num(dup["usage_revision"]) != rev1 || num(dup["count"]) != baseCount+1 {
		t.Fatalf("dedup must not bump: got rev=%v count=%v", dup["usage_revision"], dup["count"])
	}

	// Invalid usage.handle payload → early return before INSERT → no bump.
	if _, err := handleMethod("usage.handle", []byte(`{"result":{"bogus":`)); err != nil {
		t.Fatal(err)
	}
	_, failed := getChanges(t, "GET")
	if num(failed["usage_revision"]) != rev1 || num(failed["count"]) != baseCount+1 {
		t.Fatalf("failed write must not bump: got rev=%v count=%v", failed["usage_revision"], failed["count"])
	}

	// A distinct payload bumps again (strictly, possibly past seq gaps).
	seedRecord(t, time.Date(2026, 10, 18, 8, 0, 1, 0, time.UTC), map[string]any{"Model": "m-b"})
	_, third := getChanges(t, "GET")
	if num(third["usage_revision"]) <= rev1 || num(third["count"]) != baseCount+2 {
		t.Fatalf("second insert: got rev=%v count=%v", third["usage_revision"], third["count"])
	}
}

func TestChangesDatasetID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.sqlite")
	openDB(path)
	_, first := getChanges(t, "GET")
	id1, _ := first["dataset_id"].(string)
	if len(id1) != 32 {
		t.Fatalf("dataset_id minted on open: %q", id1)
	}

	// Reopening the same file keeps the id (plugin_meta row persists).
	openDB(path)
	_, again := getChanges(t, "GET")
	if id2, _ := again["dataset_id"].(string); id2 != id1 {
		t.Fatalf("reopen must keep dataset_id: %q vs %q", id1, id2)
	}

	// A different DB file gets a different id — same max(id) cannot alias.
	openDB(filepath.Join(dir, "b.sqlite"))
	_, other := getChanges(t, "GET")
	id3, _ := other["dataset_id"].(string)
	if len(id3) != 32 || id3 == id1 {
		t.Fatalf("fresh file must mint a distinct dataset_id: %q vs %q", id1, id3)
	}
}

func TestChangesGetOnly(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "methods.sqlite"))
	for _, m := range []string{"POST", "PUT", "DELETE"} {
		if status, _ := getChanges(t, m); status != 404 {
			t.Fatalf("%s /api/changes: got %d, want 404", m, status)
		}
	}
	if status, _ := getChanges(t, "GET"); status != 200 {
		t.Fatal("GET must still be 200 after rejected methods")
	}
}

func TestChangesNoSensitiveFields(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "hygiene.sqlite"))
	seedRecord(t, time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC), map[string]any{
		"Model": "m-secret", "Source": "sk-supersecret-key-material",
	})
	status, body := callMgmtQuery(t, "/v0/management/plugins/cpa-token-analytics/api/changes", "GET", nil)
	if status != 200 {
		t.Fatalf("status: %d", status)
	}
	for _, frag := range []string{"payload", "source", "api_key", "key-", "sk-supersecret", "model"} {
		if strings.Contains(string(body), frag) {
			t.Fatalf("response leaks %q: %s", frag, body)
		}
	}
	// Shape contract: exactly the four documented fields.
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"usage_revision", "count", "dataset_id", "checked_at"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing field %q in %s", k, body)
		}
	}
	if len(m) != 4 {
		t.Fatalf("unexpected extra fields: %s", body)
	}
}

// TestChangesConcurrentPoll races real inserts against concurrent polls and
// asserts the observed revision is monotonically non-decreasing and the
// endpoint never errors. Run with -race.
func TestChangesConcurrentPoll(t *testing.T) {
	openDB(filepath.Join(t.TempDir(), "race.sqlite"))

	var stop sync.WaitGroup
	polls := 8
	errCh := make(chan string, polls*64)
	for i := 0; i < polls; i++ {
		stop.Add(1)
		go func(id int) {
			defer stop.Done()
			prev := int64(-1) // per-poller sequence must never decrease
			for j := 0; j < 25; j++ {
				req, _ := json.Marshal(map[string]any{"result": map[string]any{
					"Method": "GET", "Path": "/v0/management/plugins/cpa-token-analytics/api/changes",
					"Query": map[string][]string{}, "Headers": map[string][]string{}, "Body": nil,
				}})
				raw, err := handleMethod("management.handle", req)
				if err != nil {
					errCh <- err.Error()
					return
				}
				var e envelope
				if json.Unmarshal(raw, &e) != nil || !e.OK {
					errCh <- "bad envelope"
					return
				}
				var resp struct {
					StatusCode int
					Body       []byte
				}
				if json.Unmarshal(e.Result, &resp) != nil || resp.StatusCode != 200 {
					errCh <- "non-200"
					return
				}
				var m map[string]any
				if json.Unmarshal(resp.Body, &m) != nil {
					errCh <- "bad body"
					return
				}
				rev := int64(num(m["usage_revision"]))
				if rev < prev {
					errCh <- fmt.Sprintf("poller %d saw revision drop %d → %d", id, prev, rev)
					return
				}
				prev = rev
			}
		}(i)
	}

	const writes = 30
	for i := 0; i < writes; i++ {
		rec := map[string]any{"result": map[string]any{
			"Model": fmt.Sprintf("m-%d", i), "Source": "sk-race",
			"RequestedAt": time.Date(2026, 10, 18, 10, i%60, i, 0, time.UTC).Format(time.RFC3339Nano),
		}}
		body, _ := json.Marshal(rec)
		if _, err := handleMethod("usage.handle", body); err != nil {
			t.Fatal(err)
		}
	}
	stop.Wait()
	close(errCh)
	for msg := range errCh {
		t.Fatalf("poll error: %s", msg)
	}
	_, final := getChanges(t, "GET")
	if got := num(final["usage_revision"]); got != writes {
		t.Fatalf("final revision: got %v want %d", got, writes)
	}
}

// BenchmarkChangesQuery measures the revision probe over 100k / 1M rows to
// keep COUNT(*) honest — the endpoint must stay well under 50ms.
// Run: go test -bench ChangesQuery -benchtime=10x -run '^$'
func BenchmarkChangesQuery(b *testing.B) {
	for _, n := range []int{100_000, 1_000_000} {
		if n > 100_000 && testing.Short() {
			b.Skip("1M skipped under -short")
		}
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			openBenchDB(b, seedBenchDB(b, n, 0, true))
			b.ReportAllocs()
			b.ResetTimer()
			var rev, cnt int64
			for i := 0; i < b.N; i++ {
				mu.Lock()
				rev, cnt = changesRevisionLocked()
				mu.Unlock()
			}
			b.StopTimer()
			if rev != int64(n) || cnt != int64(n) {
				b.Fatalf("rev=%d count=%d want %d", rev, cnt, n)
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/1e6, "ms/op")
		})
	}
}
