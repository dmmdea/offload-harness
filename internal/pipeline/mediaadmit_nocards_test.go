package pipeline

// "Queued, never refused" for a reason that is not a card. The allocator can find no card for a
// call although every card is idle: the host is short of RAM (starting another ComfyUI instance
// would push it into swap), or a transient nvidia-smi failure left the monitor's card unknown.
// Waiting a window does not fix a lease holder, but it often fixes these, and a call that waited
// it out used to leave through the raw allocator error: classed as a lease fault
// (gpu_lease_unavailable), no place in line, no way to come back.

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// krea2Binding makes the fixture's image route a bf16 Krea 2 binding. Its weights cannot fit a 16 GiB
// card, so a call declares the documented 24.5 GiB UNet + 8.3 GiB text encoder (the fixture has no
// model tree to read the real sizes from): the incident's lane.
func krea2Binding(c *config.Config) {
	c.ImageGenFamily, c.ImageGenCkpt = "krea2", "krea2_turbo_bf16.safetensors"
}

// shortHost is a 100 GiB host with the given memory committed.
func shortHost(commit float64) gpuprobe.HostMemory {
	return gpuprobe.HostMemory{PhysicalGiB: 100, AvailableGiB: 5, CommitUsedGiB: commit, CommitLimitGiB: 160}
}

// useHost puts both doors onto one host reading: the allocator's pre-filter (the pipeline's own
// reader) and the lease grant (the process-wide one). They apply one rule and must see one host.
func (f *admitFixture) useHost(t *testing.T, read func() gpuprobe.HostMemory) {
	t.Helper()
	f.p.alloc.HostMemory = func() (gpuprobe.HostMemory, bool) { return read(), true }
	restore := gpuprobe.UseHostMemoryReader(func() (gpuprobe.HostMemory, bool) { return read(), true })
	t.Cleanup(restore)
}

// lowHostRAM leaves 5 GiB of the host uncommitted: nothing the incident's lane declares fits.
func lowHostRAM(t *testing.T, f *admitFixture) {
	f.useHost(t, func() gpuprobe.HostMemory { return shortHost(95) })
}

// A resumable call gets a place in line on the cards that would qualify but for the host, with the
// reason, and resumes it when the host recovers.
func TestACallThatFoundNoCardForWantOfHostRAMGetsAPlaceInLine(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: krea2Binding})
	lowHostRAM(t, f)

	res := f.await(f.image(nil))
	if res.Meta.ErrClass != "gpu_queued" || res.DeferClass != core.DeferClassCapacity {
		t.Fatalf("want a queued capacity defer, got ok=%v class=%q/%q: %s", res.OK, res.Meta.ErrClass, res.DeferClass, res.Reason)
	}
	if !strings.Contains(res.Reason, "waiting for host RAM: needs 32.8 GiB, committed 95.0 of 100.0 GiB physical, 8.0 GiB headroom") {
		t.Errorf("the reason must say what is short: %q", res.Reason)
	}
	var p struct {
		Token   string   `json:"waiter_token"`
		Devices []string `json:"devices"`
		ETASec  int      `json:"eta_s"`
	}
	if err := json.Unmarshal(res.Data, &p); err != nil || p.Token == "" {
		t.Fatalf("no waiter_token in %s (%v)", res.Data, err)
	}
	// The monitor's card is not among the cards the place is held on.
	if len(p.Devices) != 2 || strings.Contains(strings.Join(p.Devices, ","), leaseIDOf(admitUUIDB)) {
		t.Errorf("devices = %v, want the two non-display cards", p.Devices)
	}
	if p.ETASec != 0 || strings.Contains(res.Reason, "at most 0s") {
		t.Errorf("nothing declares an end here: eta_s=%d, reason %q", p.ETASec, res.Reason)
	}

	// The host recovers; the same request with the token is served and the place is spent.
	f.useHost(t, func() gpuprobe.HostMemory { return roomyHostMem })
	f.letRunnersGo()
	again := f.await(f.image(map[string]any{"waiter_token": p.Token}))
	if !again.OK {
		t.Fatalf("resumed after the host recovered: class=%q %s", again.Meta.ErrClass, again.Reason)
	}
	if _, ok := f.m.ResumeToken(p.Token); ok {
		t.Error("a served place is dropped")
	}
}

// A door that cannot resume keeps the plain defer, with the reason, and leaves nothing behind.
func TestAHostRAMShortageForADoorThatCannotResumeIsABusyDeferWithTheReason(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: krea2Binding})
	lowHostRAM(t, f)
	res := f.await(f.plain(core.TaskGenerateImage, "a calm ocean at dawn", map[string]any{"out": filepath.Join(f.dir, "plain.png")}))
	busyNotQueued(t, res)
	if !strings.Contains(res.Reason, "waiting for host RAM") {
		t.Errorf("the reason must say what is short: %q", res.Reason)
	}
	if n := len(f.m.Tokens()); n != 0 {
		t.Errorf("%d token(s)", n)
	}
}

// When no card could qualify however long the call waited (every one is the display card, or
// unknown), there is nothing to hold a place on: the defer says so.
func TestACallThatNoCardCouldEverTakeIsABusyDeferNotAPlace(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
	for i := range f.cards {
		f.cards[i].Display = true // every card is the operator's screen, and the operator is at the desk
	}
	res := f.await(f.image(nil))
	if res.OK || res.Meta.ErrClass != "gpu_busy" {
		t.Fatalf("want the busy defer, got ok=%v class=%q: %s", res.OK, res.Meta.ErrClass, res.Reason)
	}
	if !strings.Contains(res.Reason, "display") {
		t.Errorf("the reason must say why no card qualifies: %q", res.Reason)
	}
	if n := len(f.m.Tokens()); n != 0 {
		t.Errorf("%d token(s): there is no card to hold a place on", n)
	}
}
