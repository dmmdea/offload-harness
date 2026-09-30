package tierseed

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/vllmseat"
)

// A tier can serve MORE than one vLLM seat on its card: the lane seat that answers a
// default contract, and an on-demand seat backing a second layer. The table used to
// hold exactly one `vllm_seat`, so the second seat and the layer it backs lived only
// in a reference box's hand-edited config — the capability-loss class ADR 0048 was
// written against. These tests pin what a tier with an `extra_vllm_seats` list seeds.

const (
	laneSeatID  = "lane-27b-vllm"
	extraSeatID = "fast-35b-vllm"
)

func laneSeat() *vllmseat.Spec {
	return &vllmseat.Spec{
		ID: laneSeatID, Aliases: []string{"lane-pool"}, Unit: "vllm-agent-seat", Port: 18797, Device: "0",
		ModelRepo: "hub/models--org--lane-27b", MaxModelLen: 32768, GPUMemoryUtilization: 0.9,
		MaxNumSeqs: 32, MaxBatchedTokens: 4096, KVCacheDtype: "fp8_e5m2",
		ToolCallParser: "qwen3_xml", ReasoningParser: "qwen3",
		Fallback: "lane-4b-agent", FallbackCtx: 131072, AgentCtxTokens: 32768,
		StorelessReason: "lane measured reason",
	}
}

func extraSeat() vllmseat.Spec {
	return vllmseat.Spec{
		ID: extraSeatID, Aliases: []string{"fast-pool"}, Unit: "vllm-35b-seat", Port: 18797, Device: "0",
		ModelRepo: "hub/models--org--fast-35b", MaxModelLen: 32768, GPUMemoryUtilization: 0.9,
		MaxNumSeqs: 8, MaxBatchedTokens: 4096, KVCacheDtype: "fp8_e5m2",
		ToolCallParser: "qwen3_coder", ReasoningParser: "qwen3",
		StorelessReason: "fast measured reason",
	}
}

// twoSeatProfile is a one-card tier in the ampere-16 shape: two layers on device 0,
// the lane seat under `single` and the extra seat under `fast`.
func twoSeatProfile() Profile {
	return Profile{
		Backend:        "cuda",
		ConfigSeed:     map[string]any{"agent_model": "lane-4b-agent"},
		VLLMSeat:       laneSeat(),
		ExtraVLLMSeats: []vllmseat.Spec{extraSeat()},
		Layers: []config.LayerSpec{
			{Name: "single", Tier: "t", Devices: []string{"0"}, Seats: []config.LayerSeat{
				{Role: "agent", Model: laneSeatID, Device: "0", CtxTokens: 32768, FootprintGiB: 13.9},
			}},
			{Name: "fast", Tier: "t", Devices: []string{"0"}, Seats: []config.LayerSeat{
				{Role: "agent", Model: extraSeatID, Device: "0", CtxTokens: 32768, MaxInflight: 8, FootprintGiB: 11.7},
			}},
		},
	}
}

func resolveTwoSeat(t *testing.T, primary bool, extras ...string) map[string]any {
	t.Helper()
	active := map[string]bool{}
	for _, id := range extras {
		active[id] = true
	}
	seed, err := Resolve(twoSeatProfile(), "t", Options{GOOS: "linux", Home: "/opt/offload", VLLMSeatActive: primary, ExtraVLLMSeatsActive: active})
	if err != nil {
		t.Fatal(err)
	}
	return seed
}

func layerNames(seed map[string]any) []string {
	layers, _ := seed["layers"].([]config.LayerSpec)
	var out []string
	for _, l := range layers {
		out = append(out, l.Name)
	}
	return out
}

