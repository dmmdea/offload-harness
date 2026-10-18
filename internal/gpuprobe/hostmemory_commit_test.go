package gpuprobe

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// The admission rule keeps two quantities apart (hostmemory.go): the working set a job TOUCHES must fit the physical RAM
// that is available, and the commit it is CHARGED must fit the OS commit limit. The readings below are the ones the change
// was made for: a Windows box with 127.7 GiB of RAM and a 171.7 GiB commit limit that read 70.5 GiB committed while 75.6 GiB
// were physically free, and a coder-model arm whose process tree touched at most 27.8 GiB but was charged 58.1 GiB.
var referenceWindowsHost = HostMemory{PhysicalGiB: 127.7, AvailableGiB: 75.6, CommitUsedGiB: 70.5, CommitLimitGiB: 171.7}

// statedAdmits is the single-figure form with the commit STATED: the figure goes to both quantities, and the commit is judged
// against the commit limit (a caller that measured its commit). hostRAMAdmits is the same figure with no commit stated, which is
// judged against physical RAM (hostmemory_defaulted_test.go).
func statedAdmits(mem HostMemory, readable, supported bool, needGiB, pendingGiB, headroomGiB float64) HostRAMCheck {
	return hostRAMAdmitsLoad(mem, readable, supported, HostLoad{TouchGiB: needGiB, CommitGiB: needGiB, CommitStated: true}, HostLoad{TouchGiB: pendingGiB, CommitGiB: pendingGiB}, headroomGiB)
}

func TestMeasuredCoderArmIsAdmittedAgainstTheCommitLimit(t *testing.T) {
	need := HostLoad{TouchGiB: 27.8, CommitGiB: 58.1, CommitStated: true} // the measured charge, stated
	// The comparison this replaces for a job that states its commit: commit now + the declared need against PHYSICAL RAM less the headroom. It refused the arm.
	if old := referenceWindowsHost.CommitUsedGiB + need.CommitGiB; old <= referenceWindowsHost.PhysicalGiB-8 {
		t.Fatalf("the fixture is wrong: %.1f GiB projected fits %.1f GiB of physical RAM less the headroom, so the old comparison admitted it too", old, referenceWindowsHost.PhysicalGiB)
	}
	got := hostRAMAdmitsLoad(referenceWindowsHost, true, true, need, HostLoad{}, 8)
	if !got.OK || got.Impossible || got.TouchShort || got.CommitShort {
		t.Fatalf("the coder arm touches 27.8 GiB of the 75.6 free and commits 58.1 of the 101.2 the commit limit leaves: want admitted, got %+v (%s)", got, got.Why)
	}
	if !near(got.CommitProjectedGiB, 70.5+58.1) || !near(got.CommitBoundGiB, 171.7-8) {
		t.Errorf("the check must carry the commit arithmetic it decided on: projected %.1f bound %.1f", got.CommitProjectedGiB, got.CommitBoundGiB)
	}
}

