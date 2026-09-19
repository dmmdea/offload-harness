package pipeline

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// Register D-129, found by the A-100 proof contract (2026-09-18 23:42, Lenovo
// 35B seat, three re-pack attempts all "got string, want object"): the re-pack
// handed vLLM `gbnf.JSONSchema(fields)` — the GBNF-typed projection of the
// contract's schema — and internal/gbnf has no object or array-of-object type,
// so a contract whose schema nests objects was CONSTRAINED into the wrong
// shape on every vLLM seat, where before D-129 the ignored grammar at least
// let the model answer freely. vLLM accepts full JSON Schema: the re-pack on
// a vLLM seat sends the contract's own schema. llama.cpp seats keep the GBNF
// projection (the documented ADR 0002 limit).
func TestRepackSendsTheContractsOwnSchemaToAVLLMSeat(t *testing.T) {
	const vseat = "qwen3.8-27b-vllm"
	fake := &structuredSwapFake{
		models: []string{vseat},
		answer: `{"layers":[{"name":"single","tier":"blackwell-16"}],"rows":["r1"]}`,
	}
	srv := fake.server(t)

	cfg := config.Default()
	cfg.Endpoint = srv.URL
	cfg.Model = vseat
	cfg.TriageModel = vseat
	cfg.EscalationModel = ""
	cfg.MaxRetries = 0
	cfg.ThresholdsPath, cfg.RouterWeightsPath, cfg.TierOverridesPath, cfg.ConfHeadLabelsPath, cfg.CachePath = "", "", "", "", ""
	cfg.VLLMSeats = []string{vseat}
	ledgerPath := filepath.Join(t.TempDir(), "ledger.jsonl")
	cfg.LedgerPath = ledgerPath
	led, err := ledger.Open(ledgerPath)
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	defer led.Close()
	p := New(cfg, llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second), nil, led)

	schema := json.RawMessage(`{"type":"object","properties":{
		"layers":{"type":"array","items":{"type":"object","properties":{"name":{"type":"string"},"tier":{"type":"string"}},"required":["name","tier"]}},
		"rows":{"type":"array","items":{"type":"string"}}},"required":["layers","rows"]}`)
	if _, _, _, _, rerr := p.repackStructured(context.Background(), vseat, schema, "digest text", 0); rerr != nil {
		t.Fatalf("repackStructured on the vLLM seat: %v", rerr)
	}

	bodies := fake.bodiesFor(vseat)
	if len(bodies) == 0 {
		t.Fatal("no body reached the vLLM seat")
	}
	for i, b := range bodies {
		so, ok := b["structured_outputs"].(map[string]any)
		if !ok {
			t.Fatalf("body %d carries no structured_outputs: %v", i, b)
		}
		js, _ := so["json"].(map[string]any)
		props, _ := js["properties"].(map[string]any)
		layers, _ := props["layers"].(map[string]any)
		items, _ := layers["items"].(map[string]any)
		if items["type"] != "object" {
			t.Fatalf("body %d: structured_outputs.json flattened the contract's schema — layers.items = %v, want the contract's object items", i, items)
		}
		if _, hasGrammar := b["grammar"]; hasGrammar {
			t.Fatalf("body %d: a vLLM seat still received grammar", i)
		}
	}
}
