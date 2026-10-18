package main

// The host-RAM docs and comments say what the rule does, and several sentences kept saying what an earlier version of it did:
// the commit condition off on a soft Linux reading, the warm-back held to the commit limit, "allocations fail past the commit
// limit" without the page file that makes it true, and every declaring lease waiting on NEAR. Comment- and doc-only text has no
// behaviour to fail, so these refuse the retired wordings by file, the way fence_cost_docs_test.go does; the behaviour each
// sentence describes is pinned beside it (named per test).
//
// A released CHANGELOG section is history and is not scanned.

import (
	"fmt"
	"strings"
	"testing"
)

// retiredHostRAMWordings is the check over the files under root, as a list, so a test can run it on a root of its own.
func retiredHostRAMWordings(root string, retired map[string][]string, instead string) []string {
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
				problems = append(problems, fmt.Sprintf("%s still says %q: %s", name, phrase, instead))
			}
		}
	}
	return problems
}

// A commit limit the OS does not enforce switches off the judgment of a commit a lease STATES. A commit nobody stated is held to
// physical RAM less the headroom on every reading (internal/gpucards TestTheStatusLineOnASoftReadingSaysWhichCommitLineStillApplies,
// internal/gpuprobe TestAHostThatReadsNearOnThePhysicalCommitLineStillAdmitsALeaseThatStatesItsCommit).
func TestNoSentenceSaysASoftCommitLimitSwitchesTheWholeCommitConditionOff(t *testing.T) {
	problems := retiredHostRAMWordings(".", map[string][]string{
		"internal/gpucards/hostview.go":   {"the commit condition is not applied", "the commit condition is applied to it"},
		"internal/gpuprobe/hostmemory.go": {"the commit condition is not applied"},
		"docs/systems/gpu-lease.md":       {"whether the commit condition is applied on this host"},
		// OVER is also the commit charge above physical RAM (the line a commit nobody stated is held to), and the grant judges a
		// commit against that line, not only against an enforced limit.
		"gpu_cmd.go": {
			"or the commit charge at an enforced commit limit",
			"(and its commit against the commit limit)",
			"the commit charge plus its commit and what the running leases have yet to be charged would pass the limit",
		},
		"internal/mcpserver/status_section.go": {"or the commit charge at an enforced commit limit"},
	}, "a commit limit the OS does not enforce stops judging a commit a lease states, and a commit nobody stated is still held to physical RAM less the headroom")
	for _, p := range problems {
		t.Error(p)
	}
}

// The warm-back declares the seat's footprint as one figure and states no commit, so it is held to physical RAM less the headroom
// (TestAWarmBackIsHeldToPhysicalRAMAndNotToTheCommitLimit).
func TestNoSentenceHoldsTheWarmBackToTheCommitLimit(t *testing.T) {
	problems := retiredHostRAMWordings(".", map[string][]string{
		"docs/systems/gpu-lease.md":     {"and stay under the commit limit less the headroom"},
		"internal/hostneed/hostneed.go": {"taken as the commit unless the caller states one, against the commit limit"},
	}, "a footprint declared as one figure states no commit, so it is held to physical RAM less the headroom, not to the commit limit")
	for _, p := range problems {
		t.Error(p)
	}
}

// Past the Windows commit limit other processes' allocations fail only once the page file has reached its maximum; before that
// Windows grows a system-managed file. gpu-lease.md "The commit limit moves" says so, and the flat sentences beside it must not.
func TestNoSentenceSaysAllocationsFailPastTheCommitLimitWithoutTheCondition(t *testing.T) {
	problems := retiredHostRAMWordings(".", map[string][]string{
		"docs/systems/gpu-lease.md": {"and then other processes' allocations fail", "allocations fail at once the page file"},
		"internal/gpuprobe/hostmemory.go": {
			"allocations fail while physical RAM may sit free",
			"(allocations by other processes can fail)",
			"what the OS refuses allocations at",
		},
		"internal/gpulease/hostram.go": {"what the OS refuses allocations at"},
	}, "other processes' allocations fail past the commit limit only once the page file has reached its maximum; before that a system-managed file grows")
	for _, p := range problems {
		t.Error(p)
	}
}

// A stated commit is judged where the OS enforces a commit limit: always on Windows, on Linux in strict mode, and on Linux when the
// mode cannot be read (assumed strict: a limit assumed away on an input nobody read is a brake switched off). Not "only with
// vm.overcommit_memory = 2", which leaves out the unreadable mode (internal/gpuprobe
// TestTheLinuxReaderMarksTheCommitLimitSoftOnlyInAKnownLaxMode).
func TestNoSentenceSaysAStatedCommitIsJudgedOnLinuxOnlyInStrictMode(t *testing.T) {
	problems := retiredHostRAMWordings(".", map[string][]string{
		"gpu_cmd.go":                {"on Linux only with vm.overcommit_memory=2"},
		"docs/OPERATOR-GUIDE.md":    {"on Linux only with `vm.overcommit_memory` = 2"},
		"docs/systems/gpu-lease.md": {"on Linux only in strict overcommit mode"},
	}, "a stated commit is judged on Linux in strict mode and when the mode cannot be read (assumed strict), not only with vm.overcommit_memory = 2")
	for _, p := range problems {
		t.Error(p)
	}
}

// A commit nobody stated is held to physical RAM less the headroom, or to an enforced commit limit less the headroom when that sits
// below physical RAM (strict-mode Linux with little swap): the nearer of the two (internal/gpuprobe
// TestACommitNobodyStatedIsHeldToAnEnforcedLimitThatSitsBelowPhysicalRAM).
func TestNoSentenceHoldsAnUnstatedCommitToPhysicalRAMAlone(t *testing.T) {
	problems := retiredHostRAMWordings(".", map[string][]string{
		"docs/systems/gpu-lease.md": {
			"held to physical RAM less the headroom on every host)",
			"A commit nobody stated is still held to physical RAM on every reading,",
		},
		"docs/OPERATOR-GUIDE.md": {"under physical RAM less the headroom, on every OS."},
	}, "a commit nobody stated is held to physical RAM less the headroom, or to an enforced commit limit less the headroom when that sits below physical RAM")
	for _, p := range problems {
		t.Error(p)
	}
}

// NEAR holds back the leases judged on the line that is inside the headroom, and not every lease that declares host RAM: one that
// states its commit is not judged on the physical-RAM line (internal/gpuprobe
// TestAHostThatReadsNearOnThePhysicalCommitLineStillAdmitsALeaseThatStatesItsCommit).
func TestNoSentenceSaysNearHoldsBackEveryDeclaringLease(t *testing.T) {
	problems := retiredHostRAMWordings(".", map[string][]string{
		"internal/gpuprobe/hostmemory.go": {"so a lease that declares any host RAM waits"},
		"internal/gpucards/hostview.go":   {"so a lease that declares any host RAM waits"},
		"docs/systems/gpu-lease.md":       {"the next declaring lease waits"},
	}, "a lease waits on NEAR only on a line that applies to it: one that states its commit is not held to the physical-RAM line")
	for _, p := range problems {
		t.Error(p)
	}
}
