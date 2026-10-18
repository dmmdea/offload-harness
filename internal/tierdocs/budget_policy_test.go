package tierdocs

import (
	"strings"
	"testing"
)

// A tier that keeps the loop's default completion budget on purpose (register D-24, decision
// D8: `agent_budget_policy: "loop-default-chosen"`) says so on its page, and says the figure is
// CHOSEN and not measured. A page that showed no budget at all would let a reader take the
// absence of a number for "unlimited" or for "measured elsewhere".
func TestAMarkedTierPageSaysItsBudgetIsChosenNotMeasured(t *testing.T) {
	profiles := shippedProfiles(t)
	marked := 0
	for id, p := range profiles {
		if p.AgentBudgetPolicy == "" {
			continue
		}
		marked++
		page := renderTier(id, p, nil)
		for _, want := range []string{"Completion budget", "loop default", "1,024", "chosen", "not measured", "agent_budget_policy"} {
			if !strings.Contains(page, want) {
				t.Errorf("tier %s carries agent_budget_policy %q but its page does not say %q:\n%s", id, p.AgentBudgetPolicy, want, page)
			}
		}
	}
	if marked == 0 {
		t.Fatal("no shipped tier carries agent_budget_policy: this test matched nothing")
	}
}

// A tier without the marker is byte-for-byte what it was: the note is for the marked tiers only.
func TestAnUnmarkedTierPageHasNoBudgetNote(t *testing.T) {
	for id, p := range shippedProfiles(t) {
		if p.AgentBudgetPolicy != "" {
			continue
		}
		if page := renderTier(id, p, nil); strings.Contains(page, "Completion budget") {
			t.Errorf("tier %s has no agent_budget_policy but its page carries a completion-budget note", id)
		}
	}
}