// Each condition refuses on its own, and the refusal says which one and with whose numbers.
func TestEachConditionRefusesAlone(t *testing.T) {
	cases := []struct {
		name                       string
		mem                        HostMemory
		need, pending              HostLoad
		wantOK                     bool
		wantTouchShort, wantCommit bool
		wantWhy                    []string
	}{
		{"both pass",
			HostMemory{PhysicalGiB: 127.7, AvailableGiB: 90, CommitUsedGiB: 40, CommitLimitGiB: 171.7},
			HostLoad{TouchGiB: 20, CommitGiB: 40}, HostLoad{}, true, false, false, nil},
		{"the touched set does not fit what is available although the commit limit has room",
			HostMemory{PhysicalGiB: 127.7, AvailableGiB: 30, CommitUsedGiB: 40, CommitLimitGiB: 171.7},
			HostLoad{TouchGiB: 28, CommitGiB: 28}, HostLoad{}, false, true, false,
			[]string{"waiting for host RAM", "needs 28.0 GiB", "only 30.0 GiB of 127.7 GiB physical is available", "8.0 GiB headroom"}},
		{"the commit does not fit the commit limit although plenty of physical RAM is available",
			HostMemory{PhysicalGiB: 127.7, AvailableGiB: 90, CommitUsedGiB: 150, CommitLimitGiB: 171.7},
			HostLoad{TouchGiB: 10, CommitGiB: 20}, HostLoad{}, false, false, true,
			[]string{"waiting for host RAM", "needs 20.0 GiB of commit", "committed 150.0 of 171.7 GiB commit limit", "8.0 GiB headroom"}},
		{"both fail and the refusal names both",
			HostMemory{PhysicalGiB: 127.7, AvailableGiB: 20, CommitUsedGiB: 150, CommitLimitGiB: 171.7},
			HostLoad{TouchGiB: 15, CommitGiB: 20}, HostLoad{}, false, true, true,
			[]string{"only 20.0 GiB of 127.7 GiB physical is available", "committed 150.0 of 171.7 GiB commit limit"}},
		{"what the running leases have yet to touch counts against the available memory only",
			HostMemory{PhysicalGiB: 127.7, AvailableGiB: 40, CommitUsedGiB: 40, CommitLimitGiB: 171.7},
			HostLoad{TouchGiB: 10, CommitGiB: 10}, HostLoad{TouchGiB: 25}, false, true, false,
			[]string{"+25.0 GiB still to load by leases already running", "only 40.0 GiB of 127.7 GiB physical is available"}},
		{"what the running leases have yet to commit counts against the commit limit only",
			HostMemory{PhysicalGiB: 127.7, AvailableGiB: 90, CommitUsedGiB: 40, CommitLimitGiB: 171.7},
			HostLoad{TouchGiB: 10, CommitGiB: 10}, HostLoad{CommitGiB: 130}, false, false, true,
			[]string{"+130.0 GiB still to load by leases already running", "committed 40.0 of 171.7 GiB commit limit"}},
		{"exactly at the available floor is admitted",
			HostMemory{PhysicalGiB: 128, AvailableGiB: 23, CommitUsedGiB: 50, CommitLimitGiB: 192},
			HostLoad{TouchGiB: 15, CommitGiB: 15}, HostLoad{}, true, false, false, nil},
		{"exactly at the commit bound is admitted",
			HostMemory{PhysicalGiB: 128, AvailableGiB: 100, CommitUsedGiB: 150, CommitLimitGiB: 192},
			HostLoad{TouchGiB: 10, CommitGiB: 34}, HostLoad{}, true, false, false, nil},
		{"a quarter GiB past the commit bound is refused",
			HostMemory{PhysicalGiB: 128, AvailableGiB: 100, CommitUsedGiB: 150.25, CommitLimitGiB: 192},
			HostLoad{TouchGiB: 10, CommitGiB: 34}, HostLoad{}, false, false, true, []string{"commit limit"}},
	}
	for _, c := range cases {
		c.need.CommitStated = true // these cases put a STATED commit to the commit limit
		got := hostRAMAdmitsLoad(c.mem, true, true, c.need, c.pending, 8)
		if got.OK != c.wantOK || got.Impossible || got.TouchShort != c.wantTouchShort || got.CommitShort != c.wantCommit {
			t.Errorf("%s: ok=%v impossible=%v touchShort=%v commitShort=%v, want ok=%v touchShort=%v commitShort=%v (%s)",
				c.name, got.OK, got.Impossible, got.TouchShort, got.CommitShort, c.wantOK, c.wantTouchShort, c.wantCommit, got.Why)
		}
		if !c.wantOK && got.Why == "" {
			t.Errorf("%s: a refusal must say why", c.name)
		}
		for _, w := range c.wantWhy {
			if !strings.Contains(got.Why, w) {
				t.Errorf("%s: why = %q, want it to contain %q", c.name, got.Why, w)
			}
		}
		if c.wantOK && got.Why != "" {
			t.Errorf("%s: an admission carries no refusal text, got %q", c.name, got.Why)
		}
	}
}

