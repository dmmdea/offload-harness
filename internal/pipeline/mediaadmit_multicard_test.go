package pipeline

// The card-only placement at the typed doors (imagegen_placement / gen_edit_placement: multicard; render/multicard-recipes.json). A binding that asks for
// it takes a new need kind: the box's TWO non-display cards, leased as a set, queued like any other lease and never degraded to a one-card run. The
// runner gets --placement multicard and the lease's two cards (GPU_LEASE_DEVICES); it takes no per-card instance (COMFY_CARD_UUID, COMFY_API), because
// the pair has an instance of its own that the runner keys itself, and none of the single-card launch settings that contradict two cards under
// --gpu-only. The declaration is sized on the pair, and a binding without the key is the lease, the argv and the declaration it always was.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// multicardBinding binds the image route to the 2512 Q5_1 GGUF on the card-only placement, with the settings the placement ignores set on purpose.
func multicardBinding(c *config.Config) {
	c.ImageGenFamily, c.ImageGenCkpt, c.ImageGenPlacement = "qwen-image", "qwen-image-2512-Q5_1.gguf", "multicard"
	c.ComfyCudaDevice, c.ComfyDynamicVRAM = "2", "on"
	c.ImageGenPoolVvramGB, c.ImageGenPoolCompute, c.ImageGenPoolDonor = 12, "cuda:0", "cuda:1" // a pool that names the display card and one of the pair: the placement ignores it
}

// multicardEditBinding binds the edit route to the 2511 fp8mixed DiT on the card-only placement (the measured row).
func multicardEditBinding(c *config.Config) {
	c.GenEditScript, c.GenEditUnet, c.GenEditPlacement = c.ImageGenScript, "qwen_image_edit_2511_fp8mixed.safetensors", "multicard"
}

// singleEditBinding binds the edit route as it always was: one card.
func singleEditBinding(c *config.Config) {
	c.GenEditScript, c.GenEditUnet = c.ImageGenScript, "qwen_image_edit_2511_fp8mixed.safetensors"
}

func both(a, b func(*config.Config)) func(*config.Config) {
	return func(c *config.Config) { a(c); b(c) }
}

// edit starts one generative edit call in the background.
func (f *admitFixture) edit(extra map[string]any) <-chan core.Result {
	f.t.Helper()
	params := map[string]any{"image": filepath.Join(f.dir, "in.png"), "out": filepath.Join(f.dir, fmt.Sprintf("edit-%d.png", time.Now().UnixNano()))}
	for k, v := range extra {
		params[k] = v
	}
	return f.start(core.TaskEditImageGenerative, "make the light orange", params)
}

// pairIDs are the two non-display cards of the fixture, in lease-id order.
func pairIDs() []string { return []string{leaseIDOf(admitUUIDA), leaseIDOf(admitUUIDC)} }

func pairEnv() string { return strings.Join(pairIDs(), ",") }

func TestAMulticardImageBindingLeasesTwoNonDisplayCardsAndPassesThePlacement(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: multicardBinding})
	ch := f.image(nil)
	probes := f.waitStarted(1)
	leases := f.m.Leases()
	if len(leases) != 1 || strings.Join(leases[0].Devices, ",") != pairEnv() {
		t.Fatalf("leases = %+v, want ONE lease holding both non-display cards %v", leases, pairIDs())
	}
	env := probes[0].Env
	if env["GPU_LEASE_DEVICES"] != pairEnv() {
		t.Errorf("GPU_LEASE_DEVICES = %q, want both cards", env["GPU_LEASE_DEVICES"])
	}
	if strings.Contains(env["GPU_LEASE_DEVICES"], leaseIDOf(admitUUIDB)) {
		t.Errorf("the display card is in the set: %v", env)
	}
	for _, k := range []string{"COMFY_CARD_UUID", "COMFY_API", "COMFY_CUDA_DEVICE", "COMFY_DYNAMIC_VRAM"} {
		if env[k] != "" {
			t.Errorf("%s = %q: the pair has an instance of its own that the runner keys, and a single-card pin or dynamic VRAM contradicts two cards under --gpu-only", k, env[k])
		}
	}
	argv := strings.Join(probes[0].Argv, " ")
	if !strings.Contains(argv, "--placement multicard") || !strings.Contains(argv, "--family qwen-image") {
		t.Errorf("argv = %q, want --placement multicard", argv)
	}
	for _, flag := range []string{"--cards", "--resident-gb", "--pool-vvram", "--pool-compute", "--pool-donor"} {
		if strings.Contains(argv, flag) {
			t.Errorf("argv = %q: %s is not the door's to pass (the lease names the cards, the recipe the shares, the placement is not a pool)", argv, flag)
		}
	}
	if env["GPU_LEASE_CLASS"] != "media" || env["GPU_LEASE_EPOCH"] == "" || env["GPU_LEASE_UNLOAD_MODELS"] == "" {
		t.Errorf("lease env incomplete: %v", env)
	}
	f.letRunnersGo()
	f.await(ch)
	if left := f.m.Leases(); len(left) != 0 {
		t.Errorf("leases left: %+v", left)
	}
}

