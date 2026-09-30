package main

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/tierseed"
)

// Register A-113 (P1). The ampere-16 `fast` layer and the 35B seat behind it existed only in
// the reference box's hand-edited config and in two Go test files (internal/placement and
// internal/delegate pin the same layer values), never in setup/templates/profiles.json — so a
// fresh ampere-16 install lost both. These tests pin what the TABLE now carries and what a
// render of it seeds.
const (
	a16LaneSeat = "qwen38-27b-gsq-vllm"
	a16FastSeat = "qwen36-35b-a3b-gsq-vllm"
)

// referenceAmpere16Layers is the layer declaration the reference A2 node runs — the values
// internal/placement's and internal/delegate's tests pin — as the config schema spells them.
// Comparing the seed against it whole (not field by field) is what makes "a fresh install
// reproduces the hand-edited config" checkable: `audit-config` reports MATCH for the layer
// keys only if the two are JSON-identical.
func referenceAmpere16Layers() []config.LayerSpec {
	return []config.LayerSpec{
		{Name: "single", Tier: "ampere-16", Devices: []string{"0"}, Seats: []config.LayerSeat{
			{Role: "agent", Model: a16LaneSeat, Device: "0", CtxTokens: 32768, FootprintGiB: 13.9},
		}},
		{Name: "fast", Tier: "ampere-16", Devices: []string{"0"}, Seats: []config.LayerSeat{
			{Role: "agent", Model: a16FastSeat, Device: "0", CtxTokens: 32768, MaxInflight: 8, FootprintGiB: 11.7},
		}},
	}
}

func ampere16Profile(t *testing.T) tierseed.Profile {
	t.Helper()
	profiles, err := tierseed.Parse(embeddedProfiles)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := profiles["ampere-16"]
	if !ok {
		t.Fatal("no ampere-16 tier")
	}
	return p
}

func resolveAmpere16(t *testing.T, lane bool, fast bool) map[string]any {
	t.Helper()
	seed, err := tierseed.Resolve(ampere16Profile(t), "ampere-16", tierseed.Options{
		Home: "/opt/offload", GOOS: "linux", VLLMSeatActive: lane,
		ExtraVLLMSeatsActive: map[string]bool{a16FastSeat: fast},
	})
	if err != nil {
		t.Fatal(err)
	}
	return seed
}

// A box that runs both seats seeds the two layers, both seats in the roster, and one
// storeless binding per seat carrying its MEASURED reason — with no hand edit.
func TestAmpere16SeedCarriesTheFastLayerAndBothSeats(t *testing.T) {
	seed := resolveAmpere16(t, true, true)

	if seed["tier_profile"] != "ampere-16" || !reflect.DeepEqual(seed["tiers"], []string{"ampere-16"}) {
		t.Fatalf("identity = %v / %v, want tier_profile ampere-16 and tiers [ampere-16]", seed["tier_profile"], seed["tiers"])
	}
	layers, _ := seed["layers"].([]config.LayerSpec)
	if !reflect.DeepEqual(layers, referenceAmpere16Layers()) {
		got, _ := json.MarshalIndent(layers, "", " ")
		want, _ := json.MarshalIndent(referenceAmpere16Layers(), "", " ")
		t.Fatalf("the seeded layers are not the reference node's:\n got %s\nwant %s", got, want)
	}
	// The 27B stays the single layer's agent and the agent LANE: the 35B is a separate layer
	// reached by name, never a seat swap (blind coverage 4.65 against the 27B's 8.53).
	if seed["agent_model"] != a16LaneSeat {
		t.Fatalf("agent_model = %v, want %s — the fast seat must never bind the lane", seed["agent_model"], a16LaneSeat)
	}
	if got, want := seed["vllm_seats"], []string{a16LaneSeat, a16FastSeat}; !reflect.DeepEqual(got, want) {
		t.Fatalf("vllm_seats = %v, want %v", got, want)
	}
	binds, _ := seed["kv_cache_server"].([]map[string]any)
	if len(binds) != 2 {
		t.Fatalf("kv_cache_server = %v, want one storeless binding per vLLM seat", seed["kv_cache_server"])
	}
	measured := map[string][]string{
		// what the reason must carry, so the measurement travels and a generic sentence cannot replace it
		a16LaneSeat: {"MEASURED 2026-09-18", "register B-01", "LMCache 0.5.5", "util 0.85"},
		a16FastSeat: {"MEASURED 2026-09-18", "register B-01", "CUDA OOM", "util <= 0.85"},
	}
	for i, seat := range []string{a16LaneSeat, a16FastSeat} {
		b := binds[i]
		reason, _ := b["reason"].(string)
		if b["seat"] != seat || b["storeless"] != true || strings.TrimSpace(reason) == "" {
			t.Fatalf("binding %d = %v, want a storeless opt-out for %s with a non-empty reason", i, b, seat)
		}
		for _, frag := range measured[seat] {
			if !strings.Contains(reason, frag) {
				t.Errorf("%s's storeless reason lost its measurement: missing %q in %q", seat, frag, reason)
			}
		}
		if strings.HasPrefix(reason, "the tier declares no cache_server") {
			t.Errorf("%s's reason is the generic default, not the measured one: %q", seat, reason)
		}
	}

	// The seeded config loads exactly as the box loads it, and `doctor`'s per-seat cache
	// gate finds every roster seat bound — a fresh install must not ship a config its own
	// doctor rejects.
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
		t.Fatalf("doctor would fail these seats: %v", un)
	}
}

