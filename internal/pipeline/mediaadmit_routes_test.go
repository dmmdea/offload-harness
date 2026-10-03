package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/comfyinst"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpualloc"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/imagegen"
)

// ---------------------------------------------------------------------------------------------
// Which card, which instance, which lease: the rules, one by one.
// ---------------------------------------------------------------------------------------------

// order "1,0,2" is the box's declared ComfyUI order, fastest first: ComfyUI index 0 is nvidia-smi
// index 1 (the 5070 Ti, the display card), index 1 is nvidia 0, index 2 is nvidia 2.
const admitOrder = "1,0,2"

func (f *admitFixture) leaseDevices() [][]string {
	var out [][]string
	for _, l := range f.m.Leases() {
		out = append(out, l.Devices)
	}
	return out
}

// An explicit comfy_cuda_device is a hard constraint: the call goes to the card the pin names
// and queues for it, however many other cards are free. It is never re-picked.
func TestExplicitPinIsHardConstraint(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: func(c *config.Config) { c.ComfyCudaDevice = "2" }})
	// ComfyUI index 2 is nvidia index 2: card C. Hold it, leave card A free.
	holder, err := f.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "another job", TTL: time.Hour, Devices: []string{leaseIDOf(admitUUIDC)}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Release() }()

	res := f.await(f.image(nil))
	if res.OK || res.Meta.ErrClass != "gpu_queued" {
		t.Fatalf("the pinned card is held and card A is free: the call must queue for the pinned card, got ok=%v class=%q %s", res.OK, res.Meta.ErrClass, res.Reason)
	}
	if got := f.started(); len(got) != 0 {
		t.Fatalf("the call ran on %v although its pin names the held card", got)
	}
	if !strings.Contains(string(res.Data), leaseIDOf(admitUUIDC)) || strings.Contains(string(res.Data), leaseIDOf(admitUUIDA)) {
		t.Errorf("the place it holds is for the pinned card, and only that one: %s", res.Data)
	}

	// Released, the same call (with its token) runs on the pinned card.
	_ = holder.Release()
	f.letRunnersGo()
	tok := tokenOf(t, res)
	again := f.await(f.image(map[string]any{"waiter_token": tok}))
	if !again.OK {
		t.Fatalf("resumed: %+v", again)
	}
	got := f.started()
	if len(got) != 1 || got[0].Env["COMFY_CARD_UUID"] != admitUUIDC || got[0].Env["GPU_LEASE_DEVICES"] != leaseIDOf(admitUUIDC) || got[0].Env["COMFY_CUDA_DEVICE"] != "" {
		t.Fatalf("probes = %+v, want one run on card C, bound by uuid, with no legacy index", got)
	}
}

// The pin is the operator's word: it may name the display card (ComfyUI index 0 here).
func TestAnExplicitPinMayNameTheDisplayCard(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: func(c *config.Config) { c.ComfyCudaDevice = "0" }})
	f.letRunnersGo()
	if r := f.await(f.image(nil)); !r.OK {
		t.Fatalf("%+v", r)
	}
	if got := f.started(); len(got) != 1 || got[0].Env["COMFY_CARD_UUID"] != admitUUIDB {
		t.Fatalf("probes = %+v, want the call on the display card the pin names", got)
	}
}

// Without a declared order a pin cannot be turned into a card; it is never guessed. The call holds
// the whole node and keeps today's --cuda-device.
func TestAPinThatCannotBeResolvedKeepsTheWholeNodeAndTheLegacyPin(t *testing.T) {
	for name, tc := range map[string]struct {
		order, pin string
	}{
		"order not declared": {"", "2"},
		"several cards":      {admitOrder, "1,2"},
		"not an index":       {admitOrder, "two"},
		"no such position":   {admitOrder, "7"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newAdmitFixtureWith(t, admitSpec{order: tc.order, mutate: func(c *config.Config) { c.ComfyCudaDevice = tc.pin }})
			ch := f.image(nil)
			probes := f.waitStarted(1)
			if devs := f.leaseDevices(); len(devs) != 1 || len(devs[0]) != 0 {
				t.Errorf("leases = %v, want ONE whole-node lease (no cards)", devs)
			}
			env := probes[0].Env
			if env["COMFY_CUDA_DEVICE"] != tc.pin || env["COMFY_CARD_UUID"] != "" || env["COMFY_API"] != "" || env["GPU_LEASE_DEVICES"] != "" {
				t.Errorf("env = %v, want the legacy pin %q and nothing per-card", env, tc.pin)
			}
			f.letRunnersGo()
			if r := f.await(ch); !r.OK {
				t.Fatalf("%+v", r)
			}
		})
	}
}

// A pooled route computes on the cards its keys name, through the default instance with every
// card visible (its launch is unchanged); it leases those cards, not the node.
func TestPoolRouteRequestsPoolCards(t *testing.T) {
	pool := func(c *config.Config) {
		c.ImageGenPoolVvramGB, c.ImageGenPoolCompute, c.ImageGenPoolDonor = 24, "cuda:2", "cuda:1"
		c.ComfyCudaDevice = "2" // a pooled seat is never pinned
	}
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: pool})
	ch := f.image(nil)
	probes := f.waitStarted(1)
	want := []string{leaseIDOf(admitUUIDA), leaseIDOf(admitUUIDC)} // cuda:1 = nvidia 0, cuda:2 = nvidia 2, sorted whatever the keys' order
	if devs := f.leaseDevices(); len(devs) != 1 || strings.Join(devs[0], ",") != strings.Join(want, ",") {
		t.Errorf("lease devices = %v, want the pool's cards %v", devs, want)
	}
	env := probes[0].Env
	if env["COMFY_CARD_UUID"] != "" || env["COMFY_API"] != "" || env["COMFY_CUDA_DEVICE"] != "" {
		t.Errorf("a pooled route runs in the default instance with every card visible: env %v", env)
	}
	if env["GPU_LEASE_DEVICES"] != strings.Join(want, ",") {
		t.Errorf("GPU_LEASE_DEVICES = %q, want %q", env["GPU_LEASE_DEVICES"], strings.Join(want, ","))
	}
	f.letRunnersGo()
	if r := f.await(ch); !r.OK {
		t.Fatalf("%+v", r)
	}
}

