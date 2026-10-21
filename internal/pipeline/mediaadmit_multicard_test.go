package pipeline

// The card-only placement at the typed doors (imagegen_placement / gen_edit_placement: multicard; render/multicard-recipes.json). A binding that asks for
// it takes a new need kind: the box's TWO non-display cards, leased as a set, queued like any other lease and never degraded to a one-card run. The
// runner gets --placement multicard and the lease's two cards (GPU_LEASE_DEVICES); it takes no per-card instance (COMFY_CARD_UUID, COMFY_API), because
// the pair has an instance of its own that the runner keys itself, and none of the single-card launch settings that contradict two cards under
// --gpu-only. The declaration is sized on the pair, and a binding without the key is the lease, the argv and the declaration it always was.

import (
	"context"
	"encoding/json"
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

// answerDevices reads the cards a success answer names (its "devices" key, the one the queued answer uses) and whether the key is there at all.
func answerDevices(t *testing.T, data []byte) (ids []string, present bool) {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("the answer is not a JSON object: %v: %s", err, data)
	}
	raw, ok := m["devices"]
	if !ok {
		return nil, false
	}
	if err := json.Unmarshal(raw, &ids); err != nil {
		t.Fatalf("devices is not a list of card ids: %v: %s", err, raw)
	}
	return ids, true
}

// wantAnswerNamesThePair fails unless a finished call's answer says which two cards it ran on: the lease's own ids, the ones the queued answer names.
func wantAnswerNamesThePair(t *testing.T, what string, res core.Result) {
	t.Helper()
	if !res.OK {
		t.Fatalf("%s: the call must succeed for its answer to be read: class=%q %s", what, res.Meta.ErrClass, res.Reason)
	}
	ids, present := answerDevices(t, res.Data)
	if !present || strings.Join(ids, ",") != pairEnv() {
		t.Errorf("%s: the success answer must name the two cards it ran on (devices = %v, want %v): %s", what, ids, pairIDs(), res.Data)
	}
}

// wantAnswerNamesNoCards fails unless a finished call's answer carries no "devices" key: a call that did not hold a pair answers as it always did.
func wantAnswerNamesNoCards(t *testing.T, what string, res core.Result) {
	t.Helper()
	if !res.OK {
		t.Fatalf("%s: the call must succeed for its answer to be read: class=%q %s", what, res.Meta.ErrClass, res.Reason)
	}
	if ids, present := answerDevices(t, res.Data); present {
		t.Errorf("%s: a call that held no card-only pair names no cards, got devices = %v: %s", what, ids, res.Data)
	}
}

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
	// the answer names the cards the call ran on (the queued answer already does)
	wantAnswerNamesThePair(t, "generate-image", f.await(ch))
	if left := f.m.Leases(); len(left) != 0 {
		t.Errorf("leases left: %+v", left)
	}
}

