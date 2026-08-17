package agent

import (
	"context"
	"encoding/json"
	"testing"
)

func stubTool(name string) Tool {
	return Tool{
		ToolSpec: ToolSpec{Name: name, Description: name, Schema: json.RawMessage(`{"type":"object"}`)},
		Exec:     func(ctx context.Context, args string) (string, error) { return "", nil },
	}
}

// AdvertisedTools must report what the planner ACTUALLY sees, after narrowing.
//
// This exists because the MCP door used to report `len(BuildResult.Tools)` — a
// snapshot taken BEFORE WithProfile runs. WithProfile replaces l.specs with a fresh
// container, so that snapshot never reflects the narrowing: an ampere-6 box seeded
// `research` advertised 3 tools while the response said 11. With the box-level
// agent_profile the narrowing can happen with NO caller action, so a wrong count is
// the caller's only signal and it was lying.
func TestAdvertisedToolsReflectsProfileNarrowing(t *testing.T) {
	// The read-only front-door shape: names drawn from the research subset plus
	// several that profile must drop.
	names := []string{"list_dir", "read_file", "summarize_file", "search_files",
		"offload_summarize", "offload_extract", "offload_vqa"}
	tools := make([]Tool, 0, len(names))
	for _, n := range names {
		tools = append(tools, stubTool(n))
	}

	loop := NewLoop(nil, tools, 12)
	if got := len(loop.AdvertisedTools()); got != len(names) {
		t.Fatalf("before narrowing want %d advertised tools, got %d", len(names), got)
	}

	prof, err := LookupProfile("research")
	if err != nil {
		t.Fatal(err)
	}
	loop.WithProfile(prof)

	got := loop.AdvertisedTools()
	// research keeps only what it lists AND what was registered (narrow-only).
	want := map[string]bool{"list_dir": true, "read_file": true, "summarize_file": true}
	if len(got) != len(want) {
		t.Fatalf("after research narrowing want %d advertised tools, got %d (%v)", len(want), len(got), got)
	}
	for _, n := range got {
		if !want[n] {
			t.Fatalf("research advertised %q, which it does not list", n)
		}
	}
}

// The general profile must leave the advertised set untouched — this is what makes
// "a box that does not set agent_profile is byte-identical" true, and what lets the
// callers apply a profile unconditionally.
func TestAdvertisedToolsUnchangedByGeneral(t *testing.T) {
	names := []string{"list_dir", "read_file", "search_files"}
	tools := make([]Tool, 0, len(names))
	for _, n := range names {
		tools = append(tools, stubTool(n))
	}
	loop := NewLoop(nil, tools, 12).WithSystem("SYS")
	before := loop.AdvertisedTools()

	prof, err := LookupProfile("general")
	if err != nil {
		t.Fatal(err)
	}
	loop.WithProfile(prof)

	after := loop.AdvertisedTools()
	if len(before) != len(after) {
		t.Fatalf("general changed the advertised set: %v -> %v", before, after)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("general reordered/changed the advertised set: %v -> %v", before, after)
		}
	}
	if loop.system != "SYS" {
		t.Fatalf("general overwrote the system prompt: %q", loop.system)
	}
}
