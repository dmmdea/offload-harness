package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/vllmseat"
)

// Every vllm_seat in the committed tier table must be renderable. A seat that is
// half-specified fails on someone's box, where the symptom is a systemd unit that
// will not start or an agent lane routed at a model llama-swap never serves — so it
// is refused here instead, in a test over the table itself.
func TestEveryDeclaredVLLMSeatValidates(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("setup", "templates", "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Profiles map[string]struct {
			ConfigSeed map[string]json.RawMessage `json:"config_seed"`
			VLLMSeat   *vllmseat.Spec             `json:"vllm_seat"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("profiles.json is not valid JSON: %v", err)
	}
	if len(doc.Profiles) == 0 {
		t.Fatal("no profiles parsed — the schema moved and this gate went blind")
	}
	declared := 0
	for tier, p := range doc.Profiles {
		if p.VLLMSeat == nil {
			continue
		}
		declared++
		if err := p.VLLMSeat.Validate(tier); err != nil {
			t.Errorf("%v", err)
		}
		// config_seed must carry the FALLBACK, not the vLLM seat. A box without the
		// hand-built venv reads config_seed, and naming the vLLM seat there would
		// point the agent lane at a model that box cannot serve.
		if rawModel, ok := p.ConfigSeed["agent_model"]; ok {
			var got string
			if err := json.Unmarshal(rawModel, &got); err == nil && got == p.VLLMSeat.ID {
				t.Errorf("tier %s seeds config_seed.agent_model = %q, the vLLM seat id — config_seed is what a box "+
					"WITHOUT the venv reads, so it must name the fallback (%s) instead",
					tier, got, p.VLLMSeat.Fallback)
			}
		}
	}
	if declared == 0 {
		t.Skip("no tier declares a vllm_seat yet")
	}
	t.Logf("validated %d declared vLLM seat(s)", declared)
}
