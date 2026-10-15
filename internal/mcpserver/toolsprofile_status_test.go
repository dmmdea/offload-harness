package mcpserver

import (
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// statusGuidance are the clauses of offload_status's description that tell a caller how to read the live roster. They were fused into
// the one 2,000-character "Returns {...}" sentence, which compact drops whole as a result shape, so a caller on the compact list was
// told to read the roster and not to trust it over a written figure, nor that a held card is queued behind and never refused.
var statusGuidance = []string{
	"Trust it over any rules file or written figure.",
	"idle_agent_nodes > 0 means paid-for capacity is sitting unused.",
	`Read verdict before concluding anything from "held"; a held card is queued behind with queue_with, never refused.`,
}

// TestStatusGuidanceIsItsOwnSentencesAndSurvivesCompact: in the full description each guidance clause is a sentence of its own (the
// splitter does not cut inside the open brace of the result shape), and compact keeps each of them. Nothing but punctuation moved
// to make them so.
func TestStatusGuidanceIsItsOwnSentencesAndSurvivesCompact(t *testing.T) {
	var full, compact *mcp.Tool
	for _, tool := range listTools(t, fullSurfaceCfg("full")) {
		if tool.Name == "offload_status" {
			full = tool
		}
	}
	for _, tool := range listTools(t, fullSurfaceCfg("compact")) {
		if tool.Name == "offload_status" {
			compact = tool
		}
	}
	if full == nil || compact == nil {
		t.Fatal("offload_status is not advertised")
	}
	fullSents, compactSents := splitSentences(full.Description), splitSentences(compact.Description)
	for _, want := range statusGuidance {
		if !slices.Contains(fullSents, want) {
			t.Errorf("the full description has no sentence of its own for %q (it is fused into the result shape)", want)
		}
		if !slices.Contains(compactSents, want) {
			t.Errorf("the compact description does not keep %q", want)
		}
	}
	// No other sentence of the description holds a guidance clause: it is not fused into the result shape again.
	for _, s := range fullSents {
		for _, want := range statusGuidance {
			if s != want && strings.Contains(s, want[:len(want)-1]) {
				t.Errorf("a guidance clause is fused into a longer sentence again: %.120q", s)
			}
		}
	}
}