// The same numbers, a hard commit limit and a soft one: the commit condition is applied only where the OS enforces it.
func TestTheCommitConditionIsAppliedOnlyWhereTheLimitIsHard(t *testing.T) {
	mem := HostMemory{PhysicalGiB: 128, AvailableGiB: 100, CommitUsedGiB: 150, CommitLimitGiB: 192}
	need := HostLoad{TouchGiB: 10, CommitGiB: 40, CommitStated: true} // 150 + 40 = 190 > 192 - 8
	strict := hostRAMAdmitsLoad(mem, true, true, need, HostLoad{}, 8)
	if strict.OK || !strict.CommitShort || !strict.CommitApplies {
		t.Fatalf("a hard commit limit refuses 190 against a bound of 184: %+v (%s)", strict, strict.Why)
	}
	mem.CommitLimitSoft = true
	soft := hostRAMAdmitsLoad(mem, true, true, need, HostLoad{}, 8)
	if !soft.OK || soft.CommitShort || soft.CommitApplies {
		t.Fatalf("a soft commit limit is not applied: want admitted by the available condition alone, got %+v (%s)", soft, soft.Why)
	}
	// The available condition is never relaxed.
	mem.AvailableGiB = 12
	if low := hostRAMAdmitsLoad(mem, true, true, need, HostLoad{}, 8); low.OK || !low.TouchShort {
		t.Fatalf("a soft commit limit leaves the available condition in force: %+v (%s)", low, low.Why)
	}
}

// A job that declares nothing of one quantity is not judged on it.
func TestAQuantityDeclaredAsZeroIsNotJudged(t *testing.T) {
	nearLimit := HostMemory{PhysicalGiB: 128, AvailableGiB: 100, CommitUsedGiB: 190, CommitLimitGiB: 192}
	if got := hostRAMAdmitsLoad(nearLimit, true, true, HostLoad{TouchGiB: 10}, HostLoad{}, 8); !got.OK {
		t.Fatalf("a job that declares no commit is not held by a commit charge it adds nothing to: %+v (%s)", got, got.Why)
	}
	nothingFree := HostMemory{PhysicalGiB: 128, AvailableGiB: 1, CommitUsedGiB: 40, CommitLimitGiB: 192}
	if got := hostRAMAdmitsLoad(nothingFree, true, true, HostLoad{CommitGiB: 10}, HostLoad{}, 8); !got.OK {
		t.Fatalf("a job that declares no touched set is not held by the available memory: %+v (%s)", got, got.Why)
	}
	if got := hostRAMAdmitsLoad(HostMemory{}, false, true, HostLoad{}, HostLoad{TouchGiB: 50, CommitGiB: 50}, 8); !got.OK {
		t.Fatalf("a job that declares nothing is admitted without a reading: %+v", got)
	}
}

// A commit no state of the box can ever admit ends the request at once, as a touched set above physical RAM always did.
func TestACommitAboveTheLimitLessHeadroomIsImpossible(t *testing.T) {
	mem := HostMemory{PhysicalGiB: 128, AvailableGiB: 100, CommitUsedGiB: 30, CommitLimitGiB: 192}
	got := hostRAMAdmitsLoad(mem, true, true, HostLoad{TouchGiB: 10, CommitGiB: 190, CommitStated: true}, HostLoad{}, 8)
	if got.OK || !got.Impossible || !got.CommitShort {
		t.Fatalf("190 GiB of commit can never fit 192 less 8: want impossible on commit, got %+v (%s)", got, got.Why)
	}
	if !strings.Contains(got.Why, "can never admit") || !strings.Contains(got.Why, "--ram-commit") {
		t.Errorf("an impossible commit says what to change: %q", got.Why)
	}
	mem.CommitLimitSoft = true
	if soft := hostRAMAdmitsLoad(mem, true, true, HostLoad{TouchGiB: 10, CommitGiB: 190, CommitStated: true}, HostLoad{}, 8); !soft.OK {
		t.Fatalf("a soft limit makes no stated commit impossible: %+v (%s)", soft, soft.Why)
	}
	// A commit nobody stated is held to physical RAM on every reading, so 190 against 128 less 8 is impossible on a soft one too.
	if unstated := hostRAMAdmitsLoad(mem, true, true, HostLoad{TouchGiB: 10, CommitGiB: 190}, HostLoad{}, 8); !unstated.Impossible || !unstated.CommitShort || !strings.Contains(unstated.Why, "physical RAM") {
		t.Fatalf("an unstated commit above physical RAM less the headroom is impossible, naming the line: %+v (%s)", unstated, unstated.Why)
	}
	// A touched set above physical RAM less the headroom stays impossible, whatever the commit says.
	if got := hostRAMAdmitsLoad(mem, true, true, HostLoad{TouchGiB: 125, CommitGiB: 1}, HostLoad{}, 8); got.OK || !got.Impossible || !got.TouchShort {
		t.Fatalf("a touched set of 125 GiB can never fit 128 less 8: %+v (%s)", got, got.Why)
	}
}