// The seed of a box that runs BOTH seats: both in the roster, one storeless binding
// each carrying its own measured reason, both layers, and the lane binding still the
// lane seat — an extra seat must never take `agent_model`.
func TestExtraSeatsSeedTheRosterTheBindingsAndTheLayers(t *testing.T) {
	seed := resolveTwoSeat(t, true, extraSeatID)
	if got, want := seed["vllm_seats"], []string{laneSeatID, extraSeatID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("vllm_seats = %v, want %v", got, want)
	}
	binds, _ := seed["kv_cache_server"].([]map[string]any)
	if len(binds) != 2 {
		t.Fatalf("kv_cache_server = %v, want one binding per seat", seed["kv_cache_server"])
	}
	for i, want := range []struct{ seat, reason string }{{laneSeatID, "lane measured reason"}, {extraSeatID, "fast measured reason"}} {
		b := binds[i]
		if b["seat"] != want.seat || b["storeless"] != true || b["reason"] != want.reason {
			t.Errorf("binding %d = %v, want a storeless opt-out for %s carrying %q", i, b, want.seat, want.reason)
		}
	}
	if seed["agent_model"] != laneSeatID {
		t.Fatalf("agent_model = %v, want the LANE seat %s — an extra seat never binds the lane", seed["agent_model"], laneSeatID)
	}
	if got, want := layerNames(seed), []string{"single", "fast"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("layers = %v, want %v", got, want)
	}
	if seed["tier_profile"] != "t" || !reflect.DeepEqual(seed["tiers"], []string{"t"}) {
		t.Fatalf("identity = %v / %v, want tier_profile t and tiers [t]", seed["tier_profile"], seed["tiers"])
	}
	// The seeded config must load exactly as a box loads it: layers valid, every roster
	// seat covered by a binding (what `doctor` fails on), no seat bound twice.
	b, _ := json.Marshal(seed)
	var c config.Config
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	if err := c.ValidateLayers(); err != nil {
		t.Fatalf("seeded layers must validate: %v", err)
	}
	if err := config.ValidateKVCacheServers(c.KVCacheServers); err != nil {
		t.Fatalf("seeded bindings must validate: %v", err)
	}
	if un := c.KVCacheServers.UnboundSeats(c.VLLMSeats); len(un) != 0 {
		t.Fatalf("a fresh install must not ship a config its own doctor rejects: unbound seats %v", un)
	}
}

// The extra seat is a prerequisite-gated capability exactly like the lane seat: a box
// that has not fetched its weights runs neither the seat nor the layer it backs, and
// the config must not advertise either.
func TestAnInactiveExtraSeatIsNotSeededAndItsLayerIsDropped(t *testing.T) {
	seed := resolveTwoSeat(t, true)
	if got, want := seed["vllm_seats"], []string{laneSeatID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("vllm_seats = %v, want only the lane seat %v", got, want)
	}
	if binds, _ := seed["kv_cache_server"].([]map[string]any); len(binds) != 1 || binds[0]["seat"] != laneSeatID {
		t.Fatalf("kv_cache_server = %v, want the lane seat's binding only", seed["kv_cache_server"])
	}
	if got, want := layerNames(seed), []string{"single"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("layers = %v, want only %v — the fast layer's seat is not served on this box", got, want)
	}
}

// `single` is the planner-default layer (placement row 5b): a layer set without it
// would make the node ineligible for every contract it ran the day before. A box
// whose lane seat is not running therefore seeds NO layers, even when an extra seat
// is present — it is a plain llama.cpp box that happens to hold a spare seat.
func TestWithoutTheLaneSeatNoLayerIsSeeded(t *testing.T) {
	seed := resolveTwoSeat(t, false, extraSeatID)
	for _, k := range []string{"layers", "tiers", "tier_profile"} {
		if _, present := seed[k]; present {
			t.Errorf("a box whose lane seat is absent must not seed %s (got %v)", k, seed[k])
		}
	}
	if got, want := seed["vllm_seats"], []string{extraSeatID}; !reflect.DeepEqual(got, want) {
		t.Errorf("vllm_seats = %v, want the running extra seat %v", got, want)
	}
	if seed["agent_model"] != "lane-4b-agent" {
		t.Errorf("agent_model = %v, want the lane seat's fallback", seed["agent_model"])
	}
}

