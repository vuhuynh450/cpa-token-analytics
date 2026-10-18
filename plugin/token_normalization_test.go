package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Keep management tests away from the user's default usage database.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "cpa-test-")
	if err != nil {
		panic(err)
	}
	openDB(filepath.Join(dir, "isolation.sqlite"))
	code := m.Run()
	if db != nil {
		_ = db.Close()
	}
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func TestTokenNormalization(t *testing.T) {
	cases := []struct {
		name, provider                                          string
		input, output, read, cached, creation, reasoning, total int64
		wantInput, wantCache                                    int64
		executor                                                string
	}{
		{"claude_read_and_create", "claude", 0, 2872, 476842, 476842, 1391, 0, 481105, 478233, 476842, ""},
		{"openai_subset", "codex", 100, 20, 80, 80, 0, 5, 120, 100, 80, ""},
		{"openai_legacy_cached_only", "codex", 100, 20, 0, 80, 0, 0, 120, 100, 80, ""},
		{"no_cache", "claude", 100, 20, 0, 0, 0, 0, 120, 100, 0, ""},
		{"claude_read_missing_total", "claude", 0, 20, 80, 80, 0, 0, 0, 80, 80, ""},
		{"claude_creation_only", "claude", 100, 10, 0, 1000, 1000, 0, 1110, 1100, 0, ""},
		{"claude_creation_missing_total", "claude", 0, 10, 0, 1000, 1000, 0, 0, 1000, 0, ""},
		{"gemini_reasoning_equals_cache", "gemini", 100, 20, 80, 80, 0, 80, 200, 100, 80, ""},
		{"claude_nonzero_input", "claude", 100, 20, 10, 10, 5, 4, 135, 115, 10, ""},
		{"claude_legacy_cached", "claude", 10, 20, 0, 80, 0, 0, 110, 90, 80, ""},
		{"claude_inclusive_total", "claude", 100, 20, 80, 80, 0, 0, 120, 100, 80, ""},
		{"claude_via_openai", "openai-compatibility", 100, 20, 80, 80, 0, 80, 200, 100, 80, ""},
		{"claude_executor_override", "claude", 100, 20, 80, 80, 0, 80, 200, 100, 80, "OpenAICompatExecutor"},
		{"aliased_model", "custom", 100, 20, 10, 10, 5, 4, 135, 115, 10, "ClaudeExecutor"},
		{"historical_claude_read", "", 0, 2872, 476842, 476842, 1391, 0, 481105, 478233, 476842, ""},
		{"historical_claude_creation", "", 100, 10, 0, 1000, 1000, 0, 1110, 1100, 0, ""},
		{"historical_missing_total", "", 0, 10, 0, 1000, 1000, 0, 0, 1000, 0, ""},
		{"historical_gemini_collision", "", 100, 20, 80, 80, 0, 80, 200, 100, 80, ""},
		{"historical_ambiguous", "", 100, 20, 10, 10, 0, 0, 130, 100, 10, ""},
		{"historical_nonzero_input", "", 314, 20, 61020690, 61020690, 0, 0, 61021024, 61021004, 61020690, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			openDB(filepath.Join(t.TempDir(), "event.sqlite"))
			payload, err := json.Marshal(map[string]any{"result": map[string]any{
				"Provider": tc.provider, "ExecutorType": tc.executor, "Model": tc.name, "Source": "test-fixture",
				"RequestedAt": "2026-09-10T11:30:00+08:00",
				"Detail": map[string]int64{
					"InputTokens": tc.input, "OutputTokens": tc.output,
					"CacheReadTokens": tc.read, "CachedTokens": tc.cached,
					"CacheCreationTokens": tc.creation, "ReasoningTokens": tc.reasoning,
					"TotalTokens": tc.total,
				},
			}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := handleMethod("usage.handle", payload); err != nil {
				t.Fatal(err)
			}
			var before string
			if err := db.QueryRow("SELECT payload FROM usage_events").Scan(&before); err != nil {
				t.Fatal(err)
			}
			var stored dbEvent
			if err := json.Unmarshal([]byte(before), &stored); err != nil {
				t.Fatal(err)
			}
			if stored.Provider != tc.provider || stored.ExecutorType != tc.executor || toI64(stored.Tokens["input_tokens"]) != tc.input {
				t.Fatalf("raw counters or protocol metadata changed: %s", before)
			}
			var legacy map[string]any
			if err := json.Unmarshal([]byte(before), &legacy); err != nil {
				t.Fatal(err)
			}
			delete(legacy, "provider")
			delete(legacy, "executor_type")
			legacyJSON, err := json.Marshal(legacy)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha1.Sum(legacyJSON)
			var dedup string
			if err := db.QueryRow("SELECT dedup FROM usage_events").Scan(&dedup); err != nil {
				t.Fatal(err)
			}
			if dedup != hex.EncodeToString(digest[:]) {
				t.Fatal("protocol metadata changed historical dedup identity")
			}
			status, _, body := callMgmt(t, "/v0/management/plugins/cpa-token-analytics/api/usage", "GET")
			if status != 200 {
				t.Fatalf("HTTP %d: %s", status, body)
			}
			var response struct {
				Events  []outEvent  `json:"events"`
				Buckets []outBucket `json:"buckets"`
			}
			if err := json.Unmarshal(body, &response); err != nil {
				t.Fatal(err)
			}
			if len(response.Events) != 1 || len(response.Buckets) != 1 {
				t.Fatalf("unexpected response: %s", body)
			}
			e, b := response.Events[0], response.Buckets[0]
			t.Logf("input=%d cache=%d output=%d; expected input=%d cache=%d", e.Input, e.Cache, e.Output, tc.wantInput, tc.wantCache)
			if e.Input != tc.wantInput || e.Cache != tc.wantCache || e.Output != tc.output {
				t.Errorf("event input/cache=(%d,%d), want (%d,%d)", e.Input, e.Cache, tc.wantInput, tc.wantCache)
			}
			if b.Input != tc.wantInput || b.Cache != tc.wantCache || b.Output != tc.output || b.Requests != 1 {
				t.Errorf("bucket input/cache=(%d,%d), want (%d,%d)", b.Input, b.Cache, tc.wantInput, tc.wantCache)
			}
			var after string
			if err := db.QueryRow("SELECT payload FROM usage_events").Scan(&after); err != nil {
				t.Fatal(err)
			}
			if before != after {
				t.Error("query modified stored payload")
			}
		})
	}
}
