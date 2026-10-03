package delegate

// The delegator's reading of the local lease, per contract (plan P4, register C-86): a lease
// on one card makes the local box busy only for a contract whose every local agent seat sits
// on a held card. These tests run the real lease write path in a scratch lease directory and
// the real placement chain over the flagship fixture; only the card table is synthetic.

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
	placetable "github.com/dmmdea/offload-harness/internal/placement"
)

const (
	leaseCard0 = "gpu-aaaa0000"
	leaseCard1 = "gpu-bbbb0000"
	leaseCard2 = "gpu-cccc0000"
)

// flagshipLeaseBox is the three-card flagship config rooted in a scratch lease directory,
// with a synthetic card table (PCI order 0 / 1 / 2, card 1 the display card) and the
// writer enabled. hold takes a lease on the named cards.
type flagshipLeaseBox struct {
	t   *testing.T
	cfg config.Config
	m   *gpulease.Manager
}

func newFlagshipLeaseBox(t *testing.T) *flagshipLeaseBox {
	t.Helper()
	cfg := testCfg(t)
	fx := config.FlagshipFixture()
	cfg.TierProfile, cfg.Tiers, cfg.Layers = fx.TierProfile, fx.Tiers, fx.Layers
	m, err := gpulease.OpenAt(cfg.GPULockPath, "")
	if err != nil {
		t.Fatalf("OpenAt: %v", err)
	}
	m.SetCardScoped(true)
	restore := modelaffinity.SetCardTableReader(func(context.Context, string) ([]gpuprobe.Card, string, error) {
		return []gpuprobe.Card{
			{UUID: "GPU-aaaa0000", NvidiaIndex: 0, ComfyOrder: -1},
			{UUID: "GPU-bbbb0000", NvidiaIndex: 1, Display: true, ComfyOrder: -1},
			{UUID: "GPU-cccc0000", NvidiaIndex: 2, ComfyOrder: -1},
		}, "", nil
	})
	t.Cleanup(restore)
	return &flagshipLeaseBox{t: t, cfg: cfg, m: m}
}

func (b *flagshipLeaseBox) hold(class gpulease.Class, devs ...string) {
	b.t.Helper()
	l, err := b.m.TryAcquire(class, gpulease.Options{Reason: "scratch job", Devices: devs})
	if err != nil {
		b.t.Fatalf("acquire %s on %v: %v", class, devs, err)
	}
	b.t.Cleanup(func() { _ = l.Release() })
}

func (b *flagshipLeaseBox) runner() *runner { return &runner{cfg: b.cfg} }

func agentContract(layer string) core.AgentContract {
	return core.AgentContract{Goal: "summarise the findings", Layer: layer}
}

func TestLocalLeaseForNarrowsToTheSeatsCards(t *testing.T) {
	b := newFlagshipLeaseBox(t)
	b.hold(gpulease.ClassMedia, leaseCard2)
	if got := LocalLeaseFor(b.cfg.GPULockPath, b.cfg.StateDir, []string{"0"}); got.Held {
		t.Fatalf("a seat on card 0 is not held by a card-2 lease, got %+v", got)
	}
	if got := LocalLeaseFor(b.cfg.GPULockPath, b.cfg.StateDir, []string{"2"}); !got.Held {
		t.Fatal("a seat on card 2 is held by a card-2 lease")
	}
	if got := LocalLeaseFor(b.cfg.GPULockPath, b.cfg.StateDir, nil); !got.Held {
		t.Fatal("an unknown pin is every card: held")
	}
}

func TestLocalBusyFalseWhenAFreeCardServesTheSeat(t *testing.T) {
	b := newFlagshipLeaseBox(t)
	b.hold(gpulease.ClassMedia, leaseCard2)
	r := b.runner()
	// The flagship (cards 2,1,0) is on the held card; the single layer's agent seat is on
	// card 0 and free, so the box is not busy for an unnamed agent contract.
	lease := r.localLease(agentContract(""))
	if lease.Held {
		t.Fatalf("a free card serves the contract: the local lease must read idle, got %+v", lease)
	}
	if r.anyLeaseHeld(LocalLease(b.cfg.GPULockPath, b.cfg.StateDir), []core.AgentContract{agentContract("")}) {
		t.Fatal("the deal's busy reading must agree: not busy while a free card serves the seat")
	}
}

func TestLocalBusyTrueWhenEveryLocalSeatIsOnAHeldCard(t *testing.T) {
	b := newFlagshipLeaseBox(t)
	b.hold(gpulease.ClassMedia, leaseCard2)
	b.hold(gpulease.ClassMedia, leaseCard0)
	r := b.runner()
	lease := r.localLease(agentContract(""))
	if !lease.Held {
		t.Fatal("cards 0 and 2 are held: the flagship and the single layer's seat are both on a held card")
	}
	if !r.anyLeaseHeld(LocalLease(b.cfg.GPULockPath, b.cfg.StateDir), []core.AgentContract{agentContract("")}) {
		t.Fatal("the deal's busy reading must say busy")
	}
}

func TestLocalLeaseForANamedLayerIsThatLayersCards(t *testing.T) {
	b := newFlagshipLeaseBox(t)
	b.hold(gpulease.ClassMedia, leaseCard2)
	r := b.runner()
	if got := r.localLease(agentContract("triple")); !got.Held {
		t.Fatal("a contract that names the flagship waits on the flagship's cards, which a card-2 lease holds")
	}
	if got := r.localLease(agentContract("single")); got.Held {
		t.Fatalf("the single layer's seat is on card 0, free: got %+v", got)
	}
}

