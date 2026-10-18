package main

import (
	"strings"
	"testing"
)

// The --ram-commit help says that a job whose commit is smaller than its working set states it with this flag, not that it
// cannot say so: the code takes --ram-commit below --ram as stated.
func TestRamCommitHelpSaysASmallerCommitIsStatedWithTheFlag(t *testing.T) {
	for _, want := range []string{"cannot say so by leaving the flag out", "taken as stated"} {
		if !strings.Contains(ramCommitHelp, want) {
			t.Errorf("the --ram-commit help lacks %q:\n%s", want, ramCommitHelp)
		}
	}
}
