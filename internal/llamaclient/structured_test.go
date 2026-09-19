// structured_test.go pins the wire shape of the vLLM constraint field
// (register D-129). The ONE thing that matters: a call that carries a JSON
// schema sends `structured_outputs` and NO `grammar`, and a call that does not
// keeps the raw GBNF exactly as every llama.cpp seat has always received it.
package llamaclient

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestGenerateWithJSONSchemaSendsStructuredOutputsAndNoGrammar reads both
// directions off one recorded body each.
func TestGenerateWithJSONSchemaSendsStructuredOutputsAndNoGrammar(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"label": map[string]any{"type": "string"},
		},
		"required":             []any{"label"},
		"additionalProperties": false,
	}

	t.Run("with the schema", func(t *testing.T) {
		var raw string
		srv := bodyCapture(t, &raw)
		defer srv.Close()

		c := New(srv.URL, "", "qwen3.8-27b-vllm", 5*time.Second)
		if _, err := c.Generate(context.Background(), "", "sys", "hi", `root ::= "{}"`, 64, 0, 0, WithJSONSchema(schema)); err != nil {
			t.Fatalf("Generate: %v", err)
		}

		var body map[string]any
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Fatalf("unmarshal recorded body: %v (%s)", err, raw)
		}
		if _, ok := body["grammar"]; ok {
			t.Errorf("a schema-constrained call still sent `grammar` — the field vLLM discards: %v", body["grammar"])
		}
		so, ok := body["structured_outputs"].(map[string]any)
		if !ok {
			t.Fatalf("no structured_outputs object on the wire; body: %s", raw)
		}
		got, _ := json.Marshal(so["json"])
		want, _ := json.Marshal(schema)
		if string(got) != string(want) {
			t.Errorf("structured_outputs.json = %s, want the schema verbatim %s", got, want)
		}
	})

	t.Run("without the schema", func(t *testing.T) {
		var raw string
		srv := bodyCapture(t, &raw)
		defer srv.Close()

		c := New(srv.URL, "", "gemma-4-e4b", 5*time.Second)
		if _, err := c.Generate(context.Background(), "", "sys", "hi", `root ::= "{}"`, 64, 0, 0); err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if !strings.Contains(raw, `"grammar":"root ::= \"{}\""`) {
			t.Errorf("an option-free call lost its raw GBNF grammar; body: %s", raw)
		}
		if strings.Contains(raw, "structured_outputs") {
			t.Errorf("an option-free call sent structured_outputs, which llama.cpp does not serve; body: %s", raw)
		}
	})

	// An EMPTY schema is not a constraint: the caller could not build one, so
	// the grammar it passed must still ride. Otherwise a schema-compilation
	// miss would silently un-constrain the call on every engine.
	t.Run("an empty schema changes nothing", func(t *testing.T) {
		var raw string
		srv := bodyCapture(t, &raw)
		defer srv.Close()

		c := New(srv.URL, "", "gemma-4-e4b", 5*time.Second)
		if _, err := c.Generate(context.Background(), "", "sys", "hi", `root ::= "{}"`, 64, 0, 0, WithJSONSchema(nil)); err != nil {
			t.Fatalf("Generate: %v", err)
		}
		const want = `{"model":"gemma-4-e4b","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}],"temperature":0,"max_tokens":64,"grammar":"root ::= \"{}\"","cache_prompt":true,"stream":false}`
		if raw != want {
			t.Fatalf("an empty schema moved the payload:\n got: %s\nwant: %s", raw, want)
		}
	})
}

// TestGenerateVisionWithJSONSchemaSendsStructuredOutputs covers the multimodal
// request: ADR 0048 makes vLLM first-class on every seat, a vision seat very
// much included, so the mmChatReq path may not stay on a field vLLM ignores.
func TestGenerateVisionWithJSONSchemaSendsStructuredOutputs(t *testing.T) {
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"has_people": map[string]any{"type": "boolean"}},
		"required":   []any{"has_people"},
	}
	uri := "data:image/png;base64,AAAA"

	for _, tc := range []struct {
		name string
		send func(*Client) error
	}{
		{"GenerateVision", func(c *Client) error {
			_, err := c.GenerateVision(context.Background(), "", "sys", "look", []string{uri}, `root ::= "{}"`, 64, 0, 0, WithJSONSchema(schema))
			return err
		}},
		{"GenerateVisionInterleaved", func(c *Client) error {
			_, err := c.GenerateVisionInterleaved(context.Background(), "", "sys", []string{"<0.0 seconds>"}, []string{uri}, "look", `root ::= "{}"`, 64, 0, 0, WithJSONSchema(schema))
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var raw string
			srv := bodyCapture(t, &raw)
			defer srv.Close()

			if err := tc.send(New(srv.URL, "", "vision-vllm", 5*time.Second)); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			var body map[string]any
			if err := json.Unmarshal([]byte(raw), &body); err != nil {
				t.Fatalf("unmarshal recorded body: %v (%s)", err, raw)
			}
			if _, ok := body["grammar"]; ok {
				t.Errorf("%s still sent `grammar` alongside the schema: %v", tc.name, body["grammar"])
			}
			so, ok := body["structured_outputs"].(map[string]any)
			if !ok {
				t.Fatalf("%s sent no structured_outputs; body: %s", tc.name, raw)
			}
			got, _ := json.Marshal(so["json"])
			want, _ := json.Marshal(schema)
			if string(got) != string(want) {
				t.Errorf("%s: structured_outputs.json = %s, want %s", tc.name, got, want)
			}
		})
	}
}