// The display card is never part of the set, with the operator at the desk or away: the allocator's rule lets an away operator's screen card be
// auto-assigned to a ONE-card call, and the placement is not that.
func TestTheMulticardPairNeverIncludesTheDisplayCardWhetherTheOperatorIsAwayOrNot(t *testing.T) {
	for _, away := range []bool{false, true} {
		f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: multicardBinding})
		f.away = away
		ch := f.image(nil)
		probes := f.waitStarted(1)
		if got := probes[0].Env["GPU_LEASE_DEVICES"]; got != pairEnv() {
			t.Errorf("away=%v: GPU_LEASE_DEVICES = %q, want the two non-display cards", away, got)
		}
		f.letRunnersGo()
		f.await(ch)
	}
}

func TestAMulticardImageBindingWithOneFreeNonDisplayCardQueuesInsteadOfRunningOnOne(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: multicardBinding})
	holder := hold(t, f, "a bench render", leaseIDOf(admitUUIDC))

	res := f.await(f.image(nil))
	if res.OK || res.Meta.ErrClass != "gpu_queued" || res.DeferClass != core.DeferClassCapacity {
		t.Fatalf("one of the two cards is held: the call keeps its place in line, got ok=%v class=%q/%q: %s", res.OK, res.Meta.ErrClass, res.DeferClass, res.Reason)
	}
	if got := f.started(); len(got) != 0 {
		t.Fatalf("a runner started on %v although only one of the pair is free", got)
	}
	for _, id := range pairIDs() {
		if !strings.Contains(string(res.Data), id) {
			t.Errorf("the place in line is for the pair: %s lacks %s", res.Data, id)
		}
	}
	token := resultTokenOf(t, res)

	_ = holder.Release()
	f.letRunnersGo()
	again := f.await(f.image(map[string]any{"waiter_token": token}))
	if again.Meta.ErrClass == "gpu_queued" || again.Meta.ErrClass == "gpu_busy" {
		t.Fatalf("resumed when both were free: class=%q %s", again.Meta.ErrClass, again.Reason)
	}
	if probes := f.started(); len(probes) != 1 || probes[0].Env["GPU_LEASE_DEVICES"] != pairEnv() {
		t.Fatalf("probes = %+v, want one run on both cards", probes)
	}
}

func TestAMulticardImageBindingOnABoxWithoutExactlyTwoNonDisplayCardsAnswersWithTheReason(t *testing.T) {
	cases := map[string]struct {
		spec admitSpec
		fix  func(*admitFixture)
		want string
	}{
		"one non-display card":    {spec: admitSpec{order: admitOrder, mutate: multicardBinding}, fix: func(f *admitFixture) { f.cards[2].Display = true }, want: "has 1"},
		"three non-display cards": {spec: admitSpec{order: "1,0,2,3", fourCards: true, mutate: multicardBinding}, want: "has 3"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newAdmitFixtureWith(t, tc.spec)
			if tc.fix != nil {
				tc.fix(f)
			}
			res := f.await(f.image(nil))
			if res.OK || res.Meta.ErrClass != core.ErrClassGPULeaseUnavailable {
				t.Fatalf("want a refusal that ran nothing, got ok=%v class=%q: %s", res.OK, res.Meta.ErrClass, res.Reason)
			}
			for _, want := range []string{"exactly two non-display cards", tc.want, "imagegen_placement", "gpu reserve --devices"} {
				if !strings.Contains(res.Reason, want) {
					t.Errorf("the reason must say %q: %s", want, res.Reason)
				}
			}
			if got := f.started(); len(got) != 0 {
				t.Fatalf("a runner started: %v", got)
			}
			if n := len(f.m.Leases()); n != 0 {
				t.Errorf("%d lease(s) taken for a call that cannot run", n)
			}
			if toks := f.m.Tokens(); len(toks) != 0 {
				t.Errorf("a place in line for a call that can never be served: %+v", toks)
			}
		})
	}
}