// Two default-instance jobs must never run at once, and the lease is what keeps them apart: on a
// box of at most three cards two pools of two always overlap, on a larger one they need not, so
// the whole node is held.
func TestAPoolOnABoxOfFourOrMoreCardsHoldsTheWholeNode(t *testing.T) {
	pool := func(c *config.Config) {
		c.ImageGenPoolVvramGB, c.ImageGenPoolCompute, c.ImageGenPoolDonor = 24, "cuda:1", "cuda:2"
	}
	f := newAdmitFixtureWith(t, admitSpec{order: "1,0,2,3", fourCards: true, mutate: pool})
	ch := f.image(nil)
	f.waitStarted(1)
	if devs := f.leaseDevices(); len(devs) != 1 || len(devs[0]) != 0 {
		t.Errorf("leases = %v, want ONE whole-node lease", devs)
	}
	f.letRunnersGo()
	f.await(ch)

	// And with no declared order the pool's keys cannot become cards at all.
	g := newAdmitFixtureWith(t, admitSpec{order: "", mutate: pool})
	ch = g.image(nil)
	g.waitStarted(1)
	if devs := g.leaseDevices(); len(devs) != 1 || len(devs[0]) != 0 {
		t.Errorf("order not declared: leases = %v, want ONE whole-node lease", devs)
	}
	g.letRunnersGo()
	g.await(ch)
}

func TestPoolMayBeScopedOnlyWhenEveryPairOverlaps(t *testing.T) {
	for _, tc := range []struct {
		set, total int
		want       bool
	}{
		{2, 3, true}, {3, 3, true}, {2, 2, true},
		{1, 3, false}, {0, 3, false}, // one card cannot be guaranteed to overlap another's
		{2, 4, false}, {3, 4, false}, {2, 8, false},
	} {
		if got := poolMayBeScoped(tc.set, tc.total); got != tc.want {
			t.Errorf("poolMayBeScoped(%d, %d) = %v, want %v", tc.set, tc.total, got, tc.want)
		}
	}
}

