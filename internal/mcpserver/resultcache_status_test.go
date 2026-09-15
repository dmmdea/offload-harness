package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/cache"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// resultCacheView runs offload_status against a server whose pipeline carries
// the given cache handle and returns just the reuse.result_cache block.
func resultCacheView(t *testing.T, cfg config.Config, ca *cache.Cache) map[string]any {
	t.Helper()
	t.Setenv("NVIDIA_API_KEY", "")
	t.Setenv("NGC_API_KEY", "")
	s := New(pipeline.New(cfg, nil, ca, nil))
	res, err := s.handleStatus(context.Background(), callReq(`{}`))
	if err != nil {
		t.Fatalf("handleStatus: %v", err)
	}
	m := decodeResult(t, res)
	reuse, _ := m["reuse"].(map[string]any)
	if reuse == nil {
		t.Fatalf("status has no reuse section: %v", m)
	}
	rc, _ := reuse["result_cache"].(map[string]any)
	if rc == nil {
		t.Fatalf("status has no reuse.result_cache: %v", reuse)
	}
	return rc
}

// TestStatusReportsAnUnopenedCacheWithoutOpeningIt is the D-05 status contract.
// Before this change the only signal that a server had lost the shared cache was
// one stderr line an MCP stdio client never sees; now the mode is published —
// and, just as importantly, ASKING must not be what takes the lock, or the
// status tool becomes a sibling factory in its own right.
func TestStatusReportsAnUnopenedCacheWithoutOpeningIt(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.CachePath = filepath.Join(dir, "cache.db")

	ca := cache.New(cfg.CachePath)
	defer ca.Close()
	rc := resultCacheView(t, cfg, ca)

	if got := rc["mode"]; got != string(cache.ModeUnopened) {
		t.Errorf("mode = %v, want %q", got, cache.ModeUnopened)
	}
	if got := rc["available"]; got != false {
		t.Errorf("available = %v, want false for an unopened handle", got)
	}
	if got := rc["configured"]; got != cfg.CachePath {
		t.Errorf("configured = %v, want %q", got, cfg.CachePath)
	}
	if got, ok := rc["siblings"].(float64); !ok || got != 0 {
		t.Errorf("siblings = %v, want 0", rc["siblings"])
	}
	if got, ok := rc["sibling_bytes"].(float64); !ok || got != 0 {
		t.Errorf("sibling_bytes = %v, want 0", rc["sibling_bytes"])
	}
	if rc["note"] == nil {
		t.Error("an unopened cache must carry a note saying it is lazy, not broken")
	}
	if ca.Mode() != cache.ModeUnopened {
		t.Errorf("status resolved the handle (mode=%q); reporting must take no lock", ca.Mode())
	}
	// The whole point: no file was created by asking.
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Fatalf("offload_status created %d file(s) in the cache dir; it must create none", len(ents))
	}
}

// TestStatusReportsTheSiblingCensus: the sibling count and bytes are how the
// D-05 regression is detected from inside the harness. A status that reports
// "fallback: true" but not how much litter is on disk was the old blind spot —
// 49 files accumulated with nothing reporting them.
func TestStatusReportsTheSiblingCensus(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.CachePath = filepath.Join(dir, "cache.db")

	// Another harness process holds the primary.
	holder, err := cache.Open(cfg.CachePath)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()

	ca := cache.New(cfg.CachePath)
	defer ca.Close()
	if err := ca.Put("k", []byte("v")); err != nil { // resolves to this pid's sibling
		t.Fatal(err)
	}
	if ca.Mode() != cache.ModeSibling {
		t.Fatalf("setup: mode=%q, want a sibling", ca.Mode())
	}

	rc := resultCacheView(t, cfg, ca)
	if got := rc["mode"]; got != string(cache.ModeSibling) {
		t.Errorf("mode = %v, want %q", got, cache.ModeSibling)
	}
	if got := rc["fallback"]; got != true {
		t.Errorf("fallback = %v, want true", got)
	}
	if got := rc["available"]; got != true {
		t.Errorf("available = %v, want true — a sibling still serves in-loop hits", got)
	}
	if got := rc["path"]; got != ca.Path() {
		t.Errorf("path = %v, want the sibling %q", got, ca.Path())
	}
	n, ok := rc["siblings"].(float64)
	if !ok || n != 1 {
		t.Fatalf("siblings = %v, want 1", rc["siblings"])
	}
	b, ok := rc["sibling_bytes"].(float64)
	if !ok || b <= 0 {
		t.Errorf("sibling_bytes = %v, want the sibling's real size", rc["sibling_bytes"])
	}
}

// TestStatusCountsTheSharedCacheReadOnly exercises the read-only path in
// production: with nothing holding the primary, status answers "how much is
// actually cached" through a read-through reader — which must not create a file
// and must not leave a sibling behind.
func TestStatusCountsTheSharedCacheReadOnly(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.CachePath = filepath.Join(dir, "cache.db")

	seed, err := cache.Open(cfg.CachePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"a", "b", "c"} {
		if err := seed.Put(k, []byte("v")); err != nil {
			t.Fatal(err)
		}
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	ca := cache.New(cfg.CachePath) // lazy, never used
	defer ca.Close()
	rc := resultCacheView(t, cfg, ca)

	got, ok := rc["entries"].(float64)
	if !ok || got != 3 {
		t.Errorf("entries = %v, want 3 read through the read-only handle", rc["entries"])
	}
	if from := rc["entries_from"]; from != cfg.CachePath {
		t.Errorf("entries_from = %v, want the configured primary %q", from, cfg.CachePath)
	}
	if n, _ := cache.SiblingStats(cfg.CachePath); n != 0 {
		t.Errorf("the read-only count left %d sibling(s) behind", n)
	}
	if ca.Mode() != cache.ModeUnopened {
		t.Errorf("counting resolved the server's own handle (mode=%q)", ca.Mode())
	}
}
