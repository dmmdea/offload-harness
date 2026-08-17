package core

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestTaskAgentRunValid: the delegation task type is recognized by Valid()
// alongside the existing tasks; an unknown task is still rejected.
func TestTaskAgentRunValid(t *testing.T) {
	if !TaskAgentRun.Valid() {
		t.Fatal("TaskAgentRun should be Valid()")
	}
	if TaskType("nope-agent").Valid() {
		t.Fatal("unknown task must be invalid")
	}
}

// minContract returns the smallest wire-valid contract JSON, with overrides
// merged in. Building test payloads from a known-good base keeps each table
// row about exactly ONE deviation.
func minContract(t *testing.T, overrides map[string]any) string {
	t.Helper()
	m := map[string]any{"schema_version": 1, "goal": "summarize the docs", "depth": 0}
	for k, v := range overrides {
		if v == nil {
			delete(m, k)
			continue
		}
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestDecodeAgentContractRoundTrip: a fully-populated contract survives
// marshal→decode with every field intact — this is the wire, so field loss
// here is silent data loss on every delegation.
func TestDecodeAgentContractRoundTrip(t *testing.T) {
	in := AgentContract{
		SchemaVersion: 1,
		Goal:          "digest the bench logs",
		Context:       []ContextDoc{{Name: "bench.log", Text: "run 1: 42 tok/s"}},
		OutputSchema:  json.RawMessage(`{"type":"object","properties":{"digest":{"type":"string"}},"required":["digest"]}`),
		Acceptance:    []string{"nonempty:digest", "contains:tok/s"},
		Profile:       "research",
		MaxSteps:      8,
		TimeoutSec:    120,
		Depth:         0,
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := DecodeAgentContract(strings.NewReader(string(b)))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Goal != in.Goal || got.Profile != in.Profile || got.MaxSteps != 8 || got.TimeoutSec != 120 || got.Depth != 0 {
		t.Fatalf("scalar fields did not round-trip: %+v", got)
	}
	if len(got.Context) != 1 || got.Context[0].Name != "bench.log" || got.Context[0].Text != "run 1: 42 tok/s" {
		t.Fatalf("context did not round-trip: %+v", got.Context)
	}
	if string(got.OutputSchema) != string(in.OutputSchema) {
		t.Fatalf("output_schema did not round-trip: %s", got.OutputSchema)
	}
	if len(got.Acceptance) != 2 {
		t.Fatalf("acceptance did not round-trip: %v", got.Acceptance)
	}
}

// TestDecodeAgentContractUnknownFieldIgnored (roast delta 4): the reader is
// TOLERANT — an unknown field from a newer peer is ignored, not rejected, so
// staggered node deploys interoperate. Version skew is caught by the explicit
// schema_version check instead, which fails loudly (next test).
func TestDecodeAgentContractUnknownFieldIgnored(t *testing.T) {
	payload := minContract(t, map[string]any{"future_field": "from a 0.64 node", "another": 7})
	got, err := DecodeAgentContract(strings.NewReader(payload))
	if err != nil {
		t.Fatalf("unknown fields must be ignored, got error: %v", err)
	}
	if got.Goal == "" {
		t.Fatal("known fields must still decode alongside ignored unknowns")
	}
}

// TestDecodeAgentContractSchemaVersion: anything but version 1 is refused —
// tolerance covers unknown FIELDS, never an unknown CONTRACT SHAPE.
func TestDecodeAgentContractSchemaVersion(t *testing.T) {
	for _, v := range []any{nil, 0, 2, 99} {
		payload := minContract(t, map[string]any{"schema_version": v})
		if _, err := DecodeAgentContract(strings.NewReader(payload)); err == nil {
			t.Errorf("schema_version=%v: want error, got nil", v)
		}
	}
}

// TestDecodeAgentContractLimits: the strict caps (size, count, required goal,
// depth) reject; the ceilings (steps, timeout) CLAMP per roast delta 5 —
// remote MaxSteps cap is 12, not the pre-roast 24.
func TestDecodeAgentContractLimits(t *testing.T) {
	bigDoc := strings.Repeat("x", AgentContextMaxBytes+1)
	manyDocs := make([]map[string]any, AgentContextMaxDocs+1)
	for i := range manyDocs {
		manyDocs[i] = map[string]any{"name": "doc" + strings.Repeat("a", i+1) + ".txt", "text": "t"}
	}
	tests := []struct {
		name      string
		overrides map[string]any
		wantErr   string // "" = decode must succeed
		check     func(t *testing.T, c AgentContract)
	}{
		{name: "missing goal", overrides: map[string]any{"goal": nil}, wantErr: "goal"},
		{name: "blank goal", overrides: map[string]any{"goal": "   "}, wantErr: "goal"},
		{name: "context total over 256KiB", overrides: map[string]any{"context": []map[string]any{{"name": "big.txt", "text": bigDoc}}}, wantErr: "context"},
		{name: "too many docs", overrides: map[string]any{"context": manyDocs}, wantErr: "context"},
		{name: "negative depth", overrides: map[string]any{"depth": -1}, wantErr: "depth"},
		{name: "max_steps defaulted", overrides: nil, check: func(t *testing.T, c AgentContract) {
			if c.MaxSteps != AgentMaxStepsDefault {
				t.Errorf("MaxSteps = %d, want default %d", c.MaxSteps, AgentMaxStepsDefault)
			}
		}},
		{name: "max_steps over cap clamped to 12", overrides: map[string]any{"max_steps": 50}, check: func(t *testing.T, c AgentContract) {
			if c.MaxSteps != AgentMaxStepsCap {
				t.Errorf("MaxSteps = %d, want cap %d", c.MaxSteps, AgentMaxStepsCap)
			}
		}},
		{name: "max_steps in range kept", overrides: map[string]any{"max_steps": 5}, check: func(t *testing.T, c AgentContract) {
			if c.MaxSteps != 5 {
				t.Errorf("MaxSteps = %d, want 5", c.MaxSteps)
			}
		}},
		{name: "timeout defaulted", overrides: nil, check: func(t *testing.T, c AgentContract) {
			if c.TimeoutSec != AgentTimeoutSecDefault {
				t.Errorf("TimeoutSec = %d, want default %d", c.TimeoutSec, AgentTimeoutSecDefault)
			}
		}},
		{name: "timeout over cap clamped to 900", overrides: map[string]any{"timeout_sec": 5000}, check: func(t *testing.T, c AgentContract) {
			if c.TimeoutSec != AgentTimeoutSecCap {
				t.Errorf("TimeoutSec = %d, want cap %d", c.TimeoutSec, AgentTimeoutSecCap)
			}
		}},
		{name: "timeout in range kept", overrides: map[string]any{"timeout_sec": 60}, check: func(t *testing.T, c AgentContract) {
			if c.TimeoutSec != 60 {
				t.Errorf("TimeoutSec = %d, want 60", c.TimeoutSec)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := DecodeAgentContract(strings.NewReader(minContract(t, tt.overrides)))
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if tt.check != nil {
				tt.check(t, c)
			}
		})
	}
}

// TestAgentContractValidateDocNames: ContextDocs are materialized to files in a
// job-scoped dir on the receiving node (plan Task 4), so a doc name is a future
// FILENAME — traversal shapes and duplicates must die at validation, before any
// node touches its filesystem.
func TestAgentContractValidateDocNames(t *testing.T) {
	base := AgentContract{SchemaVersion: 1, Goal: "g"}
	tests := []struct {
		name string
		docs []ContextDoc
	}{
		{"empty name", []ContextDoc{{Name: "", Text: "t"}}},
		{"path separator slash", []ContextDoc{{Name: "a/b.txt", Text: "t"}}},
		{"path separator backslash", []ContextDoc{{Name: `a\b.txt`, Text: "t"}}},
		{"dot-dot traversal", []ContextDoc{{Name: "..", Text: "t"}}},
		{"windows drive colon", []ContextDoc{{Name: "C:evil", Text: "t"}}},
		{"duplicate names", []ContextDoc{{Name: "a.txt", Text: "1"}, {Name: "a.txt", Text: "2"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base
			c.Context = tt.docs
			if err := c.Validate(); err == nil {
				t.Fatalf("docs %v must fail validation", tt.docs)
			}
		})
	}
	c := base
	c.Context = []ContextDoc{{Name: "notes.md", Text: "fine"}}
	if err := c.Validate(); err != nil {
		t.Fatalf("plain doc name must validate: %v", err)
	}
}

// TestAgentContractValidateOutputSchema (roast delta 3): remote placement
// requires a schema the receiving node can actually CONSTRAIN with — so
// Validate compiles OutputSchema through the same gbnf path the extract task
// uses (properties map → gbnf.FromJSONSchema → ≥1 field). A schema that parses
// as JSON but yields zero grammar fields is the wrong-valid-schema hole the
// roast closed: it would validate here and then defer on every remote run.
func TestAgentContractValidateOutputSchema(t *testing.T) {
	tests := []struct {
		name    string
		schema  string
		wantErr bool
	}{
		{"typed properties compile", `{"type":"object","properties":{"digest":{"type":"string"},"count":{"type":"integer"}},"required":["digest"]}`, false},
		{"enum and array compile", `{"properties":{"verdict":{"enum":["go","kill"]},"items":{"type":"array"}}}`, false},
		{"not json", `{nope`, true},
		{"no properties map", `{"type":"string"}`, true},
		{"empty properties", `{"properties":{}}`, true},
		{"json but not an object", `["a","b"]`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := AgentContract{SchemaVersion: 1, Goal: "g", OutputSchema: json.RawMessage(tt.schema)}
			err := c.Validate()
			if tt.wantErr && err == nil {
				t.Fatalf("schema %s must fail validation", tt.schema)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("schema %s must validate: %v", tt.schema, err)
			}
		})
	}
}

// TestAgentContractValidateAcceptance: acceptance strings are the DSL (roast
// delta 3 — free prose no longer counts), so a malformed check dies at
// validation with a clear reason instead of at merge time on the delegator.
func TestAgentContractValidateAcceptance(t *testing.T) {
	ok := AgentContract{SchemaVersion: 1, Goal: "g", Acceptance: []string{"contains:tok/s", "regex:^[0-9]+"}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid DSL must pass: %v", err)
	}
	bad := AgentContract{SchemaVersion: 1, Goal: "g", Acceptance: []string{"looks correct and complete"}}
	if err := bad.Validate(); err == nil {
		t.Fatal("free-prose acceptance must fail validation")
	}
}

// TestParseAcceptanceCheck: the parser covers the whole v1 vocabulary and
// rejects shapes that could not gate anything (empty substring, min 0, bad
// regex) — an unfalsifiable check silently weakens the verifiability gate,
// which is the one thing Acceptance exists to enforce.
func TestParseAcceptanceCheck(t *testing.T) {
	tests := []struct {
		in      string
		wantErr bool
	}{
		{"contains:tok/s", false},
		{"contains:a:b:c", false}, // arg may itself contain colons
		{"not_contains:as an AI", false},
		{"regex:^v[0-9]+\\.", false},
		{"min_items:findings:3", false},
		{"nonempty:digest", false},
		{"", true},
		{"contains:", true},           // matches everything — cannot gate
		{"not_contains:", true},       // matches nothing — cannot pass
		{"regex:([", true},            // does not compile
		{"min_items:findings:0", true}, // min 0 can never fail
		{"min_items:findings:x", true},
		{"min_items:findings", true},
		{"nonempty:", true},
		{"shouts:loudly", true}, // unknown verb
		{"looks correct", true}, // free prose
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			_, err := ParseAcceptanceCheck(tt.in)
			if tt.wantErr && err == nil {
				t.Fatalf("%q must fail to parse", tt.in)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("%q must parse: %v", tt.in, err)
			}
		})
	}
}

// TestAcceptanceCheckEval: table over every verb × pass/fail, including the
// fail-closed posture — a structural check (min_items/nonempty) against a
// result WITHOUT structured output FAILS with a reason, it is never skipped.
// Quality-first: an unmet precondition is a failed check.
func TestAcceptanceCheckEval(t *testing.T) {
	structured := json.RawMessage(`{"digest":"three findings","findings":["a","b","c"],"empty":"","none":[],"zero":0,"flag":false}`)
	tests := []struct {
		name       string
		check      string
		structured json.RawMessage
		output     string
		wantPass   bool
	}{
		{"contains hit", "contains:tok/s", nil, "rate was 42 tok/s", true},
		{"contains miss", "contains:tok/s", nil, "no rate measured", false},
		{"contains falls back to structured when output empty", "contains:findings", structured, "", true},
		{"not_contains clean", "not_contains:as an AI", nil, "the digest", true},
		{"not_contains dirty", "not_contains:as an AI", nil, "as an AI I cannot", false},
		{"regex hit", "regex:[0-9]+ tok/s", nil, "42 tok/s", true},
		{"regex miss", "regex:[0-9]+ tok/s", nil, "fast enough", false},
		{"min_items met", "min_items:findings:3", structured, "", true},
		{"min_items short", "min_items:findings:4", structured, "", false},
		{"min_items non-array field", "min_items:digest:1", structured, "", false},
		{"min_items missing field", "min_items:absent:1", structured, "", false},
		{"min_items no structured output", "min_items:findings:1", nil, "text only", false},
		{"nonempty string", "nonempty:digest", structured, "", true},
		{"nonempty empty string", "nonempty:empty", structured, "", false},
		{"nonempty empty array", "nonempty:none", structured, "", false},
		{"nonempty missing field", "nonempty:absent", structured, "", false},
		{"nonempty zero number is present", "nonempty:zero", structured, "", true},
		{"nonempty false bool is present", "nonempty:flag", structured, "", true},
		{"nonempty no structured output", "nonempty:digest", nil, "text only", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := ParseAcceptanceCheck(tt.check)
			if err != nil {
				t.Fatalf("parse %q: %v", tt.check, err)
			}
			pass, reason := c.Eval(tt.structured, tt.output)
			if pass != tt.wantPass {
				t.Fatalf("Eval(%q) = %v (%q), want %v", tt.check, pass, reason, tt.wantPass)
			}
			if !pass && reason == "" {
				t.Fatalf("a failed check must carry a reason (%q)", tt.check)
			}
			if pass && reason != "" {
				t.Fatalf("a passing check must not carry a reason (%q gave %q)", tt.check, reason)
			}
		})
	}
}

// TestAgentWireJSONTags: pins the EXACT wire key set of both directions of the
// contract. These names cross machines running different binary versions —
// renaming one is a silent protocol break the compiler cannot catch, so the
// test catches it instead.
func TestAgentWireJSONTags(t *testing.T) {
	keysOf := func(t *testing.T, v any) map[string]bool {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		out := map[string]bool{}
		for k := range m {
			out[k] = true
		}
		return out
	}
	contract := AgentContract{
		SchemaVersion: 1, Goal: "g",
		Context:      []ContextDoc{{Name: "n", Text: "t"}},
		OutputSchema: json.RawMessage(`{"properties":{"a":{"type":"string"}}}`),
		Acceptance:   []string{"nonempty:a"}, Profile: "research",
		MaxSteps: 1, TimeoutSec: 1, Depth: 0,
	}
	wantContract := []string{"schema_version", "goal", "context", "output_schema", "acceptance", "profile", "max_steps", "timeout_sec", "depth"}
	got := keysOf(t, contract)
	for _, k := range wantContract {
		if !got[k] {
			t.Errorf("contract wire key %q missing (got %v)", k, got)
		}
	}
	if len(got) != len(wantContract) {
		t.Errorf("contract emits %d keys, want %d: %v", len(got), len(wantContract), got)
	}

	result := AgentWireResult{
		SchemaVersion: 1, NodeID: "lenovo", Seat: "offload-e4b", Output: "o",
		Structured: json.RawMessage(`{}`), Steps: 3, StopReason: "done",
		Deferred: true, Reason: "r", WallMs: 12, TokensOut: 9,
	}
	wantResult := []string{"schema_version", "node_id", "seat", "output", "structured", "steps", "stop_reason", "deferred", "reason", "wall_ms", "tokens_out"}
	got = keysOf(t, result)
	for _, k := range wantResult {
		if !got[k] {
			t.Errorf("result wire key %q missing (got %v)", k, got)
		}
	}
	if len(got) != len(wantResult) {
		t.Errorf("result emits %d keys, want %d: %v", len(got), len(wantResult), got)
	}
}
