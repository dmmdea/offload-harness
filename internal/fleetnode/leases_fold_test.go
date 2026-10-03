package fleetnode

// The singular lease block is the fold of the live leases for every reader one release behind
// AND for every reader in the current binary that reads only the singular fields (the text and
// vision remotes, the MCP door's fleet view, the delegator's leasedLanes). Each of them computes
// LeaseBusy = Busy && !Overdue (delegate/nodeview.go), so a block that says both busy and overdue
// reads as NOT busy: ranked last, never fenced. Folding Overdue as "any lease is overdue" let an
// abandoned lease on one card hide a live long render on another from every one of them. The
// fold must never tell a reader less than is true: it is busy when any lease is, and overdue
// only when no live lease is long-busy and not itself overdue.

import (
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

func foldBlock(t *testing.T, ls ...gpulease.Info) map[string]any {
	t.Helper()
	m := rawHealth(t, leasedNode(t, severalLeases(ls...), nil, nil))
	block, _ := m["lease"].(map[string]any)
	if block == nil {
		t.Fatalf("no singular lease block: %v", m)
	}
	return block
}

// The headline: lease 1 on card C ran 1 h past its window, lease 2 on card A has 6 h left. A
// reader that sees only the block must read the node as busy, not overdue.
func TestSingularBlockOverdueDoesNotMaskALiveLongLeaseOnAnotherCard(t *testing.T) {
	over := liveLease(1, gpulease.ClassMedia, []string{leaseIDOf(testUUIDc)}, -time.Hour)
	live := liveLease(2, gpulease.ClassMedia, []string{leaseIDOf(testUUIDa)}, 6*time.Hour)
	for name, ls := range map[string][]gpulease.Info{"overdue lease first": {over, live}, "live lease first": {live, over}} {
		t.Run(name, func(t *testing.T) {
			block := foldBlock(t, ls...)
			if block["busy"] != true {
				t.Fatalf("block = %v, want busy: a live 6 h render holds a card", block)
			}
			if block["overdue"] == true {
				t.Fatalf("block = %v reads overdue: every reader computes busy-and-not-overdue, so the abandoned lease hides the live render and the node reads free", block)
			}
			if rem, _ := block["remaining_sec"].(float64); rem < 5*3600 {
				t.Fatalf("remaining_sec = %v, want the live lease's time left (about 6 h)", block["remaining_sec"])
			}
		})
	}
}

// Every lease past its window: the block says overdue (ranked last, never excluded), as it does
// for one overdue lease.
func TestSingularBlockIsOverdueWhenEveryLeaseIsOverdue(t *testing.T) {
	a := liveLease(1, gpulease.ClassMedia, []string{leaseIDOf(testUUIDa)}, -time.Hour)
	c := liveLease(2, gpulease.ClassMedia, []string{leaseIDOf(testUUIDc)}, -2*time.Hour)
	block := foldBlock(t, a, c)
	if block["busy"] != true || block["overdue"] != true {
		t.Fatalf("block = %v, want busy and overdue: nothing live is long, so the reading is the abandoned one", block)
	}
}

// A live lease that is too short to count as busy does not rescue an overdue one: the node still
// reads as one an abandoned lease holds, ranked last.
func TestSingularBlockStaysOverdueBesideAShortLiveLease(t *testing.T) {
	over := liveLease(1, gpulease.ClassMedia, []string{leaseIDOf(testUUIDa)}, -time.Hour)
	short := liveLease(2, gpulease.ClassMedia, []string{leaseIDOf(testUUIDc)}, 30*time.Second)
	block := foldBlock(t, over, short)
	if block["overdue"] != true || block["busy"] != true {
		t.Fatalf("block = %v, want busy and overdue: the short lease is not a long hold", block)
	}
}

// A lease the owner standing marks overdue (its window may be far off) is overdue for the fold
// too: the fold reads the per-lease entries, standing included, not the raw window. Lease 1 has
// 6 h left but its owner is gone past the grace, so it is no live hold; lease 2 is too short to
// be one, so the block is overdue. Read off the raw window alone it would be busy and not overdue.
func TestSingularBlockFoldReadsTheStandingOverdueToo(t *testing.T) {
	standingOverdue := liveLease(1, gpulease.ClassMedia, []string{leaseIDOf(testUUIDa)}, 6*time.Hour)
	short := liveLease(2, gpulease.ClassMedia, []string{leaseIDOf(testUUIDc)}, 30*time.Second)
	standing := func(l gpulease.Info) gpulease.Standing {
		return gpulease.Standing{Overdue: l.Epoch == 1}
	}
	m := rawHealth(t, leasedNode(t, severalLeases(standingOverdue, short), standing, nil))
	block, _ := m["lease"].(map[string]any)
	if block["busy"] != true || block["overdue"] != true {
		t.Fatalf("block = %v, want busy and overdue: lease 1 is abandoned by its owner and lease 2 is no long hold", block)
	}
}
