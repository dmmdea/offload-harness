package llamaclient

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// WithFrequencyPenalty puts the OpenAI-standard `frequency_penalty` on the
// request and nothing else moves; without it (or with a non-positive value) the
// key is absent, so every existing call's body stays byte-identical
// (TestGenerateWithoutOptionOmitsChatTemplateKwargs pins the full string).
func TestGenerateWithFrequencyPenaltySendsItAndOnlyIt(t *testing.T) {
	var raw string
	srv := bodyCapture(t, &raw)
	defer srv.Close()

	c := New(srv.URL, "", "seat-x", 5*time.Second)
	if _, err := c.Generate(context.Background(), GenRequest{System: "sys", User: "hi", Grammar: `root ::= "{}"`, MaxTokens: 64}, WithFrequencyPenalty(0.5)); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode request body (%v): %s", err, raw)
	}
	if fp, ok := body["frequency_penalty"].(float64); !ok || fp != 0.5 {
		t.Fatalf("frequency_penalty = %#v, want 0.5: %s", body["frequency_penalty"], raw)
	}
	if g, _ := body["grammar"].(string); g != `root ::= "{}"` {
		t.Fatalf("grammar = %q, want it untouched by the option", g)
	}
	if mt, _ := body["max_tokens"].(float64); mt != 64 {
		t.Fatalf("max_tokens = %v, want it untouched by the option", body["max_tokens"])
	}
}

func TestGenerateWithANonPositiveFrequencyPenaltyIsNoOption(t *testing.T) {
	for _, p := range []float64{0, -0.5} {
		var raw string
		srv := bodyCapture(t, &raw)
		c := New(srv.URL, "", "seat-x", 5*time.Second)
		_, err := c.Generate(context.Background(), GenRequest{System: "sys", User: "hi", MaxTokens: 64}, WithFrequencyPenalty(p))
		srv.Close()
		if err != nil {
			t.Fatalf("Generate(%v): %v", p, err)
		}
		if strings.Contains(raw, "frequency_penalty") {
			t.Fatalf("penalty %v put a key on the wire: %s", p, raw)
		}
	}
}