// The display card is never part of the set, whatever the display cap: the allocator may give the screen's card to a ONE-card call up to
// gpu_display_cap_pct (A-RAM-15), and the placement is not that: the pair is named, not allocated.
func TestTheMulticardPairNeverIncludesTheDisplayCardWhateverTheDisplayCap(t *testing.T) {
	zero, whole := 0, 100
	for _, capPct := range []*int{nil, &zero, &whole} {
		name := "default"
		if capPct != nil {
			name = fmt.Sprint(*capPct)
		}
		f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: func(c *config.Config) { multicardBinding(c); c.GPUDisplayCapPct = capPct }})
		ch := f.image(nil)
		probes := f.waitStarted(1)
		if got := probes[0].Env["GPU_LEASE_DEVICES"]; got != pairEnv() {
			t.Errorf("cap %s: GPU_LEASE_DEVICES = %q, want the two non-display cards", name, got)
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
// after the pair had been held and the host's memory declared: krea2 fp8 (only the bf16 DiT has a row), 2512 bf16 (38 GiB fits no card), sdxl. No row is held back
// today; a row that was would be refused the same way, with its reason (mediamulticard.go names it).
func TestAMulticardBindingWithNoRecipeIsRefusedBeforeAnyLease(t *testing.T) {
	for _, tc := range []struct {
		name string
		bind func(*config.Config)
	}{
		{name: "krea2 fp8", bind: func(c *config.Config) {
			c.ImageGenFamily, c.ImageGenCkpt, c.ImageGenPlacement = "krea2", "krea2_turbo_fp8.safetensors", "multicard"
		}},
		{name: "2512 bf16", bind: func(c *config.Config) {
			c.ImageGenFamily, c.ImageGenCkpt, c.ImageGenPlacement = "qwen-image", "qwen_image_2512_bf16.safetensors", "multicard"
		}},
		{name: "sdxl", bind: func(c *config.Config) { c.ImageGenPlacement = "multicard" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: tc.bind})
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

// The edit recipes' peaks are scaled to a canvas of at most 2.0 MP. gen_edit_megapixels is honoured verbatim by the runner, so a binding that pins a larger
// canvas has no recipe's figures: the runner refuses it after the pair was held, so the door refuses it before any lease and says which number.
func TestAMulticardEditBindingThatPinsACanvasAboveTheRecipesCapIsRefusedBeforeAnyLease(t *testing.T) {
	for _, tc := range []struct {
		mp    float64
		fails bool
	}{{4, true}, {2.01, true}, {2.0, false}, {0.9, false}, {0, false}} {
		f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: both(multicardEditBinding, func(c *config.Config) { c.GenEditMegapixels = tc.mp })})
		ch := f.edit(nil)
		if !tc.fails {
			f.waitStarted(1)
			if n := len(f.m.Leases()); n != 1 {
				t.Errorf("a %.2f MP canvas: %d lease(s), want the pair's one", tc.mp, n)
			}
			f.letRunnersGo()
			f.await(ch)
			continue
		}
		res := f.await(ch)
		if res.OK || res.Meta.ErrClass != core.ErrClassGPULeaseUnavailable || !strings.Contains(res.Reason, "gen_edit_megapixels") || !strings.Contains(res.Reason, "gen_edit_placement") || !strings.Contains(res.Reason, "2.0") {
			t.Errorf("a %.2f MP canvas: want a refusal naming gen_edit_megapixels, gen_edit_placement and the 2.0 MP cap, got ok=%v class=%q: %s", tc.mp, res.OK, res.Meta.ErrClass, res.Reason)
		}
		if n := len(f.m.Leases()) + len(f.started()) + len(f.m.Tokens()); n != 0 {
			t.Errorf("a %.2f MP canvas: %d lease/runner/place for a call the runner would refuse", tc.mp, n)
		}
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
	// under the parent's lease the answer still names the two cards the call ran on
	wantAnswerNamesThePair(t, "generate-image under a parent lease", f.await(a))
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
			"qi-pair": {
				"license": []byte(`"Apache-2.0"`), "commercial_use": []byte(`true`),
				"imagegen_family":    []byte(`"qwen-image"`),
				"imagegen_ckpt":      []byte(`"qwen-image-2512-Q5_1.gguf"`),
				"imagegen_placement": []byte(`"multicard"`),
			},
		}
	}
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: overlay})
	named := f.image(map[string]any{"family": "qi-pair"})
	probes := f.waitStarted(1)
	if probes[0].Env["GPU_LEASE_DEVICES"] != pairEnv() || probes[0].Env["COMFY_CARD_UUID"] != "" || !strings.Contains(strings.Join(probes[0].Argv, " "), "--placement multicard") {
		t.Errorf("the named family runs on the pair: %+v", probes[0])
	}
	f.letRunnersGo()
	wantAnswerNamesThePair(t, "the named family", f.await(named))

	g := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: overlay})
	def := g.image(nil)
	probes = g.waitStarted(1)
	if probes[0].Env["COMFY_CARD_UUID"] == "" || strings.Contains(strings.Join(probes[0].Argv, " "), "--placement") || strings.Contains(probes[0].Env["GPU_LEASE_DEVICES"], ",") {
		t.Errorf("the default binding of the same node stays on one card: %+v", probes[0])
	}
	g.letRunnersGo()
	wantAnswerNamesNoCards(t, "the default binding of the same node", g.await(def))
}

