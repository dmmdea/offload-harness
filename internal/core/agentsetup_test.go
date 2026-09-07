package core

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestValidateAgentSetupActionsShape(t *testing.T) {
	ok := []AgentSetupAction{
		{Tool: "read_file", Args: json.RawMessage(`{"path":"notes.md"}`)},
		{Tool: "list_dir"}, // absent args = "{}"
		{Tool: "search_files", Args: json.RawMessage(`null`)}, // null = "{}"
	}
	if err := ValidateAgentSetupActions(ok); err != nil {
		t.Fatalf("valid list rejected: %v", err)
	}
	if got := ok[1].ArgsJSON(); got != "{}" {
		t.Fatalf("absent args must dispatch as {}, got %q", got)
	}
	if got := ok[2].ArgsJSON(); got != "{}" {
		t.Fatalf("null args must dispatch as {}, got %q", got)
	}
	if err := ValidateAgentSetupActions(nil); err != nil {
		t.Fatalf("nil list must validate: %v", err)
	}

	bad := map[string][]AgentSetupAction{
		"too many":        make([]AgentSetupAction, AgentSetupActionsMax+1),
		"empty tool":      {{Tool: ""}},
		"whitespace tool": {{Tool: " read_file"}},
		"inner space":     {{Tool: "read file"}},
		"array args":      {{Tool: "read_file", Args: json.RawMessage(`["notes.md"]`)}},
		"scalar args":     {{Tool: "read_file", Args: json.RawMessage(`"notes.md"`)}},
		"broken args":     {{Tool: "read_file", Args: json.RawMessage(`{"path":`)}},
		"huge args":       {{Tool: "read_file", Args: json.RawMessage(`{"path":"` + strings.Repeat("x", AgentSetupArgsMaxBytes) + `"}`)}},
	}
	for i := range bad["too many"] {
		bad["too many"][i] = AgentSetupAction{Tool: "list_dir"}
	}
	for name, list := range bad {
		err := ValidateAgentSetupActions(list)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !errors.Is(err, ErrAgentSetupActions) {
			t.Errorf("%s: error %v does not wrap ErrAgentSetupActions", name, err)
		}
	}
}

func TestAgentContractValidateCoversSetupActions(t *testing.T) {
	c := AgentContract{SchemaVersion: AgentWireSchemaVersion, Goal: "g",
		SetupActions: []AgentSetupAction{{Tool: "read file"}}}
	err := c.Validate()
	if err == nil || !errors.Is(err, ErrAgentSetupActions) {
		t.Fatalf("contract Validate must reject a bad setup action by class, got %v", err)
	}
	c.SetupActions = []AgentSetupAction{{Tool: "read_file", Args: json.RawMessage(`{"path":"a.md"}`)}}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid setup action rejected: %v", err)
	}
}

func TestDecodeAgentContractCarriesSetupActions(t *testing.T) {
	body := `{"schema_version":1,"goal":"g","setup_actions":[{"tool":"read_file","args":{"path":"notes.md"}},{"tool":"list_dir"}]}`
	c, err := DecodeAgentContract(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.SetupActions) != 2 || c.SetupActions[0].Tool != "read_file" || c.SetupActions[0].ArgsJSON() != `{"path":"notes.md"}` || c.SetupActions[1].ArgsJSON() != "{}" {
		t.Fatalf("setup_actions = %+v", c.SetupActions)
	}
	// Round trip: the field survives marshal → decode with its wire name.
	b, _ := json.Marshal(c)
	if !strings.Contains(string(b), `"setup_actions":[{"tool":"read_file","args":{"path":"notes.md"}}`) {
		t.Fatalf("wire shape: %s", b)
	}
	var back AgentContract
	if err := json.Unmarshal(b, &back); err != nil || len(back.SetupActions) != 2 {
		t.Fatalf("round trip: %v %+v", err, back.SetupActions)
	}
	// Absent = omitted, so a contract without the field is byte-identical to
	// the previous wire.
	c.SetupActions = nil
	if b, _ := json.Marshal(c); strings.Contains(string(b), "setup_actions") {
		t.Fatalf("empty setup_actions must be omitted: %s", b)
	}
}

func TestSeedContextReadsOnePerDocInOrder(t *testing.T) {
	if got := SeedContextReads(nil); got != nil {
		t.Fatalf("no docs must seed nothing, got %+v", got)
	}
	docs := []ContextDoc{{Name: "b.md", Text: "x"}, {Name: "a.md", Text: "y"}}
	got := SeedContextReads(docs)
	if len(got) != 2 || got[0].Tool != "read_file" || got[0].ArgsJSON() != `{"path":"b.md"}` || got[1].ArgsJSON() != `{"path":"a.md"}` {
		t.Fatalf("seed = %+v", got)
	}
	if err := ValidateAgentSetupActions(got); err != nil {
		t.Fatalf("seeded list must validate: %v", err)
	}
	many := make([]ContextDoc, AgentSetupActionsMax+3)
	for i := range many {
		many[i] = ContextDoc{Name: "d" + string(rune('a'+i)) + ".md", Text: "t"}
	}
	if got := SeedContextReads(many); len(got) != AgentSetupActionsMax {
		t.Fatalf("seed must stop at the max (%d), got %d", AgentSetupActionsMax, len(got))
	}
}

func TestAgentTraceStepAndWireCarrySetup(t *testing.T) {
	b, _ := json.Marshal(AgentTraceStep{Step: 0, Tool: "read_file", Status: "committed", ObsChars: 3, Setup: true})
	if !strings.Contains(string(b), `"setup":true`) {
		t.Fatalf("trace step: %s", b)
	}
	b, _ = json.Marshal(AgentTraceStep{Step: 1, Tool: "read_file", Status: "committed"})
	if strings.Contains(string(b), "setup") {
		t.Fatalf("a model call must not carry setup: %s", b)
	}
	b, _ = json.Marshal(AgentWireResult{SchemaVersion: 1, SetupRan: 2})
	if !strings.Contains(string(b), `"setup_ran":2`) {
		t.Fatalf("wire: %s", b)
	}
	b, _ = json.Marshal(AgentWireResult{SchemaVersion: 1})
	if strings.Contains(string(b), "setup_ran") {
		t.Fatalf("zero setup_ran must be omitted (an old node's silence and a no-setup run look the same on purpose): %s", b)
	}
}
