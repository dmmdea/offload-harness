// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Acceptance tests for the wave D analytics verbs (residency, saturation), driven
// against a seeded mirror. No live server, no fakeswap needed: these verbs read
// the local SQLite mirror and the config YAML only.

package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"llamaswap-pp-cli/internal/mirror"
	"llamaswap-pp-cli/internal/store"
)

const analyticsTestYAML = `
models:
  "qwen3.8-27b":
    cmd: "llama-server --port ${PORT} -m V:/models/q27.gguf"
    ttl: 300
  "embeddinggemma":
    cmd: "llama-server --port ${PORT} -m V:/models/eg.gguf --embeddings"
    aliases: ["local-embed"]
    ttl: -1
`

type analyticsTestEnv struct {
	dbPath   string
	yamlPath string
}

// seedRequest inserts one mirrored request. ts is RFC3339; durMS is the
// completion-stamped duration (dispatch→done).
func (e *analyticsTestEnv) seedRequest(t *testing.T, db *store.Store, epoch, actID int64, ts, model, path string, status int, durMS, inTok, outTok int64) {
	t.Helper()
	_, err := db.DB().Exec(
		`INSERT INTO requests (epoch_id, activity_id, ts, model, req_path, status, duration_ms, input_tokens, output_tokens, censored)
		 VALUES (?,?,?,?,?,?,?,?,?,0)`,
		epoch, actID, ts, model, path, status, durMS, inTok, outTok)
	if err != nil {
		t.Fatalf("seed request: %v", err)
	}
}

func (e *analyticsTestEnv) seedEpoch(t *testing.T, db *store.Store, id int64, state string, maxAct, totalLast, prepoll int64, dense int) {
	t.Helper()
	_, err := db.DB().Exec(
		`INSERT INTO epochs (epoch_id, witness, first_seen_at, last_seen_at, state, max_activity_id, total_requests_last, ids_dense, loss_prepoll, loss_evicted)
		 VALUES (?, 'test', '2026-08-20T00:00:00Z', '2026-08-20T01:00:00Z', ?, ?, ?, ?, ?, 0)`,
		id, state, maxAct, totalLast, dense, prepoll)
	if err != nil {
		t.Fatalf("seed epoch: %v", err)
	}
}

