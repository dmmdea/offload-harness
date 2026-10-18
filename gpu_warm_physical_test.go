package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// The warm-back of a seat states no commit (it declares the seat's footprint as one figure), so it is held to physical RAM less the
// headroom and not to the commit limit. The host here has a commit limit well past physical RAM: 75 committed plus the seat's
// 21 GiB is 96 against the 92 GiB of physical RAM less the headroom, and 96 against the 132 GiB of the commit limit less it. A rule
// that judged the warm-back on the limit would load the seat; the docs say it is refused.
func TestAWarmBackIsHeldToPhysicalRAMAndNotToTheCommitLimit(t *testing.T) {
	cfgPath, m, warms := warmCardFixture(t)
	restore := gpuprobe.UseHostMemoryReader(func() (gpuprobe.HostMemory, bool) {
		return gpuprobe.HostMemory{PhysicalGiB: 100, AvailableGiB: 90, CommitUsedGiB: 75, CommitLimitGiB: 140}, true
	})
	t.Cleanup(restore)
	a := acquireCard(t, m, "lane", "gpu-aaaa0000-x")
	if err := m.MarkSeatWarmOwed("seat"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	warmBackGuarded(loadCfgPath(cfgPath), leaseWarmGuard(m, a), &out)
	if n := warms.Load(); n != 0 {
		t.Fatalf("the seat was loaded on a host whose commit charge plus the seat passes physical RAM less the headroom (%d warm(s)): %s", n, out.String())
	}
	for _, want := range []string{"NOT warming seat back", "committed 75.0 of 100.0 GiB physical", "the warm stays owed"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the refusal must say %q:\n%s", want, out.String())
		}
	}
}