// run-graph holds the whole node unless the operator declares devices.
func TestRunGraphStaysWholeNodeUnlessDevicesDeclared(t *testing.T) {
	graph := func(f *admitFixture, devices any) <-chan core.Result {
		f.cfg.RunGraphScript = f.cfg.ImageGenScript
		f.p.cfg.RunGraphScript = f.cfg.ImageGenScript
		gp := filepath.Join(f.dir, "g.json")
		_ = os.WriteFile(gp, []byte("{}"), 0o644)
		params := map[string]any{"graph_path": gp, "out_dir": f.dir}
		if devices != nil {
			params["devices"] = devices
		}
		return f.start(core.TaskRunGraph, "", params)
	}
	t.Run("no devices: the whole node", func(t *testing.T) {
		f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
		ch := graph(f, nil)
		probes := f.waitStarted(1)
		if devs := f.leaseDevices(); len(devs) != 1 || len(devs[0]) != 0 {
			t.Errorf("leases = %v, want ONE whole-node lease", devs)
		}
		if env := probes[0].Env; env["COMFY_CARD_UUID"] != "" || env["GPU_LEASE_DEVICES"] != "" {
			t.Errorf("env = %v, want nothing per-card", env)
		}
		f.letRunnersGo()
		if r := f.await(ch); !r.OK {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("one declared device: that card, in its instance", func(t *testing.T) {
		f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
		ch := graph(f, []any{"2"})
		probes := f.waitStarted(1)
		if env := probes[0].Env; env["COMFY_CARD_UUID"] != admitUUIDC || env["GPU_LEASE_DEVICES"] != leaseIDOf(admitUUIDC) || env["COMFY_API"] == "" {
			t.Errorf("env = %v, want card C by uuid at its instance", env)
		}
		f.letRunnersGo()
		f.await(ch)
	})
	// Several declared devices run in the DEFAULT instance, which sees every card, and an arbitrary
	// graph can place work on any of them: a lease on only some would not keep it off the others
	// (the card another call's own instance holds, the display card). So several devices hold the
	// whole node; only a single device, run in that card's own pinned instance, is scoped.
	t.Run("two declared devices: the whole node, because the graph sees every card", func(t *testing.T) {
		f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
		ch := graph(f, "2, 0")
		probes := f.waitStarted(1)
		if devs := f.leaseDevices(); len(devs) != 1 || len(devs[0]) != 0 {
			t.Errorf("leases = %v, want ONE whole-node lease", devs)
		}
		if env := probes[0].Env; env["GPU_LEASE_DEVICES"] != "" || env["COMFY_CARD_UUID"] != "" {
			t.Errorf("env = %v, want nothing per-card: a lease on some cards does not confine a graph that sees all of them", env)
		}
		f.letRunnersGo()
		f.await(ch)
	})
	t.Run("two declared devices on a four-card box: the whole node as well", func(t *testing.T) {
		f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, fourCards: true})
		ch := graph(f, []any{"0", "1"})
		f.waitStarted(1)
		if devs := f.leaseDevices(); len(devs) != 1 || len(devs[0]) != 0 {
			t.Errorf("leases = %v, want ONE whole-node lease", devs)
		}
		f.letRunnersGo()
		f.await(ch)
	})
	t.Run("a device that does not exist is the caller's mistake, never widened", func(t *testing.T) {
		f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
		res := f.await(graph(f, []any{"9"}))
		if res.OK || !res.Deferred || !strings.Contains(res.Reason, "devices") {
			t.Fatalf("want a defer naming the devices, got %+v", res)
		}
		if got := f.started(); len(got) != 0 {
			t.Fatalf("a run-graph whose devices cannot be resolved must not run: %+v", got)
		}
	})
}

// A host that has not turned card-scoped leases on (no flag, or no green audit) behaves exactly
// as before: a whole-node lease, the legacy pin, no per-card env.
func TestAHostWithoutCardScopedLeasesIsByteIdentical(t *testing.T) {
	for name, spec := range map[string]admitSpec{
		"flag off": {order: admitOrder, mutate: func(c *config.Config) { c.GPUCardScopedLeases = false; c.ComfyCudaDevice = "2" }},
		"no audit": {order: admitOrder, noAudit: true, mutate: func(c *config.Config) { c.ComfyCudaDevice = "2" }},
	} {
		t.Run(name, func(t *testing.T) {
			f := newAdmitFixtureWith(t, spec)
			ch := f.image(nil)
			probes := f.waitStarted(1)
			if devs := f.leaseDevices(); len(devs) != 1 || len(devs[0]) != 0 {
				t.Errorf("leases = %v, want ONE whole-node lease", devs)
			}
			env := probes[0].Env
			if env["COMFY_CUDA_DEVICE"] != "2" {
				t.Errorf("COMFY_CUDA_DEVICE = %q, want the configured legacy pin", env["COMFY_CUDA_DEVICE"])
			}
			for _, k := range []string{"COMFY_CARD_UUID", "COMFY_API", "GPU_LEASE_DEVICES", "GPU_LEASE_UNLOAD_MODELS"} {
				if v, ok := env[k]; ok {
					t.Errorf("%s = %q must not be set on a whole-node call", k, v)
				}
			}
			f.letRunnersGo()
			if r := f.await(ch); !r.OK {
				t.Fatalf("%+v", r)
			}
			if got := f.stops.list(); len(got) != 0 {
				t.Errorf("a whole-node call stops no kept instances: %+v", got)
			}
		})
	}
}

// With no card table the card path cannot choose a card: the whole node, as before, said once.
func TestAnUnreadableCardTableFallsBackToTheWholeNode(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
	f.p.alloc.Cards = func(context.Context, config.Config) ([]gpuprobe.Card, string, error) {
		return nil, "", errors.New("nvidia-smi: not on PATH")
	}
	ch := f.image(nil)
	probes := f.waitStarted(1)
	if devs := f.leaseDevices(); len(devs) != 1 || len(devs[0]) != 0 {
		t.Errorf("leases = %v, want ONE whole-node lease", devs)
	}
	if env := probes[0].Env; env["COMFY_CARD_UUID"] != "" {
		t.Errorf("env = %v", env)
	}
	f.letRunnersGo()
	if r := f.await(ch); !r.OK {
		t.Fatalf("the render must still run: %+v", r)
	}
}

// While the operator is at the desk the allocator never hands out the card the monitor is
// attached to; when they are away it may.
func TestTheDisplayCardIsAllocatableOnlyWhenTheOperatorIsAway(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{})
	a, b := f.image(nil), f.image(nil)
	f.waitStarted(2)
	for _, pr := range f.started() {
		if pr.Env["COMFY_CARD_UUID"] == admitUUIDB {
			t.Fatalf("the display card was auto-assigned at the desk: %+v", pr.Env)
		}
	}
	f.letRunnersGo()
	f.await(a)
	f.await(b)

	g := newAdmitFixtureWith(t, admitSpec{})
	g.away = true
	chans := make([]<-chan core.Result, 3)
	for i := range chans {
		chans[i] = g.image(nil)
	}
	g.waitStarted(3)
	seen := map[string]bool{}
	for _, pr := range g.started() {
		seen[pr.Env["COMFY_CARD_UUID"]] = true
	}
	if len(seen) != 3 || !seen[admitUUIDB] {
		t.Errorf("with the operator away all three cards run one call each, the display card included: %v", seen)
	}
	g.letRunnersGo()
	for _, ch := range chans {
		g.await(ch)
	}
}

// The runner is given a lease, an instance and a free target per route; and every instance kept
// under the lease is stopped before the lease is released.
func TestPostRunFreeHitsTheInstanceThatRan(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
	f.letRunnersGo()
	if r := f.await(f.image(map[string]any{"waiter_token": ""})); !r.OK {
		t.Fatalf("%+v", r)
	}
	got := f.started()
	if len(got) != 1 {
		t.Fatalf("probes = %+v", got)
	}
	var card gpuprobe.Card
	for _, c := range f.cards {
		if c.UUID == got[0].Env["COMFY_CARD_UUID"] {
			card = c
		}
	}
	want := fmt.Sprintf("POST /card%d/free", card.NvidiaIndex)
	frees := f.frees.paths()
	if len(frees) != 1 || frees[0] != want {
		t.Fatalf("post-run /free = %v, want exactly %q: the instance that ran", frees, want)
	}
}

