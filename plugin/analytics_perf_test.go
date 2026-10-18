package main

// Benchmarks for the analytics load path and the ts_unix backfill (contract
// B2 issue 6). All benchmarks use synthetic usage_events rows in temp DBs
// (b.TempDir), seeded OUTSIDE the timed region, and never touch the real DB.
//
// Run: go test -bench . -benchmem -benchtime=1x -run '^$'
// 1M-row sub-benchmarks are skipped under -short.

import (
	"database/sql"
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"
	"time"
)

// benchSizes is the row-count matrix. 1_000_000 is opt-in (not -short).
func benchSizes() []int {
	sizes := []int{1_000, 10_000, 100_000}
	if !testing.Short() {
		sizes = append(sizes, 1_000_000)
	}
	return sizes
}

var (
	benchModels   = []string{"gpt-5", "claude-sonnet-4-5", "gemini-2.5-pro", "m-a", "m-b", "m-c", "m-d", "m-e"}
	benchProvider = []string{"openai", "anthropic", "google", "codex"}
	benchSources  = []string{"sk-alpha", "sk-beta", "sk-gamma", "sk-delta"}
	benchAuth     = []string{"api-key", "oauth", "subscription"}
)

// seedBenchDB writes n synthetic usage_events rows into a fresh temp DB and
// returns its path. Rows are spread over the trailing ~30 days with mixed
// models/providers/sources and a partial cache-token mix, matching the
// payload format written by handleUsage. Every junkEvery-th row gets an
// unparseable stored ts + malformed payload (junkEvery<=0 disables). When
// fillTsUnix is false, all rows get ts_unix NULL (legacy state for the
// backfill benchmark); otherwise ts_unix carries the parsed unix seconds.
// Deterministic: seeded rng, so sub-benchmarks are reproducible.
func seedBenchDB(b *testing.B, n, junkEvery int, fillTsUnix bool) string {
	b.Helper()
	path := filepath.Join(b.TempDir(), fmt.Sprintf("bench-%d.sqlite", n))
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Exec("CREATE TABLE usage_events (id INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL, dedup TEXT UNIQUE, payload TEXT, ts_unix REAL)"); err != nil {
		b.Fatal(err)
	}
	rng := rand.New(rand.NewSource(42))
	now := time.Now()
	start := now.AddDate(0, 0, -29)
	span := int64(now.Add(-time.Hour).Sub(start).Seconds())
	tx, err := conn.Begin()
	if err != nil {
		b.Fatal(err)
	}
	st, err := tx.Prepare("INSERT INTO usage_events (ts,dedup,payload,ts_unix) VALUES (?,?,?,?)")
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if junkEvery > 0 && i%junkEvery == 0 {
			if _, err := st.Exec("not-a-time", fmt.Sprintf("dedup-%d", i), "{not json", nil); err != nil {
				b.Fatal(err)
			}
			continue
		}
		ts := start.Add(time.Duration(rng.Int63n(span)) * time.Second)
		model := benchModels[rng.Intn(len(benchModels))]
		provider := benchProvider[rng.Intn(len(benchProvider))]
		source := benchSources[rng.Intn(len(benchSources))]
		auth := benchAuth[rng.Intn(len(benchAuth))]
		input := int64(100 + rng.Intn(2000))
		output := int64(50 + rng.Intn(800))
		var cacheRead, cacheWrite, reasoning int64
		if rng.Intn(4) == 0 {
			cacheRead = input * 3 / 5
		}
		if rng.Intn(20) == 0 {
			cacheWrite = int64(50 + rng.Intn(400))
		}
		if rng.Intn(10) == 0 {
			reasoning = int64(rng.Intn(300))
		}
		failed := rng.Intn(20) == 0
		statusCode := 200
		if failed {
			statusCode = 500
		}
		latency := 200 + rng.Intn(4800)
		ttft := 20 + rng.Intn(780)
		payload := fmt.Sprintf(`{"timestamp":%q,"model":%q,"provider":%q,"source":%q,"failed":%v,"latency_ms":%d,"ttft_ms":%d,"status_code":%d,"auth_type":%q,"tokens":{"input_tokens":%d,"output_tokens":%d,"cache_read_tokens":%d,"cache_creation_tokens":%d,"reasoning_tokens":%d,"total_tokens":%d}}`,
			ts.Format(time.RFC3339Nano), model, provider, source, failed, latency, ttft, statusCode, auth,
			input, output, cacheRead, cacheWrite, reasoning, input+output)
		var tsUnix any
		if fillTsUnix {
			tsUnix = float64(ts.UnixNano()) / 1e9
		}
		if _, err := st.Exec(ts.Format("2006-01-02 15:04:05"), fmt.Sprintf("dedup-%d", i), payload, tsUnix); err != nil {
			b.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		b.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	return path
}

// openBenchDB runs the real openDB (migrations included) on the seeded file
// and fails the benchmark if the connection did not come up.
func openBenchDB(b *testing.B, path string) {
	b.Helper()
	openDB(path)
	mu.Lock()
	ok := db != nil
	mu.Unlock()
	if !ok {
		b.Fatal("openDB did not provide a connection")
	}
}

// preseedEstimates gives every migrated row a stored 'priced' estimate so
// resolveEstimates stays on the stored path — steady-state cost resolution
// (the cold-path INSERT-OR-IGNORE-per-row write storm is a separate issue,
// noted in the report).
func preseedEstimates(b *testing.B) {
	b.Helper()
	mu.Lock()
	defer mu.Unlock()
	if _, err := db.Exec(`INSERT OR IGNORE INTO cost_estimates
		(event_id, model, provider, matched_model, matched_provider, source, usd, status, calculated_at)
		SELECT id, 'm', 'p', 'm', 'p', 'models.dev', 0.000001, 'priced', '2026-01-01T00:00:00Z'
		FROM usage_events WHERE ts_unix > 0`); err != nil {
		b.Fatal(err)
	}
}

// benchRange30d returns [lo, hi) covering the seeded window plus margin.
func benchRange30d() (float64, float64) {
	now := time.Now()
	return float64(now.AddDate(0, 0, -31).Unix()), float64(now.Add(time.Hour).Unix())
}

// benchQuery30d expresses the same window as start/end date params.
func benchQuery30d() map[string][]string {
	now := time.Now()
	return map[string][]string{
		"start": {now.AddDate(0, 0, -30).Format("2006-01-02")},
		"end":   {now.Format("2006-01-02")},
	}
}

func BenchmarkLoadEventsRange30d(b *testing.B) {
	for _, n := range benchSizes() {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			openBenchDB(b, seedBenchDB(b, n, 0, true))
			lo, hi := benchRange30d()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				evs, _, err := loadEventsRange(lo, hi, "", "")
				if err != nil {
					b.Fatal(err)
				}
				_ = evs
			}
		})
	}
}

