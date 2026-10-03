package nodeswap

// What a deploy touches, and which GPU leases it waits for (GPU routing P7, register C-86).
//
// Read from deps.go and nodeswap.go (2026-10-03), a node swap touches:
//
//   - the binary file: a rename pair on disk, no card;
//   - the processes running that binary which it may stop to clear the rename: the fleet-serve
//     process on a node with a restart configured (a restart cuts every job the node is running,
//     on whichever card it runs: the job count is a node-wide scalar, so that wait stays
//     node-wide), and the idle MCP helpers (a helper holds a lease in-process while it renders,
//     and stopping it would end that render);
//   - nothing else: a lease held by a `gpu reserve` wrapper keeps running its old image.
//
// A standalone node (no health URL) waits for the GPU lease to clear before it swaps. That used
// to be ANY lease, so a 20-hour render on one card of three failed a deploy that touched no
// card. The wait is still every lease unless the OPERATOR says which cards the deploy touches
// (Plan.Cards, --cards), so it never narrows on its own. Even then a lease held by a process the
// deploy would stop holds it whatever its cards: the lease records its holder's pid and the
// deploy lists the processes running the image it replaces (leases_holders_test.go).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

const (
	deployCardA = "gpu-aaaa0000-0000-4000-8000-000000000001"
	deployCardC = "gpu-cccc0000-0000-4000-8000-000000000003"
)

func standalonePlan(cards ...string) Plan {
	return Plan{
		Staged: "staged.exe", Target: "target.exe", ExpectedSHA256: "NEWHASH", BackupSuffix: "t",
		IdlePollInterval: time.Millisecond, WaitIdleTimeout: 5 * time.Second, Cards: cards,
	}
}

func stagedState() *fakeState {
	s := newFakeState()
	s.files["staged.exe"] = "NEWHASH"
	s.files["target.exe"] = "OLDHASH"
	return s
}

func renderOn(epoch uint64, devices ...string) GPULeaseInfo {
	return GPULeaseInfo{Held: true, Reason: "media render in flight", Leases: []GPULeaseOnCards{
		{Epoch: epoch, Class: "media", Reason: "media render in flight", Devices: devices},
	}}
}

// The headline: a render on card C does not hold a deploy that says it touches card A.
func TestDeployWaitsOnlyForLeasesOnItsCards(t *testing.T) {
	s := stagedState()
	s.gpuHeld = func() (GPULeaseInfo, error) { return renderOn(7, deployCardC), nil }
	out := Run(context.Background(), standalonePlan(deployCardA), s.deps(), NewLogger(nil))
	if !out.OK {
		t.Fatalf("a render on card C must not hold a deploy that touches card A: error=%q steps=%+v", out.Error, out.Steps)
	}
	if s.files["target.exe"] != "NEWHASH" {
		t.Fatal("the swap did not happen")
	}
	if len(out.Cards) != 1 || out.Cards[0] != deployCardA {
		t.Fatalf("outcome cards = %v, want the cards the deploy declared it touches", out.Cards)
	}
	if len(out.LeasesLeftAlone) != 1 || !strings.Contains(out.LeasesLeftAlone[0], "epoch 7") {
		t.Fatalf("leases left alone = %v, want the render named so the record says what the deploy did not wait for", out.LeasesLeftAlone)
	}
}

// A lease on one of the deploy's cards does hold it, until it clears.
func TestDeployWaitsForALeaseOnItsOwnCard(t *testing.T) {
	s := stagedState()
	calls := 0
	s.gpuHeld = func() (GPULeaseInfo, error) {
		calls++
		if calls < 4 {
			return renderOn(7, deployCardA), nil
		}
		return GPULeaseInfo{}, nil
	}
	out := Run(context.Background(), standalonePlan(deployCardA), s.deps(), NewLogger(nil))
	if !out.OK || calls < 4 {
		t.Fatalf("ok=%v after %d polls (error %q): the lease on the deploy's own card must be waited out", out.OK, calls, out.Error)
	}
}

// A lease that names no cards is the whole node, so it holds a deploy that names any cards.
func TestDeployWaitsForAWholeNodeLeaseWhateverItsCards(t *testing.T) {
	s := stagedState()
	s.gpuHeld = func() (GPULeaseInfo, error) { return renderOn(3), nil }
	plan := standalonePlan(deployCardA)
	plan.WaitIdleTimeout, plan.IdlePollInterval = 3*time.Second, time.Second
	out := Run(context.Background(), plan, s.deps(), NewLogger(nil))
	if out.OK || !strings.Contains(out.Error, "GPU lease never cleared") {
		t.Fatalf("ok=%v error=%q: a whole-node lease touches every card", out.OK, out.Error)
	}
	if s.files["target.exe"] != "OLDHASH" {
		t.Fatal("the binary was touched under a whole-node lease")
	}
}

