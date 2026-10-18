package main

import (
	"strings"
	"testing"
)

// A mode the reader cannot read is assumed strict (fail closed), so a stated commit IS judged there: the help must not say that
// Linux judges it "only with vm.overcommit_memory=2".
func TestRamCommitHelpSaysAnUnreadableOvercommitModeCountsAsStrict(t *testing.T) {
	for _, want := range []string{"cannot be read", "counts as strict"} {
		if !strings.Contains(ramCommitHelp, want) {
			t.Errorf("the --ram-commit help lacks %q:\n%s", want, ramCommitHelp)
		}
	}
	if strings.Contains(ramCommitHelp, "on Linux only with vm.overcommit_memory=2") {
		t.Errorf("the --ram-commit help says Linux judges a stated commit only in strict mode, but a mode that cannot be read counts as strict:\n%s", ramCommitHelp)
	}
}

// The --ram-commit help says that a job whose commit is smaller than its working set states it with this flag, not that it
// cannot say so: the code takes --ram-commit below --ram as stated.
func TestRamCommitHelpSaysASmallerCommitIsStatedWithTheFlag(t *testing.T) {
	for _, want := range []string{"cannot say so by leaving the flag out", "taken as stated"} {
		if !strings.Contains(ramCommitHelp, want) {
			t.Errorf("the --ram-commit help lacks %q:\n%s", want, ramCommitHelp)
		}
	}
}