// The batch takes the same lease and the same argv as the single call: one pair for the whole batch.
func TestAnImageBatchOnAMulticardBindingLeasesThePairOnce(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: multicardBinding})
	f.letRunnersGo()
	items, _ := f.p.RunImageBatchAs(context.Background(), "", []ImageBatchJob{{Prompt: "a calm ocean at dawn", Out: filepath.Join(f.dir, "b1.png")}, {Prompt: "a calm lake at dusk", Out: filepath.Join(f.dir, "b2.png")}})
	if len(items) != 2 {
		t.Fatalf("%d items for two jobs", len(items))
	}
	for i, it := range items {
		// the batch answer names the pair on every item, in the JSON the CLI prints (the CLI layers the item's own JSON under its row)
		b, _ := json.Marshal(it)
		ids, present := answerDevices(t, b)
		if !present || strings.Join(ids, ",") != pairEnv() {
			t.Errorf("item %d must name the two cards the batch ran on, got devices = %v: %s", i, ids, b)
		}
	}
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
	wantAnswerNamesThePair(t, "edit-image", f.await(ch))
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
	wantAnswerNamesNoCards(t, "a plain edit", f.await(ch))
}

// An edit binding with no recipe (a 2.1 edit with no text encoder bound has none) is refused before a lease.
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
	// the edit canvas the binding pins reaches the verdict: the recipe's peaks are scaled to 2.0 MP, so a larger canvas is not the row
	big := f.cfg
	big.GenEditMegapixels = 3.0
	if got := f.p.declaredRAM(ctx, editNeed(big, ""), cards, pairIDs()); got <= 0 {
		t.Errorf("an edit canvas of 3.0 MP is above the recipe's 2.0 MP cap: %.1f GiB, want the ordinary streaming figure", got)
	}

	// a PREDICTED row (the 2511 Q5_1 GGUF edit) declares the ordinary estimate until a run has measured it: an early merge costs a wasteful wait, never a false 0
	gguf := f.cfg
	gguf.GenEditUnet = "qwen-image-edit-2511-Q5_1.gguf"
	predicted := editNeed(gguf, "")
	if predicted.Kind != needMulticard {
		t.Fatalf("the GGUF edit binding: kind %v", predicted.Kind)
	}
	ordinary := gguf
	ordinary.GenEditPlacement = ""
	want := f.p.declaredRAM(ctx, editNeed(ordinary, ""), cards, pairIDs())
	if want <= 0 {
		t.Fatalf("premise: the ordinary 2511 GGUF estimate on a 16 GiB card is %.1f GiB, want something to compare with", want)
	}
	if got := f.p.declaredRAM(ctx, predicted, cards, pairIDs()); got != want {
		t.Errorf("the predicted 2511 GGUF row: %.1f GiB, want the ordinary estimate %.1f GiB", got, want)
	}
	// the measured 2512 GGUF image row declares 0 on the pair
	if got := f.p.declaredRAM(ctx, imageNeed(f.cfg, ""), cards, pairIDs()); got != 0 {
		t.Errorf("the measured 2512 GGUF row: %.1f GiB, want 0", got)
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
			v := f.p.MediaLaneFree(context.Background(), laneReq(map[string]any{"width": 1024, "height": 1024})) // inside the row's canvas cap: the default size is refused up front
			if v.Free != tc.free {
				t.Fatalf("free = %v (%s), want %v", v.Free, v.Why, tc.free)
			}
			if !tc.free && (!strings.Contains(v.Why, A) || !strings.Contains(v.Why, C)) {
				t.Errorf("the reasons must name the pair: %s", v.Why)
			}
		})
	}
}

