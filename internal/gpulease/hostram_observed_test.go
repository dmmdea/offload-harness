package gpulease

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// The reported pair of media lanes: lane B declared 36 GiB of commit and peaked at about 25 GiB, then fell back to 8 GiB; lane A
// (28 GiB touched, 36 GiB of commit) was told that 28 GiB were "still to load" for as long as B ran. The admission check is put
// to the host through its public functions with B's process tree read from a stand-in for the process table, a clock a
// test moves, and the host figures of the report.
func TestWhatALeaseHasPeakedAtShrinksWhatIsStillToLoadOnceItHasPlateaued(t *testing.T) {
	m := scopedRealClock(t)
	f := newFakeHostMem(gpuprobe.HostMemory{PhysicalGiB: 128, AvailableGiB: 100, CommitUsedGiB: 87.5, CommitLimitGiB: 146.7})
	f.arm(m)
	at := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	m.observedClock = func() time.Time { return at }

	b, err := m.TryAcquire(ClassMedia, Options{Reason: "lane b", TTL: time.Hour, Devices: []string{card1}, HostRAMGiB: 36, HostRAMCommitGiB: 36})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Release() }()
	laneA := gpuprobe.HostLoad{TouchGiB: 28, CommitGiB: 36, CommitStated: true}
	step := func(after time.Duration, heldGiB float64) (commitPending float64, why string) {
		at = at.Add(after)
		f.setHeld(os.Getpid(), heldGiB)
		chk := m.HostRAMCheckLoad(laneA)
		return chk.CommitPendingGiB, chk.Why
	}

	if got, _ := step(0, 0); got != 36 {
		t.Fatalf("lane b has loaded nothing yet: all 36 GiB are still to load, got %.1f", got)
	}
	step(15*time.Second, 12)
	for _, s := range []time.Duration{15, 15, 15, 15} { // 25 GiB held from 30 s: the run begins and has not spanned a minute
		step(s*time.Second, 25)
	}
	// It lets go of most of what it loaded. Before the plateau only what it holds now is credited.
	got, why := step(5*time.Second, 8)
	if got != 28 || !strings.Contains(why, "+28.0 GiB still to load by leases already running") {
		t.Fatalf("before the plateau the figure is the unshrunk 28, got %.1f: %s", got, why)
	}
	// The run of 25 GiB then spans more than a minute: the 25 GiB it peaked at are what it is credited with. 36 - 25 = 11.
	if got, _ = step(15*time.Second, 8); got != 11 {
		t.Fatalf("once the plateau spans a minute still to load must be 36 - 25 = 11, got %.1f", got)
	}
	if chk := m.HostRAMCheckLoad(laneA); !chk.OK {
		t.Fatalf("87.5 committed + 36 + 11 still to load is under 138.7: %s", chk.Why)
	}
	// On a host with less room the wait says the shrunk figure.
	f.setCommit(100)
	if chk := m.HostRAMCheckLoad(laneA); chk.OK || !strings.Contains(chk.Why, "needs 36.0 GiB of commit, committed 100.0 of 146.7 GiB commit limit (+11.0 GiB still to load by leases already running)") {
		t.Fatalf("the wait must show the shrunk figure: %+v", chk)
	}
}