// HostRAMAdmits, the single-figure form every existing caller uses, declares the same figure for both quantities and states no
// commit: the commit is judged against physical RAM less the headroom, as it always was. On the reference host a 72 GiB declaration
// is refused on both conditions, and a 60 GiB one on the commit charge alone (70.5 + 60 > 119.7) although the available memory has
// room (75.6 - 60 >= 8); the same 60 GiB STATED as a commit fits the commit limit (70.5 + 60 <= 163.7).
func TestTheSingleFigureFormDeclaresTheSameFigureForBoth(t *testing.T) {
	// Not the coder arm's figures: one number, the old caller's.
	got := hostRAMAdmits(referenceWindowsHost, true, true, 72, 0, 8)
	if got.OK || !got.TouchShort || !got.CommitShort {
		t.Fatalf("72 GiB declared once: refused on the available memory and on the commit charge, got %+v (%s)", got, got.Why)
	}
	if got.CommitNeedGiB != 72 || got.CommitStated {
		t.Errorf("the commit declaration defaults to the touched figure and is not stated, got %.1f stated=%v", got.CommitNeedGiB, got.CommitStated)
	}
	if refused := hostRAMAdmits(referenceWindowsHost, true, true, 60, 0, 8); refused.OK || !refused.CommitShort || refused.TouchShort {
		t.Fatalf("60 GiB declared once: refused on the commit charge against physical RAM less the headroom (70.5 + 60 > 119.7): %+v (%s)", refused, refused.Why)
	}
	if fits := statedAdmits(referenceWindowsHost, true, true, 60, 0, 8); !fits.OK || !fits.CommitStated {
		t.Fatalf("60 GiB stated as the commit fits both conditions on the reference host (75.6 - 60 >= 8; 70.5 + 60 <= 163.7): %+v (%s)", fits, fits.Why)
	}
}

// The incident of 2026-10-09, with the numbers it had: a 56 GiB desktop baseline on a 128 GiB box, so 72 GiB available, and
// two bf16 lanes of about 33 GiB each. The first lane fits; the second is refused while the first has not loaded, on the
// available memory.
func TestTheTwoLaneIncidentIsStillRefused(t *testing.T) {
	mem := HostMemory{PhysicalGiB: 128, AvailableGiB: 72, CommitUsedGiB: 56, CommitLimitGiB: 188}
	lane := HostLoad{TouchGiB: 32.75, CommitGiB: 32.75}
	if first := hostRAMAdmitsLoad(mem, true, true, lane, HostLoad{}, 8); !first.OK {
		t.Fatalf("the first lane fits the idle box: %+v (%s)", first, first.Why)
	}
	second := hostRAMAdmitsLoad(mem, true, true, lane, lane, 8)
	if second.OK || !second.TouchShort {
		t.Fatalf("the second lane is refused while the first is still loading: %+v (%s)", second, second.Why)
	}
}

func TestParseOvercommit(t *testing.T) {
	cases := []struct {
		in   string
		mode int
		ok   bool
	}{{"0\n", 0, true}, {"1\n", 1, true}, {"2\n", 2, true}, {" 2 ", 2, true}, {"", 0, false}, {"banana\n", 0, false}, {"3\n", 0, false}, {"-1\n", 0, false}}
	for _, c := range cases {
		mode, ok := parseOvercommit(c.in)
		if mode != c.mode || ok != c.ok {
			t.Errorf("parseOvercommit(%q) = %d, %v; want %d, %v", c.in, mode, ok, c.mode, c.ok)
		}
	}
}