// A card table that cannot be read, or a host that does not lease cards, never turns a multicard call into a whole-node run that names no cards: the
// runner would refuse that, every time, after the whole node had been held for nothing.
func TestAMulticardCallIsNeverDegradedToTheWholeNode(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, noAudit: true, mutate: multicardBinding})
	res := f.await(f.image(nil))
	if res.OK || res.Meta.ErrClass != core.ErrClassGPULeaseUnavailable || !strings.Contains(res.Reason, "gpu_card_scoped_leases") || !strings.Contains(res.Reason, "multicard") {
		t.Fatalf("a host that does not lease cards: ok=%v class=%q %q", res.OK, res.Meta.ErrClass, res.Reason)
	}
	if got := f.started(); len(got) != 0 {
		t.Fatalf("a runner started: %v", got)
	}
	if n := len(f.m.Leases()); n != 0 {
		t.Errorf("%d lease(s) held for a call that cannot run", n)
	}

	g := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: multicardBinding})
	g.p.alloc.Cards = func(context.Context, config.Config) ([]gpuprobe.Card, string, error) {
		return nil, "", fmt.Errorf("the driver did not answer")
	}
	res = g.await(g.image(nil))
	if res.OK || res.Meta.ErrClass != core.ErrClassGPULeaseUnavailable || !strings.Contains(res.Reason, "card table") || !strings.Contains(res.Reason, "the driver did not answer") {
		t.Fatalf("an unreadable card table: ok=%v class=%q %q", res.OK, res.Meta.ErrClass, res.Reason)
	}
	if got := g.started(); len(got) != 0 {
		t.Fatalf("a runner started: %v", got)
	}
	if n := len(g.m.Leases()); n != 0 {
		t.Errorf("%d lease(s) held for a call that cannot run", n)
	}
}

// A binding the recipe table has no row for is refused before any lease is spent, naming the key, because the runner would refuse it by name
// after the pair had been held and the host's memory declared: krea2 (it keeps --pool-*), 2512 bf16 (38 GiB fits no card), sdxl.
func TestAMulticardBindingWithNoRecipeIsRefusedBeforeAnyLease(t *testing.T) {
	for name, bind := range map[string]func(*config.Config){
		"krea2": func(c *config.Config) {
			krea2Binding(c)
			c.ImageGenPlacement = "multicard"
		},
		"2512 bf16": func(c *config.Config) {
			c.ImageGenFamily, c.ImageGenCkpt, c.ImageGenPlacement = "qwen-image", "qwen_image_2512_bf16.safetensors", "multicard"
		},
		"sdxl": func(c *config.Config) { c.ImageGenPlacement = "multicard" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: bind})
			res := f.await(f.image(nil))
			if res.OK || res.Meta.ErrClass != core.ErrClassGPULeaseUnavailable || !strings.Contains(res.Reason, "no card-only recipe") || !strings.Contains(res.Reason, "imagegen_placement") {
				t.Fatalf("want a refusal naming the missing recipe and the key, got ok=%v class=%q: %s", res.OK, res.Meta.ErrClass, res.Reason)
			}
			if n := len(f.m.Leases()) + len(f.started()) + len(f.m.Tokens()); n != 0 {
				t.Errorf("%d lease/runner/place for a call the runner would refuse", n)
			}
		})
	}
}