// krea2Fp8MulticardBinding asks for the card-only placement with a DiT that has no recipe row: krea2 fp8 (only the bf16 DiT has one).
func krea2Fp8MulticardBinding(c *config.Config) {
	krea2Binding(c)
	c.ImageGenCkpt, c.ImageGenPlacement = "krea2_turbo_fp8.safetensors", "multicard"
}

// A binding with no recipe is not judged free by omission: the probe says it did not judge, and why.
func TestMediaLaneFreeSaysWhenAMulticardBindingHasNoRecipe(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: krea2Fp8MulticardBinding})
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
		// the measured row declares no host RAM, so a short host is no reason to wait: the weights live on the two cards
		{name: "multicard, a short host and a measured row that declares no host RAM", spec: mc, grants: true,
			setup: func(t *testing.T, f *admitFixture) { f.useHost(t, func() gpuprobe.HostMemory { return shortHost(95) }) }},
		{name: "multicard on a box with one non-display card (the real admission refuses; the probe may not judge)", spec: mc, grants: false,
			setup: func(t *testing.T, f *admitFixture) { f.cards[2].Display = true }},
		{name: "multicard with no recipe (the real admission refuses; the probe says it did not judge)", grants: false,
			spec: admitSpec{order: admitOrder, mutate: krea2Fp8MulticardBinding}},
	}
}

// A batch on a binding without the placement answers as it always did: no item names cards, and the item marshals to the same bytes.
func TestAnImageBatchWithoutThePlacementNamesNoCards(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
	f.letRunnersGo()
	items, _ := f.p.RunImageBatchAs(context.Background(), "", []ImageBatchJob{{Prompt: "a calm ocean at dawn", Out: filepath.Join(f.dir, "b1.png")}})
	if len(items) != 1 {
		t.Fatalf("%d items for one job", len(items))
	}
	if items[0].Devices != nil {
		t.Errorf("a batch that held no pair names no cards, got %v", items[0].Devices)
	}
	b, _ := json.Marshal(items[0])
	if ids, present := answerDevices(t, b); present {
		t.Errorf("the item carries a devices key (%v): %s", ids, b)
	}
}

// The measured card-only zero is said in the log once per key, like every other declaration: a 0 the footprint store can never correct (calibratedRAM leaves
// 0 alone) is the one a reader of the log must be able to see.
func TestAMeasuredCardOnlyDeclarationOfZeroIsSaidInTheLogOncePerKey(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: multicardEditBinding})
	planNoteOnce.Range(func(k, _ any) bool { planNoteOnce.Delete(k); return true })
	buf := captureLogLines(t)
	ctx := context.Background()
	edit := editNeed(f.cfg, "")
	for i := 0; i < 3; i++ {
		if got := f.p.declaredRAM(ctx, edit, f.cards, pairIDs()); got != 0 {
			t.Fatalf("the pair: %.1f GiB, want 0", got)
		}
	}
	lines := linesWith(buf, "declaring host RAM: 0.0 GiB host RAM")
	if len(lines) != 1 || !strings.Contains(lines[0], "card-only placement") || !strings.Contains(lines[0], "read-only safetensors loader") {
		t.Fatalf("three declarations of one zero leave one line that says why, got %d:\n%s", len(lines), buf.String())
	}
	// a figure that is not zero is said as it always was, and a zero that is not the recipe's says nothing
	before := len(linesWith(buf, "declaring host RAM: "))
	if got := f.p.declaredRAM(ctx, edit, f.cards, pairIDs()[:1]); got <= 0 {
		t.Fatalf("one card: %.1f GiB, want the ordinary figure", got)
	}
	if after := len(linesWith(buf, "declaring host RAM: ")); after != before+1 {
		t.Errorf("the ordinary figure is one more line, got %d -> %d:\n%s", before, after, buf.String())
	}
}
