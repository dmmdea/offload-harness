package gpucards

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// The room a lease that states no commit has is the room under the line it is held to, and on a reading whose enforced commit
// limit sits below physical RAM (Linux in strict mode with little swap) that line is the limit, not the physical RAM that lies
// further out (gpuprobe.HostRAMAdmitsLoad). 128 GiB of RAM, a 64 GiB enforced limit, 20 committed, headroom 8: 64 - 20 - 8 = 36.
func TestTheUnstatedCommitRoomIsUnderTheEnforcedLimitWhenThatSitsBelowPhysicalRAM(t *testing.T) {
	mem := gpuprobe.HostMemory{PhysicalGiB: 128, AvailableGiB: 100, CommitUsedGiB: 20, CommitLimitGiB: 64}
	got := NewHostView(mem, true, 8, gpuprobe.HostLoad{}, gpuprobe.HostLoad{}).Map()
	if got["unstated_commit_room_gib"] != 36.0 {
		t.Errorf("unstated_commit_room_gib = %v, want 36 (the 64 GiB limit less 20 committed and the 8 headroom, not 128 - 20 - 8 = 100)", got["unstated_commit_room_gib"])
	}
	// The grant agrees with the key: the figure it names is the most a one-figure lease is admitted at.
	if chk := gpuprobe.HostRAMAdmits(mem, true, 36, 0, 8); !chk.OK {
		t.Errorf("a one-figure lease of the room the key names is admitted: %s", chk.Why)
	}
	if chk := gpuprobe.HostRAMAdmits(mem, true, 36.5, 0, 8); chk.OK {
		t.Errorf("a one-figure lease past the room the key names is refused")
	}
	// A limit that is not enforced below physical RAM is no line.
	mem.CommitLimitSoft = true
	if soft := NewHostView(mem, true, 8, gpuprobe.HostLoad{}, gpuprobe.HostLoad{}).Map(); soft["unstated_commit_room_gib"] != 100.0 {
		t.Errorf("a soft reading keeps the physical line: unstated_commit_room_gib = %v, want 100", soft["unstated_commit_room_gib"])
	}
}

// OVER on the commit charge above physical RAM is the line a lease that states NO commit is held to. A lease that states its
// commit is judged on the commit limit (or, where the OS enforces none, not at all), and the rule admits it, so the loud lead says
// so beside the verdict: the note under it always did.
func TestTheOverLeadSaysWhatALeaseThatStatesItsCommitIsJudgedOn(t *testing.T) {
	enforced := gpuprobe.HostMemory{PhysicalGiB: 127.7, AvailableGiB: 75.6, CommitUsedGiB: 128.6, CommitLimitGiB: 171.7}
	lead, loud := NewHostView(enforced, true, 8, gpuprobe.HostLoad{}, gpuprobe.HostLoad{}).Lead()
	if !loud || !strings.HasPrefix(lead, "HOST RAM OVER (committed 128.6 of 127.7 GiB physical RAM, 8.0 GiB headroom)") {
		t.Fatalf("setup: the lead is %q (loud=%v)", lead, loud)
	}
	if !strings.Contains(lead, "a lease that states its commit is judged on the 171.7 GiB commit limit instead") {
		t.Errorf("the lead must say that a lease that states its commit is judged on the commit limit: %q", lead)
	}

	soft := enforced
	soft.CommitLimitSoft = true
	lead, _ = NewHostView(soft, true, 8, gpuprobe.HostLoad{}, gpuprobe.HostLoad{}).Lead()
	if !strings.Contains(lead, "a lease that states its commit is not judged on this line") {
		t.Errorf("on a limit the OS does not enforce a stated commit is not judged: %q", lead)
	}

	// The limit itself is gone: a stated commit waits as well, and the lead must not suggest otherwise.
	atLimit := gpuprobe.HostMemory{PhysicalGiB: 127.7, AvailableGiB: 75.6, CommitUsedGiB: 187.9, CommitLimitGiB: 187.7}
	lead, _ = NewHostView(atLimit, true, 8, gpuprobe.HostLoad{}, gpuprobe.HostLoad{}).Lead()
	if strings.Contains(lead, "states its commit") {
		t.Errorf("a reading at the enforced limit refuses a stated commit too, the lead must not qualify: %q", lead)
	}
	// Nothing is available: OVER on the available memory, with the commit charge under physical RAM. Nothing to qualify.
	noMem := gpuprobe.HostMemory{PhysicalGiB: 127.7, AvailableGiB: 0, CommitUsedGiB: 60, CommitLimitGiB: 187.7}
	lead, _ = NewHostView(noMem, true, 8, gpuprobe.HostLoad{}, gpuprobe.HostLoad{}).Lead()
	if strings.Contains(lead, "states its commit") {
		t.Errorf("OVER on the available memory is not the physical commit line, the lead must not qualify: %q", lead)
	}
}