func TestAnInheritedLeaseOnTwoCardsServesAMulticardImageCallOnBothCards(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: multicardBinding})
	parent := f.underParent(admitUUIDA, admitUUIDC)

	a := f.image(nil)
	probes := f.waitStarted(1)
	env := probes[0].Env
	if env["GPU_LEASE_DEVICES"] != pairEnv() || env["GPU_LEASE_EPOCH"] != fmt.Sprint(parent.Epoch()) {
		t.Errorf("the call runs under the parent's lease on both its cards: %v", env)
	}
	if env["COMFY_CARD_UUID"] != "" || env["COMFY_API"] != "" {
		t.Errorf("the pair has no per-card instance: %v", env)
	}
	if !strings.Contains(strings.Join(probes[0].Argv, " "), "--placement multicard") {
		t.Errorf("argv = %v", probes[0].Argv)
	}
	if n := len(f.m.Leases()); n != 1 {
		t.Errorf("%d live leases: a call under an inherited lease takes none of its own", n)
	}
	// one at a time: a second call has no free pair in this process and answers busy
	res := f.await(f.image(nil))
	if res.OK || res.Meta.ErrClass != "gpu_busy" {
		t.Errorf("second call: ok=%v class=%q %s, want the busy answer", res.OK, res.Meta.ErrClass, res.Reason)
	}
	f.letRunnersGo()
	f.await(a)
	if r := f.await(f.image(nil)); r.Meta.ErrClass == "gpu_busy" {
		t.Fatalf("the slots of the finished call must be released: %+v", r)
	}
}

// A parent lease that does not hold exactly two non-display cards cannot serve a multicard call: the call says so, and does not run on one card or on
// the whole node.
func TestAnInheritedLeaseThatIsNotTwoNonDisplayCardsRefusesAMulticardCallWithTheReason(t *testing.T) {
	for name, held := range map[string][]string{
		"one card":                      {admitUUIDA},
		"whole node":                    nil,
		"the display card and one more": {admitUUIDA, admitUUIDB},
	} {
		t.Run(name, func(t *testing.T) {
			f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: multicardBinding})
			f.underParent(held...)
			res := f.await(f.image(nil))
			if res.OK || res.Meta.ErrClass != core.ErrClassGPULeaseUnavailable || !strings.Contains(res.Reason, "two non-display cards") || !strings.Contains(res.Reason, "gpu reserve --devices") {
				t.Fatalf("ok=%v class=%q %q, want the reason and the way out", res.OK, res.Meta.ErrClass, res.Reason)
			}
			if got := f.started(); len(got) != 0 {
				t.Fatalf("a runner started: %v", got)
			}
		})
	}
}

// A multicard call and a single-card call never run at once on the same card, and two multicard calls serialize.
func TestAMulticardCallAndASingleCardCallNeverShareACard(t *testing.T) {
	// the single-card call is the edit route; the image route is multicard
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: both(multicardBinding, singleEditBinding)})
	single := f.edit(nil)
	f.waitStarted(1)
	res := f.await(f.image(nil))
	if res.OK || res.Meta.ErrClass != "gpu_queued" {
		t.Fatalf("a multicard call beside a single-card call on one of its cards waits: ok=%v class=%q %s", res.OK, res.Meta.ErrClass, res.Reason)
	}
	if got := f.started(); len(got) != 1 {
		t.Fatalf("%d runners in flight, want only the single-card call's", len(got))
	}
	f.letRunnersGo()
	f.await(single)

	// and the other way: a multicard call in flight holds both non-display cards, so the single-card call has none to take
	g := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: both(multicardBinding, singleEditBinding)})
	first := g.image(nil)
	g.waitStarted(1)
	res = g.await(g.edit(nil))
	if res.OK || res.Meta.ErrClass != "gpu_queued" {
		t.Fatalf("a single-card call while the pair is held: ok=%v class=%q %s", res.OK, res.Meta.ErrClass, res.Reason)
	}
	// two multicard calls serialize
	second := g.await(g.image(nil))
	if second.OK || second.Meta.ErrClass != "gpu_queued" {
		t.Fatalf("a second multicard call waits for the first: ok=%v class=%q %s", second.OK, second.Meta.ErrClass, second.Reason)
	}
	if got := g.started(); len(got) != 1 {
		t.Fatalf("%d runners in flight, want one", len(got))
	}
	g.letRunnersGo()
	g.await(first)
}

