package main

// Register C-86a, cost of the load fence's pin lookups. A seat's pins now include the serving config,
// which costs a Stat of the file per lookup. The fence's own readers (ScopeToModel and what goes through
// it, SeatCards) look the pins up only under a held lease that names cards, so an idle box and a
// whole-node lease never touch the file; PinsFor and every other caller of Config.ModelPins ask on every
// call, an idle box included. The first wording of the docs and of the seatscope.go header said the
// first half of that as if it were all of it ("an idle box ... never touch the file"). The code was
// right and the prose was wider than the code; the behaviour is pinned in internal/modelaffinity
// (TestOnlyTheFencesOwnLookupsWaitForALeaseThatNamesCards), and this refuses the retired wording.
//
// A released CHANGELOG section is history and a retired phrase is refused anywhere in the file: the
// wording was corrected before any release shipped it.

import (
	"fmt"
	"strings"
	"testing"
)

func TestTheDocsDoNotClaimAnIdleBoxNeverTouchesTheServingConfig(t *testing.T) {
	for _, problem := range fenceDocProblems(".") {
		t.Error(problem)
	}
}

// fenceDocProblems is that check over the files under root, as a list, so a test can run it on a root of its own
// (TestTheFenceCostCheckSeesAChangelogFragment).
func fenceDocProblems(root string) []string {
	retired := map[string][]string{
		"CHANGELOG.md": {
			"The idle path pays nothing new.",
			"(which touches every seat whatever its pin) never touch the file",
		},
		"docs/systems/gpu-lease.md": {
			"so an idle box and a whole-node lease do not touch the file",
		},
		"internal/modelaffinity/seatscope.go": {
			"A seat's pins are looked up on the same condition, and no sooner",
		},
	}
	var problems []string
	for name, phrases := range retired {
		raw, err := docText(root, name)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		// A quote wraps across lines, and a Go comment carries its // markers across the wrap.
		text := strings.Join(strings.Fields(strings.ReplaceAll(raw, "//", " ")), " ")
		for _, phrase := range phrases {
			if strings.Contains(text, phrase) {
				problems = append(problems, fmt.Sprintf("%s still says %q: only the fence's own lookups wait for a lease that names cards; PinsFor and the other callers of Config.ModelPins Stat the serving config on every call", name, phrase))
			}
		}
	}
	return problems
}