// A box with no vLLM prerequisites at all must seed exactly what it seeded before
// layers and extra seats existed: no composite identity, no roster, no bindings.
func TestNoVLLMAtAllSeedsANonCompositePlainBox(t *testing.T) {
	seed := resolveTwoSeat(t, false)
	for _, k := range []string{"layers", "tiers", "tier_profile", "vllm_seats", "kv_cache_server"} {
		if _, present := seed[k]; present {
			t.Errorf("a box with no vLLM seat must not seed %s (got %v)", k, seed[k])
		}
	}
	if seed["agent_model"] != "lane-4b-agent" || seed["agent_ctx_tokens"] != 131072 {
		t.Errorf("the fallback binding moved: agent_model %v, agent_ctx_tokens %v", seed["agent_model"], seed["agent_ctx_tokens"])
	}
}

// Only an ACTIVE extra seat is validated at resolve (the box will run it); a broken
// declaration is caught for every seat at parse by Doc.Validate.
func TestAnActiveExtraSeatIsValidatedAtResolve(t *testing.T) {
	p := twoSeatProfile()
	p.ExtraVLLMSeats[0].ToolCallParser = ""
	if _, err := Resolve(p, "t", Options{Home: "/opt/offload", VLLMSeatActive: true, ExtraVLLMSeatsActive: map[string]bool{extraSeatID: true}}); err == nil ||
		!strings.Contains(err.Error(), extraSeatID) || !strings.Contains(err.Error(), "tool_call_parser") {
		t.Fatalf("an active extra seat with no tool parser must be refused at resolve naming the seat, got %v", err)
	}
	if _, err := Resolve(p, "t", Options{Home: "/opt/offload", VLLMSeatActive: true}); err != nil {
		t.Fatalf("an INACTIVE extra seat is not validated at resolve (the box will not run it): %v", err)
	}
}

// ResolveLayers is the one place a layer set is turned into what a box serves. It is
// exported because the render's composition check must reason about the same layers
// the seed writes.
func TestResolveLayersFollowsWhichSeatsTheBoxRuns(t *testing.T) {
	p := twoSeatProfile()
	set := func(primary bool, extra bool) VLLMSeatSet {
		return VLLMSeatSet{Primary: p.VLLMSeat, PrimaryActive: primary, Extras: p.ExtraVLLMSeats, ExtraActive: map[string]bool{extraSeatID: extra}}
	}
	names := func(ls []config.LayerSpec) []string {
		var out []string
		for _, l := range ls {
			out = append(out, l.Name)
		}
		return out
	}
	if got := names(ResolveLayers(p.Layers, set(true, true))); !reflect.DeepEqual(got, []string{"single", "fast"}) {
		t.Errorf("both active: %v", got)
	}
	if got := names(ResolveLayers(p.Layers, set(true, false))); !reflect.DeepEqual(got, []string{"single"}) {
		t.Errorf("extra inactive: %v", got)
	}
	if got := ResolveLayers(p.Layers, set(false, true)); got != nil {
		t.Errorf("lane inactive must resolve to no layers, got %v", names(got))
	}
	if got := ResolveLayers(p.Layers, set(false, false)); got != nil {
		t.Errorf("nothing active must resolve to no layers, got %v", names(got))
	}
	// An alias names the seat as surely as its id does.
	al := twoSeatLayersByAlias()
	if got := names(ResolveLayers(al, set(true, false))); !reflect.DeepEqual(got, []string{"single"}) {
		t.Errorf("a layer seat naming its vLLM seat by ALIAS must follow that seat's activity too: %v", got)
	}
	// The table's own slice is never mutated: one Profile resolves for many boxes.
	if len(p.Layers) != 2 || len(p.Layers[1].Seats) != 1 {
		t.Fatalf("ResolveLayers mutated the table row: %+v", p.Layers)
	}
	// A tier with no vLLM-named seat (the composite tier's layers) is resolved exactly
	// as before: its bare agent seat still derives from the lane seat.
	bare := []config.LayerSpec{{Name: "single", Tier: "t", Devices: []string{"0"}, Seats: []config.LayerSeat{{Role: "agent"}}}}
	got := ResolveLayers(bare, set(true, false))
	if len(got) != 1 || got[0].Seats[0].Model != "lane-pool" || got[0].Seats[0].MaxInflight != 32 {
		t.Errorf("a bare agent seat must still derive from the lane seat, got %+v", got)
	}
	got = ResolveLayers(bare, set(false, false))
	if len(got) != 1 || got[0].Seats[0].Model != "lane-4b-agent" || got[0].Seats[0].CtxTokens != 131072 {
		t.Errorf("a bare agent seat must still fall back to the lane seat's llama.cpp fallback, got %+v", got)
	}
}