// A named family whose overlay asks for the placement leases the pair; the default binding of the same node, which does not, still takes one card.
func TestANamedFamilyThatAsksForMulticardLeasesThePairAndTheDefaultStaysOnOneCard(t *testing.T) {
	overlay := func(c *config.Config) {
		c.ImageGenFamilies = map[string]config.FamilyOverlay{
			"qi21-pair": {
				"license": []byte(`"Qwen Research License"`), "commercial_use": []byte(`false`),
				"imagegen_family":    []byte(`"qwen-image-2.1"`),
				"imagegen_ckpt":      []byte(`"qwen_image_2.1_int8_convrot.safetensors"`),
				"imagegen_clip":      []byte(`"qwen3vl_8b_bf16.safetensors"`),
				"imagegen_vae":       []byte(`"qwen_image_2.1_vae_bf16.safetensors"`),
				"imagegen_placement": []byte(`"multicard"`),
			},
		}
	}
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: overlay})
	named := f.image(map[string]any{"family": "qi21-pair"})
	probes := f.waitStarted(1)
	if probes[0].Env["GPU_LEASE_DEVICES"] != pairEnv() || probes[0].Env["COMFY_CARD_UUID"] != "" || !strings.Contains(strings.Join(probes[0].Argv, " "), "--placement multicard") {
		t.Errorf("the named family runs on the pair: %+v", probes[0])
	}
	f.letRunnersGo()
	f.await(named)

	g := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: overlay})
	def := g.image(nil)
	probes = g.waitStarted(1)
	if probes[0].Env["COMFY_CARD_UUID"] == "" || strings.Contains(strings.Join(probes[0].Argv, " "), "--placement") || strings.Contains(probes[0].Env["GPU_LEASE_DEVICES"], ",") {
		t.Errorf("the default binding of the same node stays on one card: %+v", probes[0])
	}
	g.letRunnersGo()
	g.await(def)
}

// The batch takes the same lease and the same argv as the single call: one pair for the whole batch.
func TestAnImageBatchOnAMulticardBindingLeasesThePairOnce(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: multicardBinding})
	f.letRunnersGo()
	_, _ = f.p.RunImageBatchAs(context.Background(), "", []ImageBatchJob{{Prompt: "a calm ocean at dawn", Out: filepath.Join(f.dir, "b1.png")}, {Prompt: "a calm lake at dusk", Out: filepath.Join(f.dir, "b2.png")}})
	probes := f.started()
	if len(probes) != 1 {
		t.Fatalf("%d runners for one batch, want one", len(probes))
	}
	argv := strings.Join(probes[0].Argv, " ")
	if !strings.Contains(argv, "--batch") || !strings.Contains(argv, "--placement multicard") || probes[0].Env["GPU_LEASE_DEVICES"] != pairEnv() || probes[0].Env["COMFY_CARD_UUID"] != "" {
		t.Errorf("batch probe = %+v", probes[0])
	}
}

func TestTheEditDoorLeasesTwoNonDisplayCardsForAMulticardBinding(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: both(multicardEditBinding, func(c *config.Config) { c.ComfyCudaDevice = "2"; c.ComfyDynamicVRAM = "on" })})
	ch := f.edit(nil)
	probes := f.waitStarted(1)
	leases := f.m.Leases()
	if len(leases) != 1 || strings.Join(leases[0].Devices, ",") != pairEnv() {
		t.Fatalf("leases = %+v, want ONE lease on both non-display cards", leases)
	}
	env := probes[0].Env
	if env["GPU_LEASE_DEVICES"] != pairEnv() {
		t.Errorf("GPU_LEASE_DEVICES = %q", env["GPU_LEASE_DEVICES"])
	}
	for _, k := range []string{"COMFY_CARD_UUID", "COMFY_API", "COMFY_CUDA_DEVICE", "COMFY_DYNAMIC_VRAM"} {
		if env[k] != "" {
			t.Errorf("%s = %q on a pair", k, env[k])
		}
	}
	argv := strings.Join(probes[0].Argv, " ")
	if !strings.Contains(argv, "--placement multicard") || !strings.Contains(argv, "--unet qwen_image_edit_2511_fp8mixed.safetensors") {
		t.Errorf("argv = %q", argv)
	}
	// the measured fp8mixed row on two 16 GiB cards declares no host RAM
	if got := leases[0].HostRAMGiB; got != 0 {
		t.Errorf("the lease declares %.1f GiB of host RAM, want 0 (the measured row fits the pair)", got)
	}
	f.letRunnersGo()
	f.await(ch)
}

