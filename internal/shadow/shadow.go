// Package shadow holds the capture-queue for the nightly shadow-labeling
// flywheel. At request time a small sample of offload calls is appended here
// (input + the entry tier's output); the nightly drain replays a counterfactual
// tier and writes training labels. The queue is purged on drain.
package shadow

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

type Item struct {
	TS          int64              `json:"ts"`
	Task        string             `json:"task"`
	Input       string             `json:"input"`
	Params      map[string]any     `json:"params,omitempty"`
	EntryTier   string             `json:"entry_tier"`
	EntryOutput string             `json:"entry_output"`
	Feat        map[string]float64 `json:"feat"`
}

var mu sync.Mutex

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
// claimed file. The rename is the cross-process cursor: any Enqueue after the
// claim writes to a fresh queue file and is processed by the next Drain, so an
// item is never lost to a read/truncate race even across processes. A missing
// or empty queue returns (nil, nil); a corrupt line is skipped; on a leftover
// claim from a crashed prior drain, that claim is recovered first.
func Drain(path string) ([]Item, error) {
	mu.Lock()
	defer mu.Unlock()
	claim := path + ".draining"
	// Recover a claim left by a previously-crashed drain; else claim the live queue.
	if _, statErr := os.Stat(claim); statErr != nil {
		if err := os.Rename(path, claim); err != nil {
			if os.IsNotExist(err) {
				return nil, nil // nothing queued
			}
			// e.g. a Windows sharing violation while an Enqueue briefly holds the
			// file: skip this cycle; the items drain next time (no loss).
			return nil, nil
		}
	}
	data, err := os.ReadFile(claim)
	if err != nil {
		return nil, err // leave the claim file in place for recovery on retry
	}
	var items []Item
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var it Item
		if err := json.Unmarshal(line, &it); err != nil {
			continue // skip a corrupt line, never abort the drain
		}
		items = append(items, it)
	}
	_ = os.Remove(claim)
	return items, nil
}
