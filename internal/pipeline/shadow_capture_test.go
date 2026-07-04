package pipeline

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/shadow"
)

func TestCaptureShadow_GatesAndWrites(t *testing.T) {
	q := filepath.Join(t.TempDir(), "shadow-queue.jsonl")
	p := &Pipeline{cfg: config.Config{ShadowEnabled: true, ShadowRate: 1, ShadowQueuePath: q}}

	// eligible: classify, entry tier (Escalations=0), no escalation
	e := ledger.Entry{Task: "classify", ModelTier: "gemma4-e2b", Escalations: 0, Feat: map[string]float64{"len_chars": 5}}
	req := core.Request{Input: "hello there", Params: map[string]any{"labels": []any{"greet", "other"}}}
	p.captureShadow(req, e, core.Result{Data: json.RawMessage(`{"label":"greet"}`)})

	// ineligible: escalated row must NOT be captured
	e2 := ledger.Entry{Task: "classify", ModelTier: "gemma4-e2b", Escalations: 1}
	p.captureShadow(core.Request{Input: "x"}, e2, core.Result{})

	// ineligible task
	e3 := ledger.Entry{Task: "transcribe", Escalations: 0}
	p.captureShadow(core.Request{Input: "y"}, e3, core.Result{})

	items, err := shadow.Drain(q)
	if err != nil {
		t.Fatal(err)
	}
	const wantOutput = `{"label":"greet"}`
	if len(items) != 1 || items[0].Task != "classify" || items[0].Input != "hello there" || items[0].EntryOutput != wantOutput {
		t.Fatalf("expected exactly the eligible classify row with EntryOutput=%q, got %+v", wantOutput, items)
	}
	// Params must be captured so the drain can reconstruct a valid tasks.Build request
	if items[0].Params == nil {
		t.Fatal("captured item must carry Params; got nil")
	}
	labels, ok := items[0].Params["labels"].([]any)
	if !ok || len(labels) != 2 {
		t.Fatalf("captured classify labels not preserved: %+v", items[0].Params)
	}
}

func TestCaptureShadow_Disabled(t *testing.T) {
	q := filepath.Join(t.TempDir(), "shadow-queue.jsonl")
	// ShadowEnabled=false — nothing should be written
	p := &Pipeline{cfg: config.Config{ShadowEnabled: false, ShadowRate: 1, ShadowQueuePath: q}}

	e := ledger.Entry{Task: "classify", Escalations: 0}
	p.captureShadow(core.Request{Input: "hello"}, e, core.Result{Data: json.RawMessage(`{"label":"x"}`)})

	items, err := shadow.Drain(q)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("expected 0 items when disabled, got %d", len(items))
	}
}

func TestCaptureShadow_RateZero(t *testing.T) {
	q := filepath.Join(t.TempDir(), "shadow-queue.jsonl")
	// ShadowRate=0 — rand.Float64() >= 0 is always true, so nothing captured
	p := &Pipeline{cfg: config.Config{ShadowEnabled: true, ShadowRate: 0, ShadowQueuePath: q}}

	e := ledger.Entry{Task: "triage", Escalations: 0}
	p.captureShadow(core.Request{Input: "hello"}, e, core.Result{Data: json.RawMessage(`{"decision":"yes"}`)})

	items, err := shadow.Drain(q)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("expected 0 items at rate=0, got %d", len(items))
	}
}

func TestCaptureShadow_CapturesSummarize(t *testing.T) {
	q := filepath.Join(t.TempDir(), "shadow-queue.jsonl")
	p := &Pipeline{cfg: config.Config{ShadowEnabled: true, ShadowRate: 1, ShadowQueuePath: q}}
	e := ledger.Entry{Task: "summarize", ModelTier: "offload-e4b", Escalations: 0, Feat: map[string]float64{"len_chars": 200}}
	p.captureShadow(core.Request{Input: "a long doc ..."}, e, core.Result{Data: json.RawMessage(`{"summary":"short"}`)})
	items, err := shadow.Drain(q)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Task != "summarize" {
		t.Fatalf("expected summarize captured, got %+v", items)
	}
}