func BenchmarkAnalyticsOverview30d(b *testing.B) {
	for _, n := range benchSizes() {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			openBenchDB(b, seedBenchDB(b, n, 0, true))
			q := benchQuery30d()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				body, err := analyticsOverview(q)
				if err != nil {
					b.Fatal(err)
				}
				_ = body
			}
		})
	}
}

func BenchmarkAnalyticsHeatmap30d(b *testing.B) {
	for _, n := range benchSizes() {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			openBenchDB(b, seedBenchDB(b, n, 0, true))
			preseedEstimates(b)
			q := benchQuery30d()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				body, err := analyticsHeatmap(q)
				if err != nil {
					b.Fatal(err)
				}
				_ = body
			}
		})
	}
}

func BenchmarkAnalyticsComparison30d(b *testing.B) {
	for _, n := range benchSizes() {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			openBenchDB(b, seedBenchDB(b, n, 0, true))
			preseedEstimates(b)
			q := map[string][]string{"preset": {"30d"}, "mode": {"full"}}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				body, err := analyticsComparison(q)
				if err != nil {
					b.Fatal(err)
				}
				_ = body
			}
		})
	}
}

// BenchmarkBackfillTsUnix migrates n legacy rows (all ts_unix NULL, ~10%
// junk ts) inside the timed region. Re-runs (b.N>1) first reset ts_unix to
// NULL outside the timer.
func BenchmarkBackfillTsUnix(b *testing.B) {
	for _, n := range benchSizes() {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			path := seedBenchDB(b, n, 10, false)
			conn, err := sql.Open("sqlite", path)
			if err != nil {
				b.Fatal(err)
			}
			defer conn.Close()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if i > 0 {
					b.StopTimer()
					if _, err := conn.Exec("UPDATE usage_events SET ts_unix = NULL"); err != nil {
						b.Fatal(err)
					}
					b.StartTimer()
				}
				if err := backfillTsUnix(conn); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