func newAnalyticsTestEnv(t *testing.T) (*analyticsTestEnv, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "llama-swap.yaml")
	if err := os.WriteFile(yamlPath, []byte(analyticsTestYAML), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	t.Setenv(mirror.EnvYAMLPath, yamlPath)
	t.Setenv(mirror.EnvKeepSet, "")
	t.Setenv("LLAMASWAP_CONFIG", filepath.Join(dir, "config.json")) // absent on purpose
	env := &analyticsTestEnv{dbPath: filepath.Join(dir, "data.db"), yamlPath: yamlPath}

	ctx := context.Background()
	db, err := store.OpenWithContext(ctx, env.dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := store.EnsureDomainSchema(ctx, db.DB()); err != nil {
		t.Fatalf("schema: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return env, db
}

func rfc(base time.Time, addSec int) string {
	return base.Add(time.Duration(addSec) * time.Second).UTC().Format(time.RFC3339)
}

// --- residency: TTL eviction inference + cold-load cost -----------------------

func TestResidency_InfersTTLEvictionAndColdCost(t *testing.T) {
	env, db := newAnalyticsTestEnv(t)
	base := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	env.seedEpoch(t, db, 1, "open", 3, 3, 0, 1)
	// req1 steady (100ms), then a 600s idle gap (> ttl 300) → req2 is a cold load
	// (5000ms). Steady median ~100ms, so cold delta ~4900ms.
	env.seedRequest(t, db, 1, 1, rfc(base, 0), "qwen3.8-27b", "/v1/chat/completions", 200, 100, 10, 20)
	env.seedRequest(t, db, 1, 2, rfc(base, 700), "qwen3.8-27b", "/v1/chat/completions", 200, 5000, 10, 20)
	env.seedRequest(t, db, 1, 3, rfc(base, 705), "qwen3.8-27b", "/v1/chat/completions", 200, 100, 10, 20)

	out, _, err := runSpine(t, "residency", "--json", "--db", env.dbPath)
	if err != nil {
		t.Fatalf("residency: %v\n%s", err, out)
	}
	obj := lastJSONObject(t, out)
	seats := obj["seats"].([]any)
	if len(seats) != 1 {
		t.Fatalf("want 1 seat, got %d: %s", len(seats), out)
	}
	s := seats[0].(map[string]any)
	if got := s["ttl_evictions"].(float64); got != 1 {
		t.Fatalf("ttl_evictions = %v, want 1\n%s", got, out)
	}
	if s["cold_load_p50_ms"] == nil {
		t.Fatalf("expected a cold-load sample, got nil\n%s", out)
	}
	// cold delta should be ~4900ms (5000 - 100 median), well above 4000.
	if got := s["cold_load_p50_ms"].(float64); got < 4000 {
		t.Fatalf("cold_load_p50_ms = %v, want >=4000\n%s", got, out)
	}
}

// TestResidency_IntervalDirectionIsCompletionStamped is the load-bearing
// correctness pin: idle = next.start - prev.end = (next.ts - next.dur) - prev.ts.
// A gap engineered to straddle the ttl boundary depending on direction must be
// classified by the CORRECT (completion-stamped) interval. If someone flips
// Start() to ts+dur, this eviction disappears and the test fails.
func TestResidency_IntervalDirectionIsCompletionStamped(t *testing.T) {
	env, db := newAnalyticsTestEnv(t)
	base := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	env.seedEpoch(t, db, 1, "open", 2, 2, 0, 1)
	// prev ends at t=0 (ts=0, dur ~0). next completes at t=350 with a 100s
	// duration → next.start = 250. Correct idle = 250 - 0 = 250 (< ttl 300) → NO
	// eviction. The WRONG interval [ts,ts+dur] would treat idle as 350 (> 300) →
	// a spurious eviction. Assert 0 evictions.
	env.seedRequest(t, db, 1, 1, rfc(base, 0), "qwen3.8-27b", "/v1/chat/completions", 200, 0, 1, 1)
	env.seedRequest(t, db, 1, 2, rfc(base, 350), "qwen3.8-27b", "/v1/chat/completions", 200, 100000, 1, 1)

	out, _, err := runSpine(t, "residency", "--json", "--db", env.dbPath)
	if err != nil {
		t.Fatalf("residency: %v\n%s", err, out)
	}
	s := lastJSONObject(t, out)["seats"].([]any)[0].(map[string]any)
	if got := s["ttl_evictions"].(float64); got != 0 {
		t.Fatalf("ttl_evictions = %v, want 0 (completion-stamped idle 250s < ttl 300s); a value of 1 means the interval was reversed\n%s", got, out)
	}
}

func TestResidency_KeepSetSeatIsMootAndNotEvicted(t *testing.T) {
	env, db := newAnalyticsTestEnv(t)
	base := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	env.seedEpoch(t, db, 1, "open", 2, 2, 0, 1)
	// embeddinggemma is ttl -1 in the fixture: even a huge gap must not evict.
	env.seedRequest(t, db, 1, 1, rfc(base, 0), "embeddinggemma", "/v1/embeddings", 200, 50, 5, 0)
	env.seedRequest(t, db, 1, 2, rfc(base, 100000), "embeddinggemma", "/v1/embeddings", 200, 50, 5, 0)

	out, _, err := runSpine(t, "residency", "--json", "--db", env.dbPath)
	if err != nil {
		t.Fatalf("residency: %v\n%s", err, out)
	}
	s := lastJSONObject(t, out)["seats"].([]any)[0].(map[string]any)
	if got := s["ttl_evictions"].(float64); got != 0 {
		t.Fatalf("keep-set (ttl -1) seat evictions = %v, want 0\n%s", got, out)
	}
	if note, _ := s["note"].(string); !strings.Contains(note, "moot") {
		t.Fatalf("expected a 'moot' note for ttl -1, got %q\n%s", note, out)
	}
}

func TestResidency_WhatIfRaiseAvoidsReloadsWithCorrectBoundLabels(t *testing.T) {
	env, db := newAnalyticsTestEnv(t)
	base := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	env.seedEpoch(t, db, 1, "open", 4, 4, 0, 1)
	// Two 400s idle gaps (evict at ttl 300, would NOT evict at 900).
	env.seedRequest(t, db, 1, 1, rfc(base, 0), "qwen3.8-27b", "/v1/chat/completions", 200, 100, 1, 1)
	env.seedRequest(t, db, 1, 2, rfc(base, 500), "qwen3.8-27b", "/v1/chat/completions", 200, 8000, 1, 1)
	env.seedRequest(t, db, 1, 3, rfc(base, 1000), "qwen3.8-27b", "/v1/chat/completions", 200, 8000, 1, 1)
	env.seedRequest(t, db, 1, 4, rfc(base, 1005), "qwen3.8-27b", "/v1/chat/completions", 200, 100, 1, 1)

	out, _, err := runSpine(t, "residency", "--ttl", "qwen3.8-27b=900", "--json", "--db", env.dbPath)
	if err != nil {
		t.Fatalf("residency what-if: %v\n%s", err, out)
	}
	wi := lastJSONObject(t, out)["what_if"].([]any)
	if len(wi) != 1 {
		t.Fatalf("want 1 what_if, got %d\n%s", len(wi), out)
	}
	w := wi[0].(map[string]any)
	if got := w["reloads_avoided_ceiling"].(float64); got != 2 {
		t.Fatalf("reloads_avoided_ceiling = %v, want 2\n%s", got, out)
	}
	if _, ok := w["cold_minutes_saved_ceiling"]; !ok {
		t.Fatalf("missing cold_minutes_saved_ceiling (correct bound label)\n%s", out)
	}
	if _, ok := w["resident_minutes_added_upper_bound"]; !ok {
		t.Fatalf("missing resident_minutes_added_upper_bound (correct bound label)\n%s", out)
	}
	if safe, _ := w["safe_for_keepset_and_groups"].(bool); !safe {
		t.Fatalf("27B is not keep-set and shares no group: want safe=true\n%s", out)
	}
}

// --- saturation --------------------------------------------------------------

func TestSaturation_CountsRejectionsAndOmitsConcurrency(t *testing.T) {
	env, db := newAnalyticsTestEnv(t)
	base := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	env.seedEpoch(t, db, 1, "open", 5, 5, 0, 1)
	env.seedRequest(t, db, 1, 1, rfc(base, 0), "embeddinggemma", "/v1/embeddings", 200, 50, 5, 0)
	env.seedRequest(t, db, 1, 2, rfc(base, 1), "embeddinggemma", "/v1/embeddings", 429, 5, 5, 0)
	env.seedRequest(t, db, 1, 3, rfc(base, 2), "embeddinggemma", "/v1/embeddings", 429, 5, 5, 0)
	env.seedRequest(t, db, 1, 4, rfc(base, 3), "embeddinggemma", "/v1/embeddings", 500, 5, 5, 0)
	env.seedRequest(t, db, 1, 5, rfc(base, 4), "embeddinggemma", "/v1/embeddings", 200, 50, 5, 0)

	out, _, err := runSpine(t, "saturation", "--json", "--db", env.dbPath)
	if err != nil {
		t.Fatalf("saturation: %v\n%s", err, out)
	}
	obj := lastJSONObject(t, out)
	s := obj["seats"].([]any)[0].(map[string]any)
	if got := s["rejections_429"].(float64); got != 2 {
		t.Fatalf("rejections_429 = %v, want 2\n%s", got, out)
	}
	if got := s["server_errors_5xx"].(float64); got != 1 {
		t.Fatalf("server_errors_5xx = %v, want 1\n%s", got, out)
	}
	// Concurrency/in-flight depth must NOT be present as a DATA FIELD
	// (unreconstructable at second resolution). Check keys, not prose — a note
	// explaining the omission legitimately contains the word "concurrency".
	for _, banned := range []string{"in_flight", "inflight", "concurren", "queue_depth", "depth"} {
		if k := findKeyContaining(obj, banned); k != "" {
			t.Fatalf("saturation JSON has key %q (matches %q) — in-flight depth must not be a field at second resolution\n%s", k, banned, out)
		}
	}
	if !obj["coverage"].(map[string]any)["second_resolution_ts"].(bool) {
		t.Fatalf("coverage must flag second_resolution_ts=true\n%s", out)
	}
}

func TestAnalytics_CoveragePctFromPrepollLoss(t *testing.T) {
	env, db := newAnalyticsTestEnv(t)
	base := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	// 9 mirrored rows, epoch reports 1 prepoll loss → coverage 9/10 = 90%.
	env.seedEpoch(t, db, 1, "open", 9, 10, 1, 1)
	for i := 0; i < 9; i++ {
		env.seedRequest(t, db, 1, int64(i+1), rfc(base, i), "qwen3.8-27b", "/v1/chat/completions", 200, 100, 1, 1)
	}
	out, _, err := runSpine(t, "saturation", "--json", "--db", env.dbPath)
	if err != nil {
		t.Fatalf("saturation: %v\n%s", err, out)
	}
	cov := lastJSONObject(t, out)["coverage"].(map[string]any)
	if got := cov["coverage_pct"].(float64); got < 89.9 || got > 90.1 {
		t.Fatalf("coverage_pct = %v, want ~90\n%s", got, out)
	}
}

func TestAnalytics_ColdMirrorSaysSoNotCrash(t *testing.T) {
	env, _ := newAnalyticsTestEnv(t)
	// A bytes.Buffer is not a terminal, so the verbs emit JSON: assert the empty
	// shape (no seats, zero rows) rather than the table-only "no requests" line.
	for _, verb := range []string{"residency", "saturation"} {
		out, _, err := runSpine(t, verb, "--json", "--db", env.dbPath)
		if err != nil {
			t.Fatalf("%s on cold mirror errored: %v\n%s", verb, err, out)
		}
		obj := lastJSONObject(t, out)
		if seats, ok := obj["seats"].([]any); ok && len(seats) != 0 {
			t.Fatalf("%s cold mirror: want no seats, got %d\n%s", verb, len(seats), out)
		}
		cov := obj["coverage"].(map[string]any)
		if got := cov["request_rows"].(float64); got != 0 {
			t.Fatalf("%s cold mirror: request_rows = %v, want 0\n%s", verb, got, out)
		}
	}
	// The table path (terminal) is exercised by dogfood, not the buffer harness.
}

// findKeyContaining walks a decoded JSON value and returns the first object key
// whose lowercased form contains sub, or "" if none.
func findKeyContaining(v any, sub string) string {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if strings.Contains(strings.ToLower(k), sub) {
				return k
			}
			if r := findKeyContaining(child, sub); r != "" {
				return r
			}
		}
	case []any:
		for _, child := range t {
			if r := findKeyContaining(child, sub); r != "" {
				return r
			}
		}
	}
	return ""
}

func TestResidency_RejectsBadTTLOverride(t *testing.T) {
	env, _ := newAnalyticsTestEnv(t)
	_, _, err := runSpine(t, "residency", "--ttl", "nonsense", "--db", env.dbPath)
	if err == nil {
		t.Fatalf("expected an error for malformed --ttl")
	}
	if !strings.Contains(err.Error(), "model=seconds") {
		t.Fatalf("error should explain the format, got: %v", err)
	}
}

// Sanity: the parse helper is total on the documented forms.
func TestParseTTLOverrides(t *testing.T) {
	got, err := parseTTLOverrides([]string{"a=300", "b=0"})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got["a"] != 300 || got["b"] != 0 {
		t.Fatalf("parsed wrong: %v", got)
	}
	for _, bad := range []string{"a", "a=-1", "a=x", "=300"} {
		if _, err := parseTTLOverrides([]string{bad}); err == nil {
			t.Fatalf("%q should be rejected", bad)
		}
	}
}

var _ = fmt.Sprintf
