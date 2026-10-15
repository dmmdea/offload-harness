package compeval

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/eval"
)

// The adapter writes the measurement gate's outcome words as its own strings instead of
// importing the gate's enum, so a class the gate adds, drops or reorders would leave the
// adapter's results unreadable by the gate with every adapter test still green. This pins
// CBOutcomes, and the literal the run tests check results against, to eval.AllOutcomes.
func TestContextBenchOutcomesAreTheGatesTaxonomy(t *testing.T) {
	want := eval.AllOutcomes()
	if len(CBOutcomes) != len(want) {
		t.Fatalf("CBOutcomes has %d words and the gate's taxonomy %d classes: %v vs %v", len(CBOutcomes), len(want), CBOutcomes, want)
	}
	for i, w := range want {
		if CBOutcomes[i] != string(w) {
			t.Errorf("CBOutcomes[%d] = %q, the gate's class %d is %q", i, CBOutcomes[i], i, w)
		}
	}
	if got := strings.Join(CBOutcomes, " "); got != cbSevenWords {
		t.Errorf("CBOutcomes reads %q, the run tests check results against %q", got, cbSevenWords)
	}
}