func TestInstanceStoppedWithLease(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
	// One call alone: its kept instances are stopped while its lease is STILL HELD, so the next
	// holder never finds one on its card.
	f.letRunnersGo()
	if r := f.await(f.image(nil)); !r.OK {
		t.Fatalf("%+v", r)
	}
	if one := f.stops.list(); len(one) != 1 || !one[0].held {
		t.Fatalf("stop calls = %+v, want one, made while the lease was still held", one)
	}
	if err := os.Remove(filepath.Join(f.dir, "go")); err != nil {
		t.Fatal(err)
	}
	for _, p := range f.started() { // forget the first run's probe
		_ = os.Remove(filepath.Join(f.dir, fmt.Sprintf("started-%d.json", p.PID)))
	}
	f.stops.mu.Lock()
	f.stops.calls = nil
	f.stops.mu.Unlock()
	a, b := f.image(nil), f.image(nil)
	f.waitStarted(2)
	epochs := map[string]bool{}
	for _, pr := range f.started() {
		epochs[pr.Env["GPU_LEASE_EPOCH"]] = true
	}
	f.letRunnersGo()
	f.await(a)
	f.await(b)
	stops := f.stops.list()
	if len(stops) != 2 {
		t.Fatalf("stop calls = %+v, want one per released lease", stops)
	}
	for _, s := range stops {
		if !epochs[fmt.Sprint(s.epoch)] {
			t.Errorf("stop for epoch %d, but the calls ran under %v", s.epoch, epochs)
		}
		if s.dir != f.cfg.ComfyDir {
			t.Errorf("stopped instances in %q, want the configured comfy_dir %q", s.dir, f.cfg.ComfyDir)
		}
	}
	// "Before the lease is released": the first stop saw the other lease AND its own still held
	// (two live leases), the stop is not run after the fact.
	if !stops[0].held {
		t.Error("the instances kept under a lease must be stopped while the lease is still held")
	}
}

// The free target and the lease env reach EVERY single-card route, not just image generation.
func TestLaunchProfileOnACardScopedHostBindsEverySingleCardRoute(t *testing.T) {
	requireNodePipeline(t)
	single := map[string]bool{
		"generate_image (comfy)": true, "edit_image_generative": true, "inpaint_image": true, "upscale_image": true,
		"image batch": true, "animate_character": true, "generate_audio (music)": true,
		"generate_image (comfy-render direct)": true, "generate_video": true,
	}
	for _, tc := range gpuLeaseCases() {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			probe := filepath.Join(dir, "probe.json")
			t.Setenv("RUNNER_PROBE", probe)
			t.Setenv("PNG_SRC", "")
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "gpu"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "gpu", "reader-audit.json"), []byte(`{"result":"green"}`), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg := config.Default()
			cfg.MediaDir, cfg.StateDir, cfg.GPUCardScopedLeases, cfg.GPUComfyOrder = dir, root, true, admitOrder
			cfg.ComfyDynamicVRAM, cfg.ComfyExtraArgs = "on", "--verbose"
			cfg.Endpoint = "" // no roster: the unload list is its own test
			stub := writeProbeRunner(t, dir)
			tc.setup(t, &cfg, stub, dir)
			devs, _ := gpuprobe.BuildCards([]gpuprobe.Device{
				{Index: 0, UUID: admitUUIDA, TotalGiB: 16, FreeGiB: 15, UtilKnown: true},
				{Index: 1, UUID: admitUUIDB, TotalGiB: 16, FreeGiB: 14, UtilKnown: true, DisplayAttached: true},
				{Index: 2, UUID: admitUUIDC, TotalGiB: 16, FreeGiB: 15, UtilKnown: true},
			}, admitOrder)
			p := &Pipeline{cfg: cfg}
			p.alloc = gpualloc.Deps{
				Cards:       func(context.Context, config.Config) ([]gpuprobe.Card, string, error) { return devs, "", nil },
				HostFreeRAM: func() (float64, bool) { return 64, true },
				Presence:    func(config.Config) (bool, bool) { return true, false },
			}
			// Port 1 refuses at once: the post-run /free is not part of this test and must not wait.
			p.instanceAPI = func(c gpuprobe.Card) string { return fmt.Sprintf("http://127.0.0.1:1/card%d", c.NvidiaIndex) }
			p.stopKept = func(context.Context, string, uint64) []comfyinst.Outcome { return nil }
			tc.invoke(t, p, dir)
			got := readProbe(t, probe)

			if single[tc.name] {
				u := got.Env["COMFY_CARD_UUID"]
				if u != admitUUIDA && u != admitUUIDC {
					t.Errorf("COMFY_CARD_UUID = %q, want a non-display card (env %v)", u, got.Env)
				}
				if !strings.HasPrefix(got.Env["COMFY_API"], "http://127.0.0.1:1/card") {
					t.Errorf("COMFY_API = %q, want the card's instance", got.Env["COMFY_API"])
				}
				if got.Env["COMFY_CUDA_DEVICE"] != "" {
					t.Errorf("COMFY_CUDA_DEVICE = %q: a card-bound instance is pinned by uuid, never by an index", got.Env["COMFY_CUDA_DEVICE"])
				}
				if got.Env["GPU_LEASE_DEVICES"] != strings.ToLower(u) {
					t.Errorf("GPU_LEASE_DEVICES = %q, want %q", got.Env["GPU_LEASE_DEVICES"], strings.ToLower(u))
				}
				if got.Env["COMFY_DYNAMIC_VRAM"] != "on" || got.Env["COMFY_EXTRA_ARGS"] != "--verbose" {
					t.Errorf("the launch-wide keys must still reach the runner: %v", got.Env)
				}
				return
			}
			if got.Env["COMFY_CARD_UUID"] != "" || got.Env["COMFY_API"] != "" {
				t.Errorf("this route is not a single-card ComfyUI route and must not be bound to a card: %v", got.Env)
			}
		})
	}
}

// ---------------------------------------------------------------------------------------------
// A process running under its parent's lease on several cards
// ---------------------------------------------------------------------------------------------