func TestLocalLeaseWholeNodeLeaseHoldsEverySeat(t *testing.T) {
	b := newFlagshipLeaseBox(t)
	l, err := b.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "legacy whole-node job"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Release() })
	r := b.runner()
	for _, layer := range []string{"", "triple", "single"} {
		if got := r.localLease(agentContract(layer)); !got.Held {
			t.Errorf("layer %q: a whole-node lease holds every seat, got %+v", layer, got)
		}
	}
}

func TestLocalLeaseOnAPlainBoxStaysWholeNode(t *testing.T) {
	cfg := testCfg(t)
	m, err := gpulease.OpenAt(cfg.GPULockPath, "")
	if err != nil {
		t.Fatal(err)
	}
	m.SetCardScoped(true)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "job", Devices: []string{leaseCard2}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Release() })
	r := &runner{cfg: cfg} // no layers: no declared pins, so nothing can be narrowed
	if !r.localLease(agentContract("")).Held {
		t.Fatal("a box that declares no layers cannot place a seat on a card: every lease holds it")
	}
}

func TestLongContextClassIsNotNarrowedByTheAgentChain(t *testing.T) {
	b := newFlagshipLeaseBox(t)
	b.hold(gpulease.ClassMedia, leaseCard2)
	c := agentContract("")
	c.ContextClass = core.ContextClassLong
	if !b.runner().localLease(c).Held {
		t.Fatal("a long-context contract runs on a long seat, which the agent chain does not describe: unchanged (held)")
	}
}

func TestReservedAndFenceForNarrowToTheSeatsCards(t *testing.T) {
	b := newFlagshipLeaseBox(t)
	b.hold(gpulease.ClassText, leaseCard2) // a plain text reservation (a benchmark) on card 2
	info := LocalLease(b.cfg.GPULockPath, b.cfg.StateDir)
	if !ReservedFor(info, []string{"2"}) {
		t.Fatal("card 2 is reserved by the text lease")
	}
	if ReservedFor(info, []string{"0"}) {
		t.Fatal("card 0 is not reserved by a card-2 text lease")
	}
	if !Reserved(info) {
		t.Fatal("the whole-node reading is unchanged: any text lease reserves")
	}

	b2 := newFlagshipLeaseBox(t)
	b2.hold(gpulease.ClassMedia, leaseCard2)
	info = LocalLease(b2.cfg.GPULockPath, b2.cfg.StateDir)
	if fenced, why := FencedFor(info, []string{"2"}); !fenced || !strings.Contains(why, "media") {
		t.Fatalf("a card-2 seat is fenced by a card-2 render: %v %q", fenced, why)
	}
	if fenced, _ := FencedFor(info, []string{"0"}); fenced {
		t.Fatal("a card-0 seat is not fenced by a card-2 render")
	}
	if fenced, _ := ForeignFenceFor(info, []string{"0"}); fenced {
		t.Fatal("a card-0 seat is not foreign-fenced by a card-2 render")
	}
	if fenced, _ := ForeignFenceFor(info, []string{"2"}); !fenced {
		t.Fatal("a card-2 seat is foreign-fenced by a card-2 render")
	}
}

// ---- the deal's busy formula, end to end ---------------------------------------------------

// flagshipDecider runs the real placement table over the flagship fixture with the readers an
// idle three-card box gives it, so a Run through it exercises the runner's lease reads and
// nothing else.
func flagshipDecider(cfg config.Config) func(context.Context, core.AgentContract, Subtask) placetable.Decision {
	return tableDecider(cfg, &readings{})
}

// route=auto is busy for a contract only when every local agent seat it could use is on a held
// card. With a render on card 2 alone the single layer's card-0 seat is free: the contract
// runs HERE, and the fleet is not read, dispatched to, or waited on.
func TestRunAutoStaysLocalWhileAFreeCardServesTheContract(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	b := newFlagshipLeaseBox(t)
	b.hold(gpulease.ClassMedia, leaseCard2)
	node, url := acceptingNode(t, "node-remote", "zorblax from the remote", nil)

	var calls atomic.Int64
	results, sum, err := RunWith(context.Background(), b.cfg, passingLocal(&calls),
		[]core.AgentContract{remoteContract()}, "auto", []string{url}, &RunOptions{LocalDecider: flagshipDecider(b.cfg)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Succeeded != 1 || calls.Load() != 1 {
		t.Fatalf("summary = %+v, local calls = %d: a card-2 render must not push a card-0 contract off the box", sum, calls.Load())
	}
	if node.dispatches.Load() != 0 {
		t.Fatalf("the remote was dispatched to %d time(s) although a free local card serves the contract", node.dispatches.Load())
	}
	if results[0].Node == "node-remote" {
		t.Fatalf("placed on the remote: %+v", results[0])
	}
}

// With cards 0 and 2 both held, no local agent seat is free, so the local box is busy for the
// contract and the fleet takes it.
func TestRunAutoRoutesRemoteWhenEveryLocalSeatIsOnAHeldCard(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	b := newFlagshipLeaseBox(t)
	b.hold(gpulease.ClassMedia, leaseCard2)
	b.hold(gpulease.ClassMedia, leaseCard0)
	node, url := acceptingNode(t, "node-remote", "zorblax from the remote", nil)

	var calls atomic.Int64
	_, sum, err := RunWith(context.Background(), b.cfg, passingLocal(&calls),
		[]core.AgentContract{remoteContract()}, "auto", []string{url}, &RunOptions{LocalDecider: flagshipDecider(b.cfg)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if node.dispatches.Load() != 1 || calls.Load() != 0 {
		t.Fatalf("summary = %+v, remote dispatches = %d, local calls = %d: with no free local card the remote takes the contract", sum, node.dispatches.Load(), calls.Load())
	}
}
