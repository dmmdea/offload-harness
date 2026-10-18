package gpucards

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// `gpu status` printed "the commit condition is not applied" beside a commit limit the OS does not enforce (Linux in the default
// heuristic mode, or in mode 1). It is applied to a lease that states no commit: that one is held to physical RAM less the headroom
// on every reading, enforced limit or not, and a bare `gpu reserve --ram 10` is refused on it. The line an operator reads must say
// which commit line still holds.
func TestTheStatusLineOnASoftReadingSaysWhichCommitLineStillApplies(t *testing.T) {
	soft := gpuprobe.HostMemory{PhysicalGiB: 31.2, AvailableGiB: 25, CommitUsedGiB: 19.5, CommitLimitGiB: 46.8, CommitLimitSoft: true}
	// The premise: this reading refuses a commit nobody stated, on the commit charge against physical RAM.
	if chk := gpuprobe.HostRAMAdmits(soft, true, 10, 0, 8); chk.OK || !chk.CommitShort {
		t.Fatalf("the fixture is wrong: --ram 10 on %+v must be refused on the commit charge against physical RAM, got %+v (%s)", soft, chk, chk.Why)
	}
	line := NewHostView(soft, true, 8, gpuprobe.HostLoad{}, gpuprobe.HostLoad{}).Line()
	if strings.Contains(line, "the commit condition is not applied") {
		t.Errorf("the line says the commit condition is not applied, but a commit nobody stated is refused on it:\n%s", line)
	}
	for _, want := range []string{"not a limit this OS enforces", "a commit a lease states is not judged on it", "one it does not state is still held to physical RAM"} {
		if !strings.Contains(line, want) {
			t.Errorf("the line on a soft reading must say %q:\n%s", want, line)
		}
	}
	// An enforced limit carries no such note.
	hard := soft
	hard.CommitLimitSoft = false
	if hl := NewHostView(hard, true, 8, gpuprobe.HostLoad{}, gpuprobe.HostLoad{}).Line(); strings.Contains(hl, "not a limit this OS enforces") {
		t.Errorf("an enforced commit limit must not be described as unenforced:\n%s", hl)
	}
}