func TestAnInheritedLeaseOnTwoCardsServesTwoCallsOnTwoCards(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
	parent, err := f.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "gpu reserve --devices", TTL: time.Hour,
		Devices: []string{leaseIDOf(admitUUIDA), leaseIDOf(admitUUIDC)}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = parent.Release() }()
	t.Setenv("GPU_LEASE_DIR", parent.Dir())
	t.Setenv("GPU_LEASE_EPOCH", fmt.Sprint(parent.Epoch()))
	t.Setenv("GPU_LEASE_CLASS", "media")
	t.Setenv("GPU_LEASE_DEVICES", leaseIDOf(admitUUIDA)+","+leaseIDOf(admitUUIDC))

	a, b := f.image(nil), f.image(nil)
	probes := f.waitStarted(2)
	seen := map[string]bool{}
	for _, pr := range probes {
		seen[pr.Env["COMFY_CARD_UUID"]] = true
		if pr.Env["GPU_LEASE_EPOCH"] != fmt.Sprint(parent.Epoch()) {
			t.Errorf("the call must run under the parent's lease (epoch %d): %v", parent.Epoch(), pr.Env)
		}
	}
	if len(seen) != 2 || !seen[admitUUIDA] || !seen[admitUUIDC] || seen[admitUUIDB] {
		t.Fatalf("calls ran on %v, want one each on the parent's two cards", seen)
	}
	if n := len(f.m.Leases()); n != 1 {
		t.Errorf("%d live leases: a call under an inherited lease takes none of its own", n)
	}
	// A third call has no card of the lease to run on: it waits its window and answers busy (there
	// is no lease queue to hold a place in: the lease is the parent's).
	res := f.await(f.image(nil))
	if res.OK || res.Meta.ErrClass != "gpu_busy" {
		t.Errorf("third call: ok=%v class=%q %s, want the legacy busy answer", res.OK, res.Meta.ErrClass, res.Reason)
	}
	f.letRunnersGo()
	f.await(a)
	f.await(b)
	// Both cards are free again: a later call runs.
	if r := f.await(f.image(nil)); !r.OK {
		t.Fatalf("the slots of the finished calls must be released: %+v", r)
	}
}

func TestAnInheritedWholeNodeLeaseKeepsTheLegacySlot(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
	parent, err := f.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "gpu reserve", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = parent.Release() }()
	t.Setenv("GPU_LEASE_DIR", parent.Dir())
	t.Setenv("GPU_LEASE_EPOCH", fmt.Sprint(parent.Epoch()))
	t.Setenv("GPU_LEASE_CLASS", "media")
	t.Setenv("GPU_LEASE_DEVICES", "")
	ch := f.image(nil)
	probes := f.waitStarted(1)
	if env := probes[0].Env; env["COMFY_CARD_UUID"] != "" || env["GPU_LEASE_EPOCH"] != fmt.Sprint(parent.Epoch()) {
		t.Errorf("env = %v, want the parent's whole-node lease and nothing per-card", env)
	}
	f.letRunnersGo()
	f.await(ch)
}

// ---------------------------------------------------------------------------------------------
// Never two calls on one card
// ---------------------------------------------------------------------------------------------

// Six calls, two free cards, all in flight with a long wait: at most two run at once, never two on
// one card, and every one completes.
func TestNeverTwoCallsOnOneCard(t *testing.T) {
	t.Setenv("ADMIT_HOLD_MS", "120")
	f := newAdmitFixtureWith(t, admitSpec{mutate: func(c *config.Config) { c.GPUWaitMs = 60000 }})
	var chans []<-chan core.Result
	for i := 0; i < 6; i++ {
		chans = append(chans, f.image(nil))
	}
	// Watch the live leases for as long as the calls take: every sample must show disjoint cards.
	stop := make(chan struct{})
	var violations atomic.Int32
	var maxLeases atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			seen := map[string]bool{}
			ls := f.m.Leases()
			if int32(len(ls)) > maxLeases.Load() {
				maxLeases.Store(int32(len(ls)))
			}
			for _, l := range ls {
				for _, d := range l.Devices {
					if seen[d] {
						violations.Add(1)
					}
					seen[d] = true
				}
				if len(l.Devices) != 1 {
					violations.Add(1)
				}
			}
			time.Sleep(3 * time.Millisecond)
		}
	}()
	f.waitStarted(2)
	f.letRunnersGo() // the runners hold their card a moment (ADMIT_HOLD_MS), so the queue drains through two cards
	for i, ch := range chans {
		if r := f.await(ch); !r.OK {
			t.Errorf("call %d did not complete: %+v", i, r)
		}
	}
	close(stop)
	<-done
	if v := violations.Load(); v != 0 {
		t.Fatalf("%d sample(s) showed two leases on one card or a lease holding more than one", v)
	}
	if maxLeases.Load() > 2 {
		t.Errorf("%d leases were live at once on a box with two free cards", maxLeases.Load())
	}
}

// ---------------------------------------------------------------------------------------------
// The place in line
// ---------------------------------------------------------------------------------------------

func tokenOf(t *testing.T, res core.Result) string {
	t.Helper()
	var p struct {
		Token string `json:"waiter_token"`
	}
	if err := json.Unmarshal(res.Data, &p); err != nil || p.Token == "" {
		t.Fatalf("no waiter_token in %s (%v)", res.Data, err)
	}
	return p.Token
}

// A caller that comes back keeps ONE name for its place: the second give-up reuses the token.
func TestAQueuedCallerKeepsOneTokenAcrossRecalls(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: func(c *config.Config) { c.ComfyCudaDevice = "2" }})
	holder, err := f.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "another job", TTL: time.Hour, Devices: []string{leaseIDOf(admitUUIDC)}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Release() }()
	first := f.await(f.image(nil))
	tok := tokenOf(t, first)
	second := f.await(f.image(map[string]any{"waiter_token": tok}))
	if tokenOf(t, second) != tok {
		t.Fatalf("the second answer carries %q, want the same token %q", tokenOf(t, second), tok)
	}
	if n := len(f.m.Tokens()); n != 1 {
		t.Fatalf("%d tokens on disk, want one place in line", n)
	}
	tk, _ := f.m.ResumeToken(tok)
	if time.Since(tk.Since()) < 500*time.Millisecond {
		t.Errorf("the place must keep its ORIGINAL arrival time, got %v ago", time.Since(tk.Since()))
	}
}