func TestAnEditBindingWithoutThePlacementIsTheSingleCardLeaseItAlwaysWas(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: singleEditBinding})
	ch := f.edit(nil)
	probes := f.waitStarted(1)
	env := probes[0].Env
	if len(f.m.Leases()) != 1 || len(f.m.Leases()[0].Devices) != 1 || env["COMFY_CARD_UUID"] == "" || env["COMFY_API"] == "" {
		t.Errorf("a plain edit holds one card in its own instance: leases %+v env %v", f.m.Leases(), env)
	}
	if strings.Contains(strings.Join(probes[0].Argv, " "), "--placement") {
		t.Errorf("argv = %v", probes[0].Argv)
	}
	if got := f.m.Leases()[0].HostRAMGiB; got <= 0 {
		t.Errorf("the plain fp8mixed edit declares its set on one 16 GiB card: %.1f", got)
	}
	f.letRunnersGo()
	f.await(ch)
}

// An edit binding with no recipe (the 2.1 edit family has none) is refused before a lease.
func TestAnEditBindingWithNoRecipeIsRefusedBeforeAnyLease(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: both(multicardEditBinding, func(c *config.Config) {
		c.GenEditFamily, c.GenEditUnet = config.FamilyQwenImage21, "qwen_image_2.1_bf16.safetensors"
	})})
	res := f.await(f.edit(nil))
	if res.OK || res.Meta.ErrClass != core.ErrClassGPULeaseUnavailable || !strings.Contains(res.Reason, "no card-only recipe") || !strings.Contains(res.Reason, "gen_edit_placement") {
		t.Fatalf("ok=%v class=%q %q", res.OK, res.Meta.ErrClass, res.Reason)
	}
	if n := len(f.m.Leases()) + len(f.started()) + len(f.m.Tokens()); n != 0 {
		t.Errorf("%d lease/runner/place", n)
	}
}

func TestAMulticardNeedDeclaresRAMFromThePairNotTheLargestCard(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: both(multicardEditBinding, multicardBinding)})
	ctx := context.Background()
	cards := f.cards

	edit := editNeed(f.cfg, "")
	if edit.Kind != needMulticard {
		t.Fatalf("an edit binding with the placement takes the multicard need kind, got %v", edit.Kind)
	}
	if got := f.p.declaredRAM(ctx, edit, cards, pairIDs()); got != 0 {
		t.Errorf("the pair: %.1f GiB, want 0 (the measured fp8mixed row fits two 16 GiB cards)", got)
	}
	// the same need sized on ONE card (a plan that does not name its pair) is the ordinary figure: the recipe runs on exactly two
	if got := f.p.declaredRAM(ctx, edit, cards, pairIDs()[:1]); got <= 0 {
		t.Errorf("one card: %.1f GiB, want the ordinary streaming figure", got)
	}
	if got := f.p.declaredRAM(ctx, edit, cards, nil); got <= 0 {
		t.Errorf("the whole table (three cards): %.1f GiB, want the ordinary streaming figure", got)
	}

	// a PREDICTED row declares the ordinary estimate until a run has measured it: an early merge costs a wasteful wait, never a false 0
	img := imageNeed(f.cfg, "")
	if img.Kind != needMulticard {
		t.Fatalf("the image binding: kind %v", img.Kind)
	}
	ordinary := f.cfg
	ordinary.ImageGenPlacement = ""
	want := f.p.declaredRAM(ctx, imageNeed(ordinary, ""), cards, pairIDs())
	if want <= 0 {
		t.Fatalf("premise: the ordinary 2512 GGUF estimate on a 16 GiB card is %.1f GiB, want something to compare with", want)
	}
	if got := f.p.declaredRAM(ctx, img, cards, pairIDs()); got != want {
		t.Errorf("the predicted 2512 GGUF row: %.1f GiB, want the ordinary estimate %.1f GiB", got, want)
	}

	// a binding without the key is the need it always was
	plain := f.cfg
	plain.ImageGenPlacement, plain.GenEditPlacement = "", ""
	if k := imageNeed(plain, "").Kind; k != needPooled {
		t.Errorf("no placement on a seat that names a pool: image kind %v, want the pooled need it always was", k)
	}
	plain.ImageGenPoolVvramGB, plain.ImageGenPoolCompute, plain.ImageGenPoolDonor = 0, "", ""
	if k := imageNeed(plain, "").Kind; k != needSingle {
		t.Errorf("no placement: image kind %v, want the single-card need", k)
	}
	if k := editNeed(plain, "").Kind; k != needSingle {
		t.Errorf("no placement: edit kind %v, want the single-card need", k)
	}
	plain.ImageGenPlacement, plain.GenEditPlacement = "single", "single"
	if k := imageNeed(plain, "").Kind; k != needSingle {
		t.Errorf("placement single: image kind %v, want the single-card need", k)
	}
}