// The fast layer is reachable through the placement table exactly as the reference node's
// hand-edited config makes it: by name, on the 35B, at the window the seat serves.
func TestAmpere16FastLayerIsAPlaceableNamedLayer(t *testing.T) {
	seed := resolveAmpere16(t, true, true)
	b, _ := json.Marshal(seed)
	var c config.Config
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	fast, ok := c.LayerSeat("fast", "agent")
	if !ok || fast.Model != a16FastSeat || fast.CtxTokens != 32768 || fast.MaxInflight != 8 || fast.Device != "0" {
		t.Fatalf("fast/agent = %+v (present %v), want the 35B seat at 32,768 with 8 in flight on card 0", fast, ok)
	}
	single, ok := c.LayerSeat("single", "agent")
	if !ok || single.Model != a16LaneSeat {
		t.Fatalf("single/agent = %+v (present %v), want the 27B GSQ lane seat", single, ok)
	}
	// It is a layer, not a second default: neither layer is opt-in or dormant on this node.
	for _, l := range c.Layers {
		if l.OptIn || l.Dormant || len(l.Guards) != 0 {
			t.Errorf("layer %s carries opt_in/dormant/guards (%v/%v/%v) — the reference node declares plain layers", l.Name, l.OptIn, l.Dormant, l.Guards)
		}
	}
}

// Prerequisite-gated, exactly like the lane seat: no weights, no seat, no layer.
func TestAmpere16WithoutTheFastSeatSeedsOnlyTheSingleLayer(t *testing.T) {
	seed := resolveAmpere16(t, true, false)
	layers, _ := seed["layers"].([]config.LayerSpec)
	if len(layers) != 1 || layers[0].Name != "single" {
		t.Fatalf("layers = %+v, want only single when the 35B is absent", layers)
	}
	if got, want := seed["vllm_seats"], []string{a16LaneSeat}; !reflect.DeepEqual(got, want) {
		t.Fatalf("vllm_seats = %v, want only the 27B", got)
	}
	if binds, _ := seed["kv_cache_server"].([]map[string]any); len(binds) != 1 || binds[0]["seat"] != a16LaneSeat {
		t.Fatalf("kv_cache_server = %v, want the 27B's binding only", seed["kv_cache_server"])
	}
}

// A box with no vLLM venv is a plain llama.cpp box and must stay one: no composite
// identity, no roster, no bindings — the config it seeded before layers existed.
func TestAmpere16WithoutVLLMSeedsAPlainBox(t *testing.T) {
	seed := resolveAmpere16(t, false, false)
	for _, k := range []string{"layers", "tiers", "tier_profile", "vllm_seats", "kv_cache_server"} {
		if _, present := seed[k]; present {
			t.Errorf("a box without the vLLM venv must not seed %s (got %v)", k, seed[k])
		}
	}
	if seed["agent_model"] != "qwen3.5-4b-agent" {
		t.Errorf("agent_model = %v, want the llama.cpp fallback the tier seeds", seed["agent_model"])
	}
}

// PUBLIC-REPO RULE: the measured record travels, the local identity does not. The new
// declarations are read back as JSON and scanned; a machine name, a local path or an
// address in them is a leak that git history keeps.
func TestAmpere16NewDeclarationsCarryNoLocalIdentity(t *testing.T) {
	p := ampere16Profile(t)
	if len(p.ExtraVLLMSeats) != 1 {
		t.Fatalf("ampere-16 declares %d extra vLLM seats, want the 35B", len(p.ExtraVLLMSeats))
	}
	fields := map[string]any{
		"extra_vllm_seats":           p.ExtraVLLMSeats,
		"layers":                     p.Layers,
		"vllm_seat.storeless_reason": p.VLLMSeat.StorelessReason,
	}
	// The patterns are STRUCTURAL on purpose (a path root, a drive letter, a record folder, a dotted
	// quad): a test that spelled out the names it forbids would put those names in the public tree.
	forbidden := regexp.MustCompile(`(?i)/srv/|/home/|/mnt/|\b[a-z]:\\|Benchmarks and Optimizations|\b\d{1,3}(\.\d{1,3}){3}\b`)
	for name, v := range fields {
		b, _ := json.Marshal(v)
		if m := forbidden.FindString(string(b)); m != "" {
			t.Errorf("%s carries local identity %q — say \"the ampere-16 reference box\" and drop paths", name, m)
		}
	}
}
