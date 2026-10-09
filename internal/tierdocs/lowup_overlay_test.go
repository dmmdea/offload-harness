package tierdocs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func shippedProfiles(t *testing.T) map[string]Profile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	var d doc
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	return d.Profiles
}

// TestLowUpOverlayHoldsNoMediaKeys: the tier pages render the low-and-up overlay's NON-media rows
// only (the Media section's prose is written around the mid/high overlay). A media key added to
// config_seed_ram_low_up would therefore ship in the seed and be invisible in the docs, so the
// Media section must learn the overlay before one is allowed in.
func TestLowUpOverlayHoldsNoMediaKeys(t *testing.T) {
	for id, p := range shippedProfiles(t) {
		for k := range p.ConfigSeedLowUp {
			if mediaSeedKey(k) {
				t.Errorf("tier %s: config_seed_ram_low_up carries the media key %q, which the tier page cannot show yet", id, k)
			}
		}
	}
}

// TestTheTierPageMarksTheLowUpRowAsRAMGatedOnLowMidHigh: ampere-6's agent_model row is the
// low-and-up overlay's value (the spill seat), and its page must say it applies on low/mid/high only
// rather than on mid/high only, which is what a box reading the table would otherwise conclude.
func TestTheTierPageMarksTheLowUpRowAsRAMGatedOnLowMidHigh(t *testing.T) {
	p := shippedProfiles(t)["ampere-6"]
	if len(p.ConfigSeedLowUp) == 0 {
		t.Fatal("ampere-6 declares no config_seed_ram_low_up: the spill seat's binding is gone")
	}
	page := renderTier("ampere-6", p, nil)
	// The unconditional row stays beside the gated one: it is what a `min` box keeps.
	if !strings.Contains(page, "| `agent_model` | `qwen3.5-4b-agent` |") {
		t.Errorf("the page lost the unconditional agent_model row (the 4B a min-RAM box keeps):\n%s", page)
	}
	var row string
	for _, ln := range strings.Split(page, "\n") {
		if strings.HasPrefix(ln, "| `agent_model`") {
			row = ln
		}
	}
	if row == "" {
		t.Fatalf("the page has no agent_model row:\n%s", page)
	}
	if !strings.Contains(row, "RAM-gated: applied on `low`/`mid`/`high` only") || !strings.Contains(row, "qwen3.6-35b-a3b-agent") {
		t.Errorf("the agent_model row must name the spill seat and its low/mid/high gate, got %q", row)
	}
	if strings.Contains(row, "`mid`/`high` only") && !strings.Contains(row, "`low`/`mid`/`high`") {
		t.Errorf("the row claims mid/high only: %q", row)
	}
	// every other row keeps its meaning: sort keeps the test order-independent.
	keys := make([]string, 0, len(p.ConfigSeedLowUp))
	for k := range p.ConfigSeedLowUp {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) != 1 || keys[0] != "agent_model" {
		t.Errorf("ampere-6's low-and-up overlay should bind exactly agent_model, got %v", keys)
	}
}
