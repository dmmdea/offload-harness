package pipeline

// The host-RAM term of the media admission (the paging incident of 2026-10-09). A generation call
// declares the host RAM its weights will stream from (internal/hostneed), the grant admits it
// against committed memory plus what the leases already granted have yet to load
// (internal/gpulease/hostram.go), and a call that does not fit waits in the same line with the
// reason. The fixture's image route is bound to bf16 Krea 2 (krea2Binding), which declares the
// documented 24.5 + 8.3 = 32.8 GiB on its 16 GiB cards.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/hostneed"
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

// The media admission's Managers keep the configured headroom too (config.Load installs it for the whole
// process; the pipeline builds its Managers with gpulease.OpenAt and never set one of its own, so the
// grant read the built-in 8 GiB whatever gpu_host_ram_headroom_gib said). 40 GiB committed of 100 and a
// krea2 lane of 32.8: 72.8 fits under the built-in 92, and not under a headroom of 30 (limit 70).
func TestTheMediaAdmissionKeepsTheConfiguredHeadroom(t *testing.T) {
	t.Cleanup(func() { gpulease.SetDefaultHostRAMHeadroom(0) })
	gpulease.SetDefaultHostRAMHeadroom(30)
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: krea2Binding})
	f.useHost(t, func() gpuprobe.HostMemory { return shortHost(40) })

	res := f.await(f.image(nil))
	if res.OK || res.Meta.ErrClass != "gpu_queued" {
		t.Fatalf("a headroom of 30 must hold a 32.8 GiB lane back on 40 committed of 100, got ok=%v class=%q: %s", res.OK, res.Meta.ErrClass, res.Reason)
	}
	if !strings.Contains(res.Reason, "30.0 GiB headroom") {
		t.Errorf("the reason must name the headroom in force: %s", res.Reason)
	}
	if got := len(f.started()); got != 0 {
		t.Fatalf("%d runner(s) started on a host the configured headroom says is full", got)
	}
}

// A video request that names its own transformer declares the file the runner will load, not the one
// the config binds (review, 2026-10-10: the bf16 LTX-2.5 transformer is 39.13 GiB where the int8 default
// is 20.03, and the estimate sized the default whatever the request said, 19 GiB under). The model tree
// is a size table, because a 39 GiB fixture is not an option.
func TestAVideoRequestsOwnTransformerIsWhatTheAdmissionDeclares(t *testing.T) {
	const (
		int8T = "ltx-2.5-22b-distilled-transformer-comfy-int8-convrot.safetensors"
		bf16T = "ltx-2.5-22b-distilled-transformer-bf16.safetensors"
		gemma = "gemma4-12b-with-proj-ltx-2.5-comfy-int8-convrot.safetensors"
	)
	sizes := map[string]float64{int8T: 20.03, bf16T: 39.13, gemma: 14.32}
	restore := hostneed.UseStat(func(path string) (int64, bool) {
		g, ok := sizes[filepath.Base(path)]
		return int64(g * (1 << 30)), ok
	})
	t.Cleanup(restore)

	declared := func(params map[string]any) float64 {
		t.Helper()
		f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: func(c *config.Config) {
			c.VideoGenScript, c.VideoGenFamily = c.ImageGenScript, "ltx25" // the fixture's fake runner
		}})
		for _, class := range []string{"diffusion_models", "text_encoders"} {
			if err := os.MkdirAll(filepath.Join(f.cfg.ComfyDir, "models", class), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		params["out"] = filepath.Join(f.dir, "clip.mp4")
		call := f.start(core.TaskGenerateVideo, "a calm ocean at dawn", params)
		f.waitStarted(1)
		leases := f.m.Leases()
		if len(leases) != 1 {
			t.Fatalf("want one live lease, got %+v", leases)
		}
		got := leases[0].HostRAMGiB
		f.letRunnersGo()
		if r := f.await(call); !r.OK {
			t.Fatalf("the video call: class=%q %s", r.Meta.ErrClass, r.Reason)
		}
		return got
	}
	if got := declared(map[string]any{"model": "ltx25"}); got < 34.3 || got > 34.4 {
		t.Fatalf("the int8 default declares 20.03 + 14.32 = 34.35 GiB, got %.2f", got)
	}
	if got := declared(map[string]any{"model": "ltx25", "transformer": bf16T}); got < 53.4 || got > 53.5 {
		t.Fatalf("a request that names the bf16 transformer declares 39.13 + 14.32 = 53.45 GiB, got %.2f", got)
	}
}