// The Linux reader through its seam: the commit limit is hard only in strict mode (vm.overcommit_memory 2). In the
// heuristic and always modes the kernel grants allocations past CommitLimit, so the number is no limit; a mode that cannot be
// read, or a meminfo that hides the accounting, is not trusted as one either.
func TestTheLinuxReaderMarksTheCommitLimitHardOnlyInStrictMode(t *testing.T) {
	reader := func(meminfo string, overcommit string, overcommitErr error) func(string) ([]byte, error) {
		return func(path string) ([]byte, error) {
			switch path {
			case "/proc/meminfo":
				return []byte(meminfo), nil
			case "/proc/sys/vm/overcommit_memory":
				if overcommitErr != nil {
					return nil, overcommitErr
				}
				return []byte(overcommit), nil
			}
			return nil, errors.New("unexpected path " + path)
		}
	}
	cases := []struct {
		name     string
		meminfo  string
		mode     string
		modeErr  error
		wantSoft bool
	}{
		{"strict", sampleMeminfo, "2\n", nil, false},
		{"heuristic", sampleMeminfo, "0\n", nil, true},
		{"always", sampleMeminfo, "1\n", nil, true},
		{"mode unreadable", sampleMeminfo, "", errors.New("permission denied"), true},
		{"mode garbled", sampleMeminfo, "x\n", nil, true},
		{"strict but the accounting is hidden", strings.ReplaceAll(sampleMeminfo, "CommitLimit:", "Other:"), "2\n", nil, true},
	}
	for _, c := range cases {
		got, ok := readHostMemoryLinux(reader(c.meminfo, c.mode, c.modeErr))
		if !ok {
			t.Errorf("%s: no reading", c.name)
			continue
		}
		if got.CommitLimitSoft != c.wantSoft {
			t.Errorf("%s: CommitLimitSoft = %v, want %v", c.name, got.CommitLimitSoft, c.wantSoft)
		}
		if got.PhysicalGiB != 128 || got.AvailableGiB != 32 {
			t.Errorf("%s: the meminfo figures are carried over, got %+v", c.name, got)
		}
	}
	if _, ok := readHostMemoryLinux(func(string) ([]byte, error) { return nil, errors.New("no /proc") }); ok {
		t.Error("a meminfo that cannot be read is no reading")
	}
}

// The Windows reader's arithmetic through its seam: ullTotalPageFile is the commit limit and ullAvailPageFile what is left of
// it, so the charge is their difference, and the limit is always a hard one.
func TestTheWindowsCountersBecomeAReadingWithAHardLimit(t *testing.T) {
	const gib = 1 << 30
	got, ok := hostMemoryFromStatus(128*gib, 72*gib, 172*gib, 100*gib)
	if !ok {
		t.Fatal("complete counters must give a reading")
	}
	want := HostMemory{PhysicalGiB: 128, AvailableGiB: 72, CommitUsedGiB: 72, CommitLimitGiB: 172}
	if got != want {
		t.Fatalf("reading %+v, want %+v", got, want)
	}
	if got.CommitLimitSoft {
		t.Error("the Windows commit limit is always enforced")
	}
	if over, ok := hostMemoryFromStatus(128*gib, 72*gib, 172*gib, 200*gib); !ok || over.CommitUsedGiB != 0 {
		t.Errorf("an available page file above the total reads as nothing committed, got %+v ok=%v", over, ok)
	}
	if _, ok := hostMemoryFromStatus(0, 0, 0, 0); ok {
		t.Error("no physical memory is no reading")
	}
}