// planMedia: the pair is the box's two non-display cards, named, so the lane probe and the grant read one plan.
func TestPlanMediaNamesTheTwoNonDisplayCardsForAMulticardNeed(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: multicardBinding})
	plan, err := planMedia(imageNeed(f.cfg, ""), f.cards, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.whole || plan.auto || plan.instance || strings.Join(plan.ids, ",") != pairEnv() {
		t.Fatalf("plan = %+v, want the named pair %v and no per-card instance", plan, pairIDs())
	}
	if _, err := planMedia(imageNeed(f.cfg, ""), nil, fmt.Errorf("boom")); err == nil || !strings.Contains(err.Error(), "card table") {
		t.Errorf("an unreadable table must not become a whole-node plan: %v", err)
	}
	if _, err := planMedia(imageNeed(f.cfg, ""), nil, nil); err == nil {
		t.Errorf("an empty table must refuse")
	}
	unsure := append([]gpuprobe.Card(nil), f.cards...)
	unsure[2].DisplayUnknown = true
	if _, err := planMedia(imageNeed(f.cfg, ""), unsure, nil); err == nil || !strings.Contains(err.Error(), "exactly two non-display cards") {
		t.Errorf("a card that may be the screen is not a non-display card: %v", err)
	}
}

// The model the door builds from a multicard binding carries the placement and drops what contradicts it.
func TestTheImageModelOfAMulticardBindingDropsThePoolAndTheSingleCardLaunch(t *testing.T) {
	cfg := config.Default()
	multicardBinding(&cfg)
	cfg.ComfyExtraArgs = "--verbose"
	m := imageModelFromConfig(cfg)
	if m.Placement != "multicard" || m.PoolVvramGB != 0 || m.PoolCompute != "" || m.PoolDonor != "" {
		t.Errorf("model = %+v, want the placement and no pool", m)
	}
	if m.Launch.CudaDevice != "" || m.Launch.DynamicVRAM != "" || m.Launch.ExtraArgs != "--verbose" {
		t.Errorf("launch = %+v, want no pin and no dynamic VRAM, and the operator's extra args kept", m.Launch)
	}
	// and the pooled seat without the placement is the model it always was
	cfg.ImageGenPlacement = ""
	if m := imageModelFromConfig(cfg); m.Placement != "" || m.PoolVvramGB != 12 || m.PoolCompute != "cuda:0" {
		t.Errorf("model = %+v, want the pooled seat unchanged", m)
	}
}

// The lane probe: not free while either non-display card is held, free when only the display card is; the reasons name the pair.
func TestMediaLaneFreeJudgesAMulticardImageCall(t *testing.T) {
	A, B, C := leaseIDOf(admitUUIDA), leaseIDOf(admitUUIDB), leaseIDOf(admitUUIDC)
	for name, tc := range map[string]struct {
		held []string
		free bool
	}{
		"idle":                       {nil, true},
		"card A held":                {[]string{A}, false},
		"card C held":                {[]string{C}, false},
		"both held":                  {[]string{A, C}, false},
		"only the display card held": {[]string{B}, true},
	} {
		t.Run(name, func(t *testing.T) {
			f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: multicardBinding})
			for _, id := range tc.held {
				hold(t, f, "a bench render", id)
			}
			v := f.p.MediaLaneFree(context.Background(), laneReq(map[string]any{}))
			if v.Free != tc.free {
				t.Fatalf("free = %v (%s), want %v", v.Free, v.Why, tc.free)
			}
			if !tc.free && (!strings.Contains(v.Why, A) || !strings.Contains(v.Why, C)) {
				t.Errorf("the reasons must name the pair: %s", v.Why)
			}
		})
	}
}

