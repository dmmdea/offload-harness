package shadow

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnqueueThenDrainEmptiesQueue(t *testing.T) {
	p := filepath.Join(t.TempDir(), "shadow-queue.jsonl")
	if err := Enqueue(p, Item{TS: 1, Task: "classify", Input: "hello", Params: map[string]any{"labels": []any{"a", "b"}}, EntryTier: "gemma4-e2b", EntryOutput: `{"label":"greet"}`, Feat: map[string]float64{"len_chars": 5}}); err != nil {
		t.Fatal(err)
	}
	if err := Enqueue(p, Item{TS: 2, Task: "triage", Input: "is this spam?", Params: map[string]any{"question": "is it spam?"}, EntryTier: "gemma4-e2b", EntryOutput: `{"decision":"no"}`}); err != nil {
		t.Fatal(err)
	}
	items, err := Drain(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Task != "classify" || items[1].Input != "is this spam?" {
		t.Fatalf("bad drain: %+v", items)
	}
	// Params survive the JSON round-trip
	if q, ok := items[1].Params["question"].(string); !ok || q != "is it spam?" {
		t.Fatalf("triage Params not preserved through round-trip: %+v", items[1].Params)
	}
	labels, ok := items[0].Params["labels"].([]any)
	if !ok || len(labels) != 2 {
		t.Fatalf("classify labels not preserved through round-trip: %+v", items[0].Params)
	}
	// drain must empty the queue (idempotency)
	again, err := Drain(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("queue not emptied after drain: %d", len(again))
	}
}

func TestDrainMissingFile(t *testing.T) {
	got, err := Drain(filepath.Join(t.TempDir(), "nope.jsonl"))
	if err != nil || got != nil {
		t.Fatalf("missing file should be (nil,nil), got %v %v", got, err)
	}
}

func TestDrainRecoversLeftoverClaim(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "shadow-queue.jsonl")
	// simulate a crashed prior drain: a .draining file with one item, no live queue
	claim := p + ".draining"
	if err := os.WriteFile(claim, []byte(`{"ts":9,"task":"classify","input":"x","entry_tier":"gemma4-e2b","entry_output":"{}"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	items, err := Drain(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].TS != 9 {
		t.Fatalf("expected the leftover claim recovered, got %+v", items)
	}
	if _, err := os.Stat(claim); !os.IsNotExist(err) {
		t.Fatalf("claim file should be removed after recovery")
	}
}