// A token nobody holds (expired, made up) is a new arrival, never an error.
func TestAnUnknownTokenIsANewArrival(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{})
	f.letRunnersGo()
	r := f.await(f.image(map[string]any{"waiter_token": "tk-" + strings.Repeat("z", 12)}))
	if !r.OK {
		t.Fatalf("a call with an unknown token on a free box must run: %+v", r)
	}
}

func TestAQueuedAnswerCountsAsBusyForTheCLIDoors(t *testing.T) {
	if !IsGPUBusy(&errGPUQueued{Token: "tk-abcdefgh"}) {
		t.Error("a queued answer is the card being busy, as far as a door that cannot resume is concerned")
	}
	if !IsGPUBusy(fmt.Errorf("wrapped: %w", &errGPUBusy{detail: "x"})) {
		t.Error("the legacy busy error must still count")
	}
	if IsGPUBusy(errors.New("something else")) {
		t.Error("an unrelated error is not busy")
	}
}

// ---------------------------------------------------------------------------------------------
// Pure pieces
// ---------------------------------------------------------------------------------------------

func TestInstanceEndpointIsStablePerCard(t *testing.T) {
	t.Setenv("COMFY_PORT_BASE", "")
	for idx, want := range map[int]string{0: "http://127.0.0.1:8189", 1: "http://127.0.0.1:8190", 2: "http://127.0.0.1:8191"} {
		if got := defaultInstanceAPI(gpuprobe.Card{NvidiaIndex: idx}); got != want {
			t.Errorf("card %d: %q, want %q", idx, got, want)
		}
	}
	t.Setenv("COMFY_PORT_BASE", "9300")
	if got := defaultInstanceAPI(gpuprobe.Card{NvidiaIndex: 2}); got != "http://127.0.0.1:9302" {
		t.Errorf("COMFY_PORT_BASE must move the range: %q", got)
	}
	t.Setenv("COMFY_PORT_BASE", "80") // below the range the render layer accepts
	if got := defaultInstanceAPI(gpuprobe.Card{NvidiaIndex: 0}); got != "http://127.0.0.1:8189" {
		t.Errorf("an unusable base is ignored: %q", got)
	}
}

func TestGrantLaunchBindsTheCardAndBlanksTheIndex(t *testing.T) {
	c := gpuprobe.Card{UUID: admitUUIDC, NvidiaIndex: 2}
	g := mediaGrant{Card: &c, API: "http://127.0.0.1:8191"}
	l := g.launch(imagegen.ComfyLaunch{CudaDevice: "2", DynamicVRAM: "on", ExtraArgs: "--x"})
	if l.CardUUID != admitUUIDC || l.API != "http://127.0.0.1:8191" || l.CudaDevice != "" || l.DynamicVRAM != "on" || l.ExtraArgs != "--x" {
		t.Fatalf("launch = %+v", l)
	}
	// A whole-node grant leaves the launch exactly as the binding wrote it.
	if l := (mediaGrant{}).launch(imagegen.ComfyLaunch{CudaDevice: "2"}); l.CudaDevice != "2" || l.CardUUID != "" || l.API != "" {
		t.Fatalf("a whole-node grant must not touch the launch: %+v", l)
	}
}

func TestDeclaredDevicesAcceptsAListOrACommaString(t *testing.T) {
	for in, want := range map[string]string{"": "", " ": ""} {
		if got := strings.Join(declaredDevices(map[string]any{"devices": in}), "|"); got != want {
			t.Errorf("%q -> %q", in, got)
		}
	}
	if got := strings.Join(declaredDevices(map[string]any{"devices": "0, GPU-aaaa ,,2"}), "|"); got != "0|GPU-aaaa|2" {
		t.Errorf("comma string -> %q", got)
	}
	if got := strings.Join(declaredDevices(map[string]any{"devices": []any{"1", 2, " 3 "}}), "|"); got != "1|3" {
		t.Errorf("[]any -> %q (a non-string entry is dropped)", got)
	}
	if got := strings.Join(declaredDevices(map[string]any{"devices": []string{"4"}}), "|"); got != "4" {
		t.Errorf("[]string -> %q", got)
	}
	if declaredDevices(nil) != nil || declaredDevices(map[string]any{}) != nil {
		t.Error("nothing declared is nil")
	}
}

// ---------------------------------------------------------------------------------------------
// Fairness: a card promised to a caller in line is not free for a newcomer
// ---------------------------------------------------------------------------------------------

func (f *admitFixture) leaveToken(devices []string, since time.Time) gpulease.Token {
	f.t.Helper()
	tok, err := f.m.LeaveToken(gpulease.ClassMedia, gpulease.Options{Reason: "image-gen", Devices: devices}, since)
	if err != nil {
		f.t.Fatal(err)
	}
	return tok
}

// A card a caller holds a place for (a live token) is not free for a newcomer, even though no lease
// shows it: the newcomer takes another card instead.
func TestACardHeldForAQueuedCallerIsNotFreeForANewcomer(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{})
	f.leaveToken([]string{leaseIDOf(admitUUIDA)}, time.Now().Add(-time.Minute))
	ch := f.image(nil)
	probes := f.waitStarted(1)
	if got := probes[0].Env["COMFY_CARD_UUID"]; got != admitUUIDC {
		t.Fatalf("the newcomer ran on %s; card A is promised to the caller holding a place for it, so it must take card C", got)
	}
	f.letRunnersGo()
	if r := f.await(ch); !r.OK {
		t.Fatalf("%+v", r)
	}
}

