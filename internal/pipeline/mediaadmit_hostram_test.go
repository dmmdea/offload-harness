package pipeline

// The host-RAM term of the media admission (the paging incident of 2026-10-09). A generation call
// declares the host RAM its weights will stream from (internal/hostneed), the grant admits it
// against committed memory plus what the leases already granted have yet to load
// (internal/gpulease/hostram.go), and a call that does not fit waits in the same line with the
// reason. The fixture's image route is bound to bf16 Krea 2 (krea2Binding), which declares the
// documented 24.5 + 8.3 = 32.8 GiB on its 16 GiB cards.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

func resultTokenOf(t *testing.T, res core.Result) string {
	t.Helper()
	var p struct {
		Token string `json:"waiter_token"`
	}
	if err := json.Unmarshal(res.Data, &p); err != nil || p.Token == "" {
		t.Fatalf("no waiter_token in %s (%v)", res.Data, err)
	}
	return p.Token
}

// THE INCIDENT IN MINIATURE. Two lanes that stream the same bf16 weights, on a host that fits one:
// 40 GiB committed, 100 GiB physical, each lane 32.8 GiB. The first is admitted. The second finds
// two idle cards and a host that cannot take it on top of what the first is still loading (the
// commit charge does not show those 32.8 GiB yet), and waits, with the reason, instead of running.
// When the first is done, the second is served.
func TestTheSecondLaneWaitsWhileTheFirstIsStillLoading(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: krea2Binding})
	f.useHost(t, func() gpuprobe.HostMemory { return shortHost(40) })

	first := f.image(nil)
	f.waitStarted(1)
	second := f.await(f.image(nil))
	if second.Meta.ErrClass != "gpu_queued" || second.DeferClass != core.DeferClassCapacity {
		t.Fatalf("the second lane must wait, got ok=%v class=%q/%q: %s", second.OK, second.Meta.ErrClass, second.DeferClass, second.Reason)
	}
	for _, want := range []string{"waiting for host RAM", "needs 32.8 GiB", "still to load by leases already running"} {
		if !strings.Contains(second.Reason, want) {
			t.Errorf("the reason must contain %q: %s", want, second.Reason)
		}
	}
	if got := len(f.started()); got != 1 {
		t.Fatalf("%d runners started, want exactly the first lane's", got)
	}
	token := resultTokenOf(t, second)

	f.letRunnersGo()
	if r := f.await(first); !r.OK {
		t.Fatalf("the first lane: class=%q %s", r.Meta.ErrClass, r.Reason)
	}
	again := f.await(f.image(map[string]any{"waiter_token": token}))
	if !again.OK {
		t.Fatalf("the second lane, resumed when the first was done: class=%q %s", again.Meta.ErrClass, again.Reason)
	}
}

// The allocator found a card and the grant found the host short: the cards are free, the memory is
// not, and the call waits at the grant, in the queue, with the grant's words.
func TestAGrantTimeShortageOnAFreeCardWaitsWithTheHostRAMText(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: krea2Binding})
	// The allocator's pre-filter sees a roomy host; the grant sees the real one (committed 95 of 100).
	restore := gpuprobe.UseHostMemoryReader(func() (gpuprobe.HostMemory, bool) { return shortHost(95), true })
	t.Cleanup(restore)

	res := f.await(f.image(nil))
	if res.Meta.ErrClass != "gpu_queued" {
		t.Fatalf("want a queued answer, got ok=%v class=%q: %s", res.OK, res.Meta.ErrClass, res.Reason)
	}
	if !strings.Contains(res.Reason, "waiting for host RAM: needs 32.8 GiB, committed 95.0 of 100.0 GiB physical, 8.0 GiB headroom") {
		t.Errorf("the reason must be the grant's refusal: %s", res.Reason)
	}
	if got := len(f.started()); got != 0 {
		t.Fatalf("%d runner(s) started on a host that cannot take them", got)
	}
	token := resultTokenOf(t, res)

	restore()
	f.letRunnersGo()
	if again := f.await(f.image(map[string]any{"waiter_token": token})); !again.OK {
		t.Fatalf("resumed once the host had the room: class=%q %s", again.Meta.ErrClass, again.Reason)
	}
}