// A binding with no recipe is not judged free by omission: the probe says it did not judge, and why.
func TestMediaLaneFreeSaysWhenAMulticardBindingHasNoRecipe(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: func(c *config.Config) { krea2Binding(c); c.ImageGenPlacement = "multicard" }})
	v := f.p.MediaLaneFree(context.Background(), laneReq(map[string]any{}))
	if !strings.Contains(v.Why, "not judged") || !strings.Contains(v.Why, "no card-only recipe") {
		t.Errorf("verdict = %+v, want it said that the binding was not judged and why", v)
	}
}

// allLaneCases is the table of lane states with the card-only placement's states in it, so the differential test and the lease-root snapshot test
// both cover it.
func allLaneCases() []laneCase { return append(laneCases(), multicardLaneCases()...) }

// The differential cases: a multicard lane the real wait-0 admission would grant is never called busy.
func multicardLaneCases() []laneCase {
	mc := admitSpec{order: admitOrder, mutate: multicardBinding}
	A, B, C := leaseIDOf(admitUUIDA), leaseIDOf(admitUUIDB), leaseIDOf(admitUUIDC)
	return []laneCase{
		{name: "multicard, idle", spec: mc, grants: true},
		{name: "multicard, card A held", spec: mc, grants: false, mustSeeBusy: true,
			setup: func(t *testing.T, f *admitFixture) { hold(t, f, "r1", A) }},
		{name: "multicard, card C held", spec: mc, grants: false, mustSeeBusy: true,
			setup: func(t *testing.T, f *admitFixture) { hold(t, f, "r1", C) }},
		{name: "multicard, only the display card held", spec: mc, grants: true,
			setup: func(t *testing.T, f *admitFixture) { hold(t, f, "r1", B) }},
		{name: "multicard, a whole-node lease held", spec: mc, grants: false, mustSeeBusy: true,
			setup: func(t *testing.T, f *admitFixture) {
				l, err := f.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "a whole-node render", TTL: time.Hour})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = l.Release() })
			}},
		{name: "multicard, a live place in line on card C", spec: mc, grants: false, mustSeeBusy: true,
			setup: func(t *testing.T, f *admitFixture) {
				if _, err := f.m.LeaveToken(gpulease.ClassMedia, gpulease.Options{Reason: "image-gen", Devices: []string{C}}, time.Now()); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "multicard, a live place in line on the display card only", spec: mc, grants: true,
			setup: func(t *testing.T, f *admitFixture) {
				if _, err := f.m.LeaveToken(gpulease.ClassMedia, gpulease.Options{Reason: "image-gen", Devices: []string{B}}, time.Now()); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "multicard, an in-process slot held on card A", spec: mc, grants: false, mustSeeBusy: true,
			setup: func(t *testing.T, f *admitFixture) {
				if !mediaSlots.tryTake([]string{A}) {
					t.Fatal("slot busy before the test")
				}
				t.Cleanup(func() { mediaSlots.release([]string{A}) })
			}},
		{name: "multicard, a short host and a predicted row that declares the ordinary set", spec: mc, grants: false, mustSeeBusy: true,
			setup: func(t *testing.T, f *admitFixture) { f.useHost(t, func() gpuprobe.HostMemory { return shortHost(95) }) }},
		{name: "multicard on a box with one non-display card (the real admission refuses; the probe may not judge)", spec: mc, grants: false,
			setup: func(t *testing.T, f *admitFixture) { f.cards[2].Display = true }},
		{name: "multicard with no recipe (the real admission refuses; the probe says it did not judge)", grants: false,
			spec: admitSpec{order: admitOrder, mutate: func(c *config.Config) { krea2Binding(c); c.ImageGenPlacement = "multicard" }}},
	}
}