// A place held for the whole node is a barrier: with both cards free the newcomer still queues.
func TestAQueuedWholeNodeRequestIsABarrierForANewcomer(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{})
	f.leaveToken(nil, time.Now().Add(-time.Minute))
	res := f.await(f.image(nil))
	if res.OK || res.Meta.ErrClass != "gpu_queued" {
		t.Fatalf("want a queued answer behind the whole-node place, got ok=%v class=%q %s", res.OK, res.Meta.ErrClass, res.Reason)
	}
	if got := f.started(); len(got) != 0 {
		t.Fatalf("the newcomer ran although a whole-node request is ahead: %+v", got)
	}
	if !strings.Contains(string(res.Data), `"queue_position":2`) {
		t.Errorf("position = %s, want 2: the whole-node place is first", res.Data)
	}
}

// With every non-display card promised to callers ahead, a newcomer waits behind them and answers
// with its own place, not with a failure.
func TestACallBehindLiveTokensAnswersQueuedNotFailed(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{})
	f.leaveToken([]string{leaseIDOf(admitUUIDA)}, time.Now().Add(-2*time.Minute))
	f.leaveToken([]string{leaseIDOf(admitUUIDC)}, time.Now().Add(-time.Minute))
	res := f.await(f.image(nil))
	if res.OK || res.Meta.ErrClass != "gpu_queued" || res.DeferClass != core.DeferClassCapacity {
		t.Fatalf("want a queued capacity defer, got ok=%v class=%q/%q: %s", res.OK, res.Meta.ErrClass, res.DeferClass, res.Reason)
	}
	if !strings.Contains(string(res.Data), `"queue_position":2`) {
		t.Errorf("payload = %s, want position 2 (one place ahead on the card it waits for... and itself)", res.Data)
	}
	if n := len(f.m.Tokens()); n != 3 {
		t.Errorf("%d tokens, want the two ahead and the newcomer's own", n)
	}
}

// A place whose caller has been gone past the grace no longer holds its card.
func TestAnAbsentTokenDoesNotHoldACardBack(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{})
	dir := filepath.Join(f.root, "gpu", "tokens")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Polled two minutes ago: past the 30 s grace, inside the ten-minute window.
	rec := fmt.Sprintf(`{"id":"tk-%s","class":"media","devices":[%q],"since_ms":%d,"polled_ms":%d}`,
		strings.Repeat("a", 12), leaseIDOf(admitUUIDA), time.Now().Add(-3*time.Minute).UnixMilli(), time.Now().Add(-2*time.Minute).UnixMilli())
	if err := os.WriteFile(filepath.Join(dir, "tk-"+strings.Repeat("a", 12)+".json"), []byte(rec), 0o644); err != nil {
		t.Fatal(err)
	}
	ch := f.image(nil)
	probes := f.waitStarted(1)
	if got := probes[0].Env["COMFY_CARD_UUID"]; got != admitUUIDA {
		t.Fatalf("ran on %s: the absent caller's place must not hold card A back (the lowest free card)", got)
	}
	f.letRunnersGo()
	f.await(ch)
}

// A job in this process holds a card it has no lease record for (yet): that card is claimed.
func TestACardHeldInProcessIsNotAllocated(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{})
	if !mediaSlots.tryTake([]string{leaseIDOf(admitUUIDA)}) {
		t.Fatal("setup: the slot is not free")
	}
	defer mediaSlots.release([]string{leaseIDOf(admitUUIDA)})
	ch := f.image(nil)
	probes := f.waitStarted(1)
	if got := probes[0].Env["COMFY_CARD_UUID"]; got != admitUUIDC {
		t.Fatalf("ran on %s although another job in this process holds card A", got)
	}
	f.letRunnersGo()
	f.await(ch)
}

// A whole-node lease held by another job queues both kinds of call: the allocator sees every card
// claimed, and a pinned call sees its card claimed.
func TestACallQueuesBehindAWholeNodeLease(t *testing.T) {
	for name, pin := range map[string]string{"allocated": "", "pinned": "2"} {
		t.Run(name, func(t *testing.T) {
			f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: func(c *config.Config) { c.ComfyCudaDevice = pin }})
			holder, err := f.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "a whole-node job", TTL: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = holder.Release() }()
			res := f.await(f.image(nil))
			if res.OK || res.Meta.ErrClass != "gpu_queued" {
				t.Fatalf("want a queued answer behind the whole-node lease, got ok=%v class=%q %s", res.OK, res.Meta.ErrClass, res.Reason)
			}
			if got := f.started(); len(got) != 0 {
				t.Fatalf("the call ran beside a whole-node lease: %+v", got)
			}
		})
	}
}

// An explicit pin that falls outside the cards of the lease this process runs under is not the
// inherited path's to serve: the legacy path (and its own --cuda-device) does, exactly as before.
func TestAnInheritedLeaseWithAPinOutsideItsCardsKeepsTheLegacyPath(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: func(c *config.Config) { c.ComfyCudaDevice = "2" }}) // nvidia 2 = card C
	parent, err := f.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "gpu reserve --devices", TTL: time.Hour, Devices: []string{leaseIDOf(admitUUIDA)}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = parent.Release() }()
	t.Setenv("GPU_LEASE_DIR", parent.Dir())
	t.Setenv("GPU_LEASE_EPOCH", fmt.Sprint(parent.Epoch()))
	t.Setenv("GPU_LEASE_CLASS", "media")
	t.Setenv("GPU_LEASE_DEVICES", leaseIDOf(admitUUIDA))
	ch := f.image(nil)
	probes := f.waitStarted(1)
	if env := probes[0].Env; env["COMFY_CARD_UUID"] != "" || env["COMFY_CUDA_DEVICE"] != "2" {
		t.Errorf("env = %v, want the legacy pin and no per-card binding: the pinned card is not the parent's", env)
	}
	f.letRunnersGo()
	f.await(ch)
}

