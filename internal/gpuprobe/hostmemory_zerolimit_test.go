package gpuprobe

import (
	"strings"
	"testing"
)

// A reading from a platform whose commit limit is always enforced (Windows; Linux in strict mode) that carries no limit is a
// reading the rule cannot trust: it fails CLOSED. Before, the commit condition was skipped for it ("a zero limit is no commit
// accounting"), so a counter that came back zero turned the brake off without a word.

func TestAHardReadingWithoutACommitLimitIsUnreadable(t *testing.T) {
	mem := HostMemory{PhysicalGiB: 128, AvailableGiB: 100, CommitUsedGiB: 50, CommitLimitGiB: 0}
	for name, need := range map[string]HostLoad{
		"a stated commit":      {TouchGiB: 10, CommitGiB: 10, CommitStated: true},
		"a commit nobody said": {TouchGiB: 10, CommitGiB: 10},
		"a touched set alone":  {TouchGiB: 10},
	} {
		got := hostRAMAdmitsLoad(mem, true, true, need, HostLoad{}, 8)
		if got.OK || !got.Unreadable || got.Impossible {
			t.Errorf("%s: a hard reading with no commit limit must wait as unreadable, got ok=%v unreadable=%v impossible=%v (%s)", name, got.OK, got.Unreadable, got.Impossible, got.Why)
		}
		if !strings.Contains(got.Why, "cannot be read") {
			t.Errorf("%s: the refusal must say the memory could not be read: %q", name, got.Why)
		}
	}
	// A job that declares nothing is admitted without a reading, as ever.
	if got := hostRAMAdmitsLoad(mem, true, true, HostLoad{}, HostLoad{}, 8); !got.OK {
		t.Errorf("a job that declares nothing is admitted: %+v", got)
	}
	// A SOFT reading with no limit is a sandbox that hides the accounting: it keeps being judged by what it has.
	soft := mem
	soft.CommitLimitSoft = true
	if got := hostRAMAdmitsLoad(soft, true, true, HostLoad{TouchGiB: 10, CommitGiB: 10, CommitStated: true}, HostLoad{}, 8); !got.OK {
		t.Errorf("a soft reading without a limit is judged on the available memory: %+v (%s)", got, got.Why)
	}
}

func TestTheWindowsCountersWithoutATotalPageFileAreNoReading(t *testing.T) {
	const gib = 1 << 30
	if m, ok := hostMemoryFromStatus(128*gib, 100*gib, 0, 0); ok {
		t.Fatalf("a total page file of 0 is the commit limit missing, not a limit of 0: want no reading, got %+v", m)
	}
	if _, ok := hostMemoryFromStatus(128*gib, 100*gib, 192*gib, 140*gib); !ok {
		t.Fatal("a reading with a commit limit is a reading")
	}
}

func TestTheVerdictOfAHardReadingWithoutACommitLimitIsUnknown(t *testing.T) {
	mem := HostMemory{PhysicalGiB: 128, AvailableGiB: 100, CommitUsedGiB: 50, CommitLimitGiB: 0}
	if v := mem.Verdict(8); v != HostUnknown {
		t.Errorf("a hard reading with no commit limit is not OK, it is unknown: got %s", v)
	}
	mem.CommitLimitSoft = true
	if v := mem.Verdict(8); v != HostOK {
		t.Errorf("a soft reading is judged on what it has: got %s", v)
	}
}