// A job that declares only one quantity still declares.
func TestHostLoadDeclares(t *testing.T) {
	cases := []struct {
		l    HostLoad
		want bool
	}{{HostLoad{}, false}, {HostLoad{TouchGiB: 1}, true}, {HostLoad{CommitGiB: 1}, true}, {HostLoad{TouchGiB: 1, CommitGiB: 2}, true}, {HostLoad{TouchGiB: -1, CommitGiB: -1}, false}}
	for _, c := range cases {
		if got := c.l.Declares(); got != c.want {
			t.Errorf("%+v.Declares() = %v, want %v", c.l, got, c.want)
		}
	}
}

// The reading docs/systems/gpu-lease.md quotes under "Known limits" (the reference box between jobs, 2026-10-10 about 04:12 UTC;
// session readings that are not recorded in this repository): 81.6 GiB committed, 83 GiB available, a 187.7 GiB commit limit. A
// 32.8 GiB declaration is admitted by both conditions. This pins the arithmetic the docs claim, so they cannot drift from the rule.
func TestTheQuotedCounterexampleIsAdmittedOnBothConditions(t *testing.T) {
	mem := HostMemory{PhysicalGiB: 127.7, AvailableGiB: 83, CommitUsedGiB: 81.6, CommitLimitGiB: 187.7}
	got := hostRAMAdmits(mem, true, true, 32.8, 0, 8)
	if !got.OK || got.Impossible {
		t.Fatalf("the quoted reading admits a 32.8 GiB declaration, got ok=%v impossible=%v (%s)", got.OK, got.Impossible, got.Why)
	}
	if math.Abs(got.CommitProjectedGiB-114.4) > 1e-6 || math.Abs(got.CommitBoundGiB-119.7) > 1e-6 {
		t.Errorf("declared once, the commit is held to physical RAM: projected commit = %v against a bound of %v, want 114.4 against 119.7", got.CommitProjectedGiB, got.CommitBoundGiB)
	}
	stated := statedAdmits(mem, true, true, 32.8, 0, 8)
	if !stated.OK || math.Abs(stated.CommitProjectedGiB-114.4) > 1e-6 || math.Abs(stated.CommitBoundGiB-179.7) > 1e-6 {
		t.Errorf("stated, it is held to the commit limit: ok=%v projected %v bound %v, want 114.4 against 179.7 (%s)", stated.OK, stated.CommitProjectedGiB, stated.CommitBoundGiB, stated.Why)
	}
	if math.Abs(got.AvailableAfterGiB-50.2) > 1e-6 || got.AvailableAfterGiB < 8 {
		t.Errorf("available after = %v, want 50.2, above the 8 GiB floor", got.AvailableAfterGiB)
	}
}

// The later reading the docs quote (06:32 UTC the same day): one lane holding 57.7 GiB private, 129.3 GiB committed and 49.1
// GiB available. It is under the commit limit by 58 GiB, so the rule's own lines are not reached, and a declaration is refused on
// the available memory once it would leave less than the headroom of it: 41.2 GiB (49.1 - 41.2 = 7.9) is refused, 41.0 is not.
func TestTheDocumentedReadingsAgreeWithTheRule(t *testing.T) {
	mem := HostMemory{PhysicalGiB: 127.7, AvailableGiB: 49.1, CommitUsedGiB: 129.3, CommitLimitGiB: 187.7}
	if v := mem.Verdict(8); v != HostOK {
		t.Errorf("129.3 GiB committed with 49.1 available under a 187.7 limit is OK, not %s", v)
	}
	over := statedAdmits(mem, true, true, 41.2, 0, 8)
	if over.OK || !over.TouchShort || over.CommitShort {
		t.Errorf("41.2 GiB stated is refused on the available memory alone: %+v (%s)", over, over.Why)
	}
	if fits := statedAdmits(mem, true, true, 41.0, 0, 8); !fits.OK {
		t.Errorf("41.0 GiB stated leaves 8.1 GiB of the available memory and 170.3 of 179.7 commit: %+v (%s)", fits, fits.Why)
	}
	// Declared once it is also over the commit charge's old line, which this reading is already past (129.3 > 119.7).
	if once := hostRAMAdmits(mem, true, true, 41.0, 0, 8); once.OK || !once.CommitShort {
		t.Errorf("41.0 GiB declared once is refused on the commit charge: %+v (%s)", once, once.Why)
	}
}