func twoSeatLayersByAlias() []config.LayerSpec {
	p := twoSeatProfile()
	l := append([]config.LayerSpec{}, p.Layers...)
	l[1].Seats = []config.LayerSeat{{Role: "agent", Model: "fast-pool", Device: "0", CtxTokens: 32768, MaxInflight: 8}}
	return l
}

// mutateTable runs ParseDoc over a one-tier table built from twoSeatProfile with one
// authoring mistake applied, so each refusal below is a real parse of a real document.
func mutateTable(t *testing.T, mut func(p *Profile)) error {
	t.Helper()
	p := twoSeatProfile()
	mut(&p)
	// Profile marshals with the same tags the table uses; the vLLM seats and layers
	// round-trip through the strict layer-key pass exactly as a shipped row would.
	raw, err := json.Marshal(map[string]any{"profiles": map[string]Profile{"t": p}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ParseDoc(raw)
	return err
}

func TestTheUnmutatedTwoSeatTableParses(t *testing.T) {
	if err := mutateTable(t, func(*Profile) {}); err != nil {
		t.Fatalf("the baseline two-seat table must parse: %v", err)
	}
}

// Every refusal here is a shape the renderer cannot express truthfully, refused at
// PARSE so the copy embedded in the installer cannot carry it.
func TestDocValidateRefusesTheExtraSeatShapesThatCannotRender(t *testing.T) {
	for name, tc := range map[string]struct {
		mut  func(p *Profile)
		want string
	}{
		"extras without a lane seat": {func(p *Profile) { p.VLLMSeat = nil; p.Layers = nil }, "no vllm_seat"},
		"extra repeats the lane id":  {func(p *Profile) { p.ExtraVLLMSeats[0].ID = laneSeatID }, "twice"},
		"extra repeats a lane alias": {func(p *Profile) { p.ExtraVLLMSeats[0].Aliases = []string{"lane-pool"} }, "lane-pool"},
		"extra repeats the unit":     {func(p *Profile) { p.ExtraVLLMSeats[0].Unit = "vllm-agent-seat" }, "unit"},
		"extra on another card":      {func(p *Profile) { p.ExtraVLLMSeats[0].Device = "1" }, "device"},
		"extra carries a lane field": {func(p *Profile) { p.ExtraVLLMSeats[0].AgentMaxTokens = 4096 }, "agent_max_tokens"},
		"extra is broken":            {func(p *Profile) { p.ExtraVLLMSeats[0].MaxNumSeqs = 0 }, "max_num_seqs"},
		"layer window disagrees": {func(p *Profile) {
			p.Layers[1].Seats[0].CtxTokens = 65536
		}, "max_model_len"},
		"layer concurrency disagrees": {func(p *Profile) {
			p.Layers[1].Seats[0].MaxInflight = 32
		}, "max_num_seqs"},
		"layer pin disagrees": {func(p *Profile) {
			p.Layers[1].Seats[0].Device = "1"
		}, "device"},
	} {
		t.Run(name, func(t *testing.T) {
			err := mutateTable(t, tc.mut)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want a refusal naming %q, got %v", tc.want, err)
			}
		})
	}
}
