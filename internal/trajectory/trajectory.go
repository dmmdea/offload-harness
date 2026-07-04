// Package trajectory holds the capture queue + label sidecar for the P6 agentic-
// trace flywheel. When enabled (OFF by default), a sampled fraction of completed
// standalone agent goals is appended here as an Item; an offline drain replays
// each goal under a candidate planner prompt, judges goal-satisfaction, and writes
// correctness labels to a sidecar (via ledger.AppendLabel — NEVER the savings
// ledger). The queue mechanics mirror internal/shadow (append-only Enqueue +
// atomic rename-aside Drain with crash recovery + corrupt-line skip).
package trajectory

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
)

// SchemaVersion is the Item format version (bump on a breaking field change).
const SchemaVersion = 1

// Item is one captured agent trajectory — enough to replay + judge it offline.
// The full transcript stays in the standalone trace file, referenced by TracePath.
type Item struct {
	TS         int64    `json:"ts"`
	Schema     int      `json:"schema"`
	ID         string   `json:"id"`
	Goal       string   `json:"goal"`
	Envelope   []string `json:"envelope"` // capabilities granted this run: read/write/fetch/shell
	Tools      []string `json:"tools"`    // tool names called, in order (the trajectory shape)
	Steps      int      `json:"steps"`
	StopReason string   `json:"stop_reason"`
	Output     string   `json:"output"`
	TracePath  string   `json:"trace_path,omitempty"`
}

var mu sync.Mutex

// Capture samples (rate in [0,1]) and, on a hit, appends the item. Returns
// (captured, err). rate<=0 never captures; rate>=1 always. The CALLER must treat
// any error as non-fatal — capture is best-effort and must never affect the run.
func Capture(path string, rate float64, it Item) (bool, error) {
	if path == "" || rate <= 0 {
		return false, nil
	}
	if rate < 1 && rand.Float64() >= rate {
		return false, nil
	}
	return true, Enqueue(path, it)
}

// Enqueue appends one item as a JSON line (concurrency-safe).
func Enqueue(path string, it Item) error {
	mu.Lock()
	defer mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	line, err := json.Marshal(it)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return err
}

// Drain atomically claims the queue (rename aside), reads it, and deletes the
// claim — the cross-process cursor + crash-recovery + corrupt-line-skip pattern
// from internal/shadow, so an item is never lost to a read/truncate race. A
// missing/empty queue returns (nil, nil).
func Drain(path string) ([]Item, error) {
	mu.Lock()
	defer mu.Unlock()
	claim := path + ".draining"
	if _, statErr := os.Stat(claim); statErr != nil {
		if err := os.Rename(path, claim); err != nil {
			if os.IsNotExist(err) {
				return nil, nil // nothing queued
			}
			return nil, nil // e.g. a brief Windows sharing violation — drain next cycle, no loss
		}
	}
	data, err := os.ReadFile(claim)
	if err != nil {
		return nil, err // leave the claim in place for recovery on retry
	}
	var items []Item
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var it Item
		if json.Unmarshal(line, &it) != nil {
			continue // skip a corrupt line, never abort the drain
		}
		items = append(items, it)
	}
	_ = os.Remove(claim)
	return items, nil
}