// A host that does not lease cards takes the whole-node path, and the declared need is admitted
// there too: the card table is read for the card's VRAM, the grant refuses, the call is the plain
// busy defer carrying the reason.
func TestAWholeNodeCallIsAdmittedAgainstHostRAMToo(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, noAudit: true, mutate: krea2Binding})
	restore := gpuprobe.UseHostMemoryReader(func() (gpuprobe.HostMemory, bool) { return shortHost(95), true })
	t.Cleanup(restore)

	res := f.await(f.image(nil))
	if res.OK || res.Meta.ErrClass != "gpu_busy" {
		t.Fatalf("want the busy defer, got ok=%v class=%q: %s", res.OK, res.Meta.ErrClass, res.Reason)
	}
	if !strings.Contains(res.Reason, "waiting for host RAM: needs 32.8 GiB") {
		t.Errorf("the reason must say what is short: %s", res.Reason)
	}
	if got := len(f.started()); got != 0 {
		t.Fatalf("%d runner(s) started", got)
	}

	restore()
	f.letRunnersGo()
	if again := f.await(f.image(nil)); !again.OK {
		t.Fatalf("once the host has the room the whole-node call runs: class=%q %s", again.Meta.ErrClass, again.Reason)
	}
}

// A need that no state of this host admits is a configuration fault, not a queue: it is classed apart
// from "busy", leaves no place in line, and names what to change.
func TestAnImpossibleDeclaredNeedIsAFaultNamingWhatToChange(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: krea2Binding})
	f.useHost(t, func() gpuprobe.HostMemory {
		return gpuprobe.HostMemory{PhysicalGiB: 20, AvailableGiB: 15, CommitUsedGiB: 5, CommitLimitGiB: 40}
	})
	res := f.await(f.image(nil))
	if res.OK || res.Meta.ErrClass != "gpu_lease_unavailable" {
		t.Fatalf("want a lease fault, got ok=%v class=%q: %s", res.OK, res.Meta.ErrClass, res.Reason)
	}
	for _, want := range []string{"can never admit", "--ram", "gpu_host_ram_headroom_gib"} {
		if !strings.Contains(res.Reason, want) {
			t.Errorf("the reason must contain %q: %s", want, res.Reason)
		}
	}
	if n := len(f.m.Tokens()); n != 0 {
		t.Errorf("%d token(s): waiting cannot cure this", n)
	}
}

// Overflow only: a binding whose weights fit the card declares nothing, so a box that is over its
// physical RAM does not hold a render back that streams nothing from it.
func TestACallWhoseWeightsFitTheCardIsNotHeldBackByAnOverCommittedHost(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder}) // the fixture's default binding is an SDXL checkpoint
	f.useHost(t, func() gpuprobe.HostMemory { return shortHost(400) })
	f.letRunnersGo()
	if res := f.await(f.image(nil)); !res.OK {
		t.Fatalf("an SDXL render fits its card and declares no host RAM: class=%q %s", res.Meta.ErrClass, res.Reason)
	}
}

// The declared need is stamped on the lease the call holds, where the next grant and `gpu status` read it.
func TestTheLeaseRecordCarriesTheDeclaredNeed(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: krea2Binding})
	call := f.image(nil)
	f.waitStarted(1)
	leases := f.m.Leases()
	if len(leases) != 1 || leases[0].HostRAMGiB < 32.7 || leases[0].HostRAMGiB > 32.9 {
		t.Fatalf("the live lease must carry the declared 32.8 GiB, got %+v", leases)
	}
	f.letRunnersGo()
	if r := f.await(call); !r.OK {
		t.Fatalf("the call: class=%q %s", r.Meta.ErrClass, r.Reason)
	}
}
