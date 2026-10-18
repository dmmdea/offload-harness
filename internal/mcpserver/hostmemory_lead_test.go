package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// The one-line brief leads with OVER, and on a reading that is over only on the commit charge against physical RAM it says what a
// lease that states its commit is judged on, as the block's note does: a session reading the lead alone would hold off work the
// rule admits.
func TestTheBriefLeadSaysWhatALeaseThatStatesItsCommitIsJudgedOn(t *testing.T) {
	withHost(t, gpuprobe.HostMemory{PhysicalGiB: 127.7, AvailableGiB: 75.6, CommitUsedGiB: 128.6, CommitLimitGiB: 171.7}, true)
	view := localLeaseView(context.Background(), freeLeaseConfig(t))
	if hostBlock(t, view)["verdict"] != "OVER" {
		t.Fatalf("setup: 128.6 committed on 127.7 GiB of RAM is OVER, got %v", hostBlock(t, view)["verdict"])
	}
	line := gpuLeaseVerdictLine(view)
	const want = "HOST RAM OVER (committed 128.6 of 127.7 GiB physical RAM, 8.0 GiB headroom) — a lease that states its commit is judged on the 171.7 GiB commit limit instead; "
	if !strings.HasPrefix(line, want) {
		t.Errorf("the brief line must lead with the qualified OVER verdict:\n got %s\nwant prefix %s", line, want)
	}
}
