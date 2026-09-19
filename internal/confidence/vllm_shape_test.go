package confidence

import (
	"math"
	"testing"

	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// Register D-128: the token stream a vLLM seat answers a classify call with
// (measured 2026-09-18, Qube pair seat: `{`, `\n  "label": "`, `billing`, …)
// must yield a non-zero decision margin — the shape is the OpenAI one the
// client decodes, so the gate can fire on a vLLM seat exactly as on llama.cpp.
func TestMeasuredVLLMTokenStreamYieldsAMargin(t *testing.T) {
	toks := []llamaclient.TokenLogprob{
		{Token: "{", Top: []llamaclient.AltToken{{Token: "{", Logprob: -0.18}, {Token: "```", Logprob: -1.8}}},
		{Token: "\n  \"label\": \"", Top: []llamaclient.AltToken{{Token: "\n  \"label\": \"", Logprob: -0.01}}},
		{Token: "billing", Top: []llamaclient.AltToken{
			{Token: "billing", Logprob: -0.02},
			{Token: "technical", Logprob: -4.0},
			{Token: "sales", Logprob: -6.0},
		}},
		{Token: "\",\n  \"confidence\": 0.98\n}", Top: nil},
	}
	m, ok := Margin(toks, "label", []string{"billing", "technical", "sales"})
	if !ok || m <= 0 {
		t.Fatalf("margin = %v ok=%v; the measured vLLM stream must produce a decision margin", m, ok)
	}
	// e^-0.02 vs e^-4.0 over the three declared classes: a decisive answer.
	want := (math.Exp(-0.02) - math.Exp(-4.0)) / (math.Exp(-0.02) + math.Exp(-4.0) + math.Exp(-6.0))
	if math.Abs(m-want) > 1e-9 {
		t.Fatalf("margin = %v, want %v from the three declared classes", m, want)
	}
}
