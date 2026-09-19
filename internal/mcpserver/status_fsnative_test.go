package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
)

// TestStatusReadsFSNativeReachabilityFromTheSeatStatusFile (register B-29,
// 2026-09-18). An fs_native store is a mounted path with no port to dial, so
// `reachable` was published as null with a "not validated" note in every release.
// The seat wrapper already decides the fact at every seat start (mount + write
// probe) and writes seat-l2.status; status now reads that file when the binding
// declares it: ok → reachable:true, degraded → reachable:false with the reason,
// missing → null with a note, undeclared → null with the instruction.
func TestStatusReadsFSNativeReachabilityFromTheSeatStatusFile(t *testing.T) {
	dir := t.TempDir()
	statusFile := filepath.Join(dir, "seat-l2.status")
	cfg := config.Default()
	binding := &config.KVCacheServer{Enabled: true, Store: "fs_native", Address: "/mnt/kvcache/lmcache-seat", Seat: "qwen3.8-27b-vllm", KeyPrefix: "qube-seat-fs", StatusFile: statusFile}
	cfg.KVCacheServers = config.KVCacheServers{binding}
	cfg.VLLMSeats = []string{"qwen3.8-27b-vllm"}
	row := func() map[string]any {
		v := kvCacheServerView(context.Background(), cfg)
		rows, _ := v["bindings"].([]map[string]any)
		if len(rows) != 1 {
			t.Fatalf("one binding must be listed, got %v", v["bindings"])
		}
		return rows[0]
	}
	// Declared but never written: the seat has not started since — say so, never guess.
	r := row()
	if r["reachable"] != nil || r["status_file"] != statusFile || r["reachable_note"] == nil {
		t.Fatalf("missing status file must report reachable:null with the file and a note, got %v", r)
	}
	// The wrapper's ok line: mounted + write probe passed.
	stamp := time.Now().Add(-90 * time.Second).Format(time.RFC3339)
	if err := os.WriteFile(statusFile, []byte("ok "+stamp+" mbps=412\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r = row()
	if r["reachable"] != true || r["status_line"] != "ok "+stamp+" mbps=412" {
		t.Fatalf("ok line must report reachable:true with the line, got %v", r)
	}
	if age, _ := r["status_age_s"].(int); age < 80 || age > 200 {
		t.Fatalf("status_age_s must be the line's age (~90 s), got %v", r["status_age_s"])
	}
	// The wrapper's degraded line: the store served nothing since that start.
	if err := os.WriteFile(statusFile, []byte("degraded "+stamp+" reason=share //cache/kvcache did not mount at /mnt/kvcache: port 445 closed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r = row()
	if r["reachable"] != false || r["reachable_error"] != "share //cache/kvcache did not mount at /mnt/kvcache: port 445 closed" {
		t.Fatalf("degraded line must report reachable:false with the reason, got %v", r)
	}
	// No status_file declared: the old null, with the instruction instead of "not validated in this release".
	binding.StatusFile = ""
	r = row()
	if r["reachable"] != nil || r["status_file"] != nil {
		t.Fatalf("undeclared status file must report reachable:null and no status_file, got %v", r)
	}
}