// With no cards named the deploy is the whole node: every lease holds it, as before, and the
// refusal names which lease, and what cards it sits on.
func TestDeployWithoutCardsWaitsForEveryLeaseAndNamesIt(t *testing.T) {
	s := stagedState()
	s.gpuHeld = func() (GPULeaseInfo, error) { return renderOn(7, deployCardC), nil }
	plan := standalonePlan()
	plan.WaitIdleTimeout, plan.IdlePollInterval = 3*time.Second, time.Second
	out := Run(context.Background(), plan, s.deps(), NewLogger(nil))
	if out.OK {
		t.Fatal("with no cards declared the deploy is the whole node and a render on any card holds it")
	}
	for _, want := range []string{"GPU lease never cleared", "epoch 7", "media", "1 card"} {
		if !strings.Contains(out.Error, want) {
			t.Errorf("error = %q, want it to name the lease that held the deploy (%q missing)", out.Error, want)
		}
	}
	if s.files["target.exe"] != "OLDHASH" {
		t.Fatal("the binary was touched with a lease held")
	}
	if len(out.Cards) != 0 {
		t.Fatalf("outcome cards = %v, want none recorded for a whole-node deploy", out.Cards)
	}
}

// A caller whose Deps report only Held and a reason (every test and tool written before leases
// carried cards) keeps today's whole-node wait.
func TestDeployWithALeaseThatReportsNoCardsIsWholeNode(t *testing.T) {
	s := stagedState()
	s.gpuHeld = func() (GPULeaseInfo, error) { return GPULeaseInfo{Held: true, Reason: "old fake"}, nil }
	plan := standalonePlan(deployCardA)
	plan.WaitIdleTimeout, plan.IdlePollInterval = 3*time.Second, time.Second
	out := Run(context.Background(), plan, s.deps(), NewLogger(nil))
	if out.OK {
		t.Fatal("a held lease that says nothing about its cards is the whole node")
	}
}

// The real inspector carries each live lease's cards: two leases on different cards come back
// as two entries, and a lease with no cards comes back with none (whole node).
func TestInspectGPULeaseCarriesEachLeasesCards(t *testing.T) {
	dir := t.TempDir()
	info, err := inspectGPULease(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if info.Held || len(info.Leases) != 0 {
		t.Fatalf("an empty lease directory = %+v, want nothing held", info)
	}
	l1, l2 := acquireScratchLease(t, dir, "render A", deployCardA), acquireScratchLease(t, dir, "render C", deployCardC)
	defer l1()
	defer l2()
	info, err = inspectGPULease(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if !info.Held || len(info.Leases) != 2 {
		t.Fatalf("inspection = %+v, want both live leases", info)
	}
	got := map[string]bool{}
	for _, l := range info.Leases {
		if len(l.Devices) != 1 || l.Class != "media" || l.Epoch == 0 {
			t.Fatalf("lease = %+v, want a media lease on one card with its epoch", l)
		}
		got[l.Devices[0]] = true
	}
	if !got[deployCardA] || !got[deployCardC] {
		t.Fatalf("cards = %v, want both", got)
	}
}

// acquireScratchLease takes a card-scoped media lease on one card in a scratch lease directory
// and returns the function that releases it.
func acquireScratchLease(t *testing.T, dir, reason, card string) func() {
	t.Helper()
	m, err := gpulease.OpenAt(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	m.SetCardScoped(true)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: reason, Devices: []string{card}})
	if err != nil {
		t.Fatal(err)
	}
	return func() { _ = l.Release() }
}

// Card ids compare case-insensitively: an operator pastes a UUID from nvidia-smi in capitals and
// a lease records it lower-cased.
func TestDeployCardsMatchALeaseWhateverTheCase(t *testing.T) {
	s := stagedState()
	s.gpuHeld = func() (GPULeaseInfo, error) { return renderOn(7, deployCardA), nil }
	plan := standalonePlan(strings.ToUpper(deployCardA))
	plan.WaitIdleTimeout, plan.IdlePollInterval = 3*time.Second, time.Second
	out := Run(context.Background(), plan, s.deps(), NewLogger(nil))
	if out.OK {
		t.Fatal("a lease on the deploy's card, named in capitals, must still hold it")
	}
}