// ---------------------------------------------------------------------------------------------
// A call that needs the whole node waits in line too
// ---------------------------------------------------------------------------------------------

func (f *admitFixture) graph(params map[string]any) <-chan core.Result {
	f.t.Helper()
	f.cfg.RunGraphScript = f.cfg.ImageGenScript
	f.p.cfg.RunGraphScript = f.cfg.ImageGenScript
	gp := filepath.Join(f.dir, "g.json")
	_ = os.WriteFile(gp, []byte("{}"), 0o644)
	p := map[string]any{"graph_path": gp, "out_dir": f.dir}
	for k, v := range params {
		p[k] = v
	}
	return f.start(core.TaskRunGraph, "", p)
}

// On a host that leases cards, a call that holds the whole node (run-graph with no devices, a pool
// on a big box, a pin it cannot resolve) that waited its window and still has no node answers with
// a place in line, exactly like a single-card call: the token replaces the busy refusal. A place
// for the whole node is a barrier, so a later call queues behind it.
func TestAWholeNodeCallOnAScopedHostAnswersQueued(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
	holder, err := f.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "another job", TTL: time.Hour, Devices: []string{leaseIDOf(admitUUIDA)}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Release() }()

	res := f.await(f.graph(nil))
	if res.OK || res.Meta.ErrClass != "gpu_queued" || res.DeferClass != core.DeferClassCapacity {
		t.Fatalf("want a queued capacity defer, got ok=%v class=%q/%q: %s", res.OK, res.Meta.ErrClass, res.DeferClass, res.Reason)
	}
	tok := tokenOf(t, res)
	tk, ok := f.m.ResumeToken(tok)
	if !ok || len(tk.Devices) != 0 {
		t.Fatalf("token %s = %+v ok=%v, want a place for the WHOLE node (no cards)", tok, tk, ok)
	}
	if got := f.started(); len(got) != 0 {
		t.Fatalf("the call ran beside a card lease: %+v", got)
	}

	// The holder lets go; the same call with its token takes the node, and the token is spent.
	_ = holder.Release()
	f.letRunnersGo()
	again := f.await(f.graph(map[string]any{"waiter_token": tok}))
	if !again.OK {
		t.Fatalf("resumed: %+v", again)
	}
	if _, ok := f.m.ResumeToken(tok); ok {
		t.Error("a served place is dropped")
	}
}

// A host that cannot lease cards has no token to leave: its busy answer is the one it always gave.
func TestAHostWithoutCardScopedLeasesStillAnswersBusy(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, noAudit: true})
	holder, err := f.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "whole-node job", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Release() }()
	res := f.await(f.image(nil))
	if res.OK || res.Meta.ErrClass != "gpu_busy" {
		t.Fatalf("want the legacy busy answer, got ok=%v class=%q %s", res.OK, res.Meta.ErrClass, res.Reason)
	}
	if res.Data != nil {
		t.Errorf("a busy answer carries no data: %s", res.Data)
	}
	if n := len(f.m.Tokens()); n != 0 {
		t.Errorf("%d token(s): a host that cannot lease cards keeps no places", n)
	}
}

// A whole-node caller that comes back and is still not served keeps ONE token and its original
// arrival time, like a single-card caller.
func TestAWholeNodeCallKeepsItsPlaceAcrossRecalls(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
	holder, err := f.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "another job", TTL: time.Hour, Devices: []string{leaseIDOf(admitUUIDA)}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Release() }()
	first := f.await(f.graph(nil))
	tok := tokenOf(t, first)
	second := f.await(f.graph(map[string]any{"waiter_token": tok}))
	if tokenOf(t, second) != tok {
		t.Fatalf("the second answer carries %q, want the same token %q", tokenOf(t, second), tok)
	}
	if n := len(f.m.Tokens()); n != 1 {
		t.Fatalf("%d tokens on disk, want one place in line", n)
	}
	tk, _ := f.m.ResumeToken(tok)
	if time.Since(tk.Since()) < 500*time.Millisecond {
		t.Errorf("the place must keep its ORIGINAL arrival time, got %v ago", time.Since(tk.Since()))
	}
}

// With the node free but a place held ahead of it, a whole-node call never reaches the front of the
// line: its wait ends in a place of its own, not in a failure.
func TestAWholeNodeCallBehindALiveTokenAnswersQueued(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
	f.leaveToken([]string{leaseIDOf(admitUUIDA)}, time.Now().Add(-time.Minute))
	res := f.await(f.graph(nil))
	if res.OK || res.Meta.ErrClass != "gpu_queued" || res.DeferClass != core.DeferClassCapacity {
		t.Fatalf("want a queued capacity defer, got ok=%v class=%q/%q: %s", res.OK, res.Meta.ErrClass, res.DeferClass, res.Reason)
	}
	if !strings.Contains(string(res.Data), `"queue_position":2`) {
		t.Errorf("payload = %s, want position 2 behind the place held for card A", res.Data)
	}
}

// A call with no wait window (gpu_wait_ms 0) that finds the node free is served at once; the place
// it resumed is spent even though no queue was ever joined.
func TestAServedWholeNodeCallSpendsItsPlaceEvenWithNoWait(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: func(c *config.Config) { c.GPUWaitMs = 0 }})
	tok := f.leaveToken(nil, time.Now().Add(-time.Minute))
	f.letRunnersGo()
	if r := f.await(f.graph(map[string]any{"waiter_token": tok.ID})); !r.OK {
		t.Fatalf("%+v", r)
	}
	if _, ok := f.m.ResumeToken(tok.ID); ok {
		t.Error("a served place is dropped, whether or not the call queued")
	}
}
