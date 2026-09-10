package tierseed

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// TestCompositeTierSeedsIdentityAndLayersAndPlainTiersDoNot pins the ADR 0039
// contract at the table's edge: the composite tier seeds its identity
// (tier_profile, tiers) and its layers into config, the pair's agent seat is
// DERIVED from vllm_seat so the window and the concurrency live in one place,
// the seeded layers pass config's own validator, and a plain tier seeds none of
// the three keys — that absence is what keeps every non-composite box
// byte-identical.
func TestCompositeTierSeedsIdentityAndLayersAndPlainTiersDoNot(t *testing.T) {
	profiles, err := Load(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	seed, err := Resolve(profiles["blackwell-3x16"], "blackwell-3x16", Options{GOOS: "windows", Home: "C:/x", RAMTier: "high", VLLMSeatActive: true})
	if err != nil {
		t.Fatal(err)
	}
	if seed["tier_profile"] != "blackwell-3x16" {
		t.Fatalf("tier_profile = %v", seed["tier_profile"])
	}
	tiers, _ := seed["tiers"].([]string)
	if len(tiers) != 3 || tiers[0] != "blackwell-16" || tiers[2] != "blackwell-3x16" {
		t.Fatalf("tiers = %v", seed["tiers"])
	}
	layers, ok := seed["layers"].([]config.LayerSpec)
	if !ok || len(layers) != 4 {
		t.Fatalf("layers = %#v", seed["layers"])
	}
	var pairAgent config.LayerSeat
	for _, l := range layers {
		if l.Name == "pair" {
			for _, s := range l.Seats {
				if s.Role == "agent" {
					pairAgent = s
				}
			}
		}
	}
	if pairAgent.Model != "agent-pool" || pairAgent.CtxTokens != 163840 || pairAgent.MaxInflight != 32 || pairAgent.Device != "0,2" {
		t.Fatalf("pair/agent must be derived from vllm_seat: %+v", pairAgent)
	}
	b, _ := json.Marshal(seed)
	var c config.Config
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	if err := c.ValidateLayers(); err != nil {
		t.Fatalf("seeded layers must validate: %v", err)
	}
	plain, err := Resolve(profiles["blackwell-2x16"], "blackwell-2x16", Options{GOOS: "windows", Home: "C:/x", RAMTier: "high", VLLMSeatActive: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"tier_profile", "tiers", "layers"} {
		if _, present := plain[k]; present {
			t.Fatalf("blackwell-2x16 must not seed %s", k)
		}
	}
}

// TestPairAgentFallsBackToTheLlamaCppSeatWhenVLLMIsAbsent: a box without the
// hand-built venv binds the seat's declared fallback (install seed does the same
// for agent_model), so the pair layer must describe the seat the box actually
// serves — the llama.cpp 27B on the same pair, with no vLLM concurrency count —
// or the placement table would advertise a 163k window nothing serves.
func TestPairAgentFallsBackToTheLlamaCppSeatWhenVLLMIsAbsent(t *testing.T) {
	profiles, err := Load(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	seed, err := Resolve(profiles["blackwell-3x16"], "blackwell-3x16", Options{GOOS: "linux", Home: "/opt/offload", RAMTier: "high"})
	if err != nil {
		t.Fatal(err)
	}
	layers, _ := seed["layers"].([]config.LayerSpec)
	var pairAgent config.LayerSeat
	for _, l := range layers {
		if l.Name == "pair" {
			for _, s := range l.Seats {
				if s.Role == "agent" {
					pairAgent = s
				}
			}
		}
	}
	if pairAgent.Model != "qwen3.8-27b" || pairAgent.CtxTokens != 131072 || pairAgent.MaxInflight != 0 || pairAgent.Device != "0,2" {
		t.Fatalf("pair/agent must be derived from the vllm_seat fallback: %+v", pairAgent)
	}
	if seed["agent_model"] != pairAgent.Model {
		t.Fatalf("the pair agent seat (%q) and agent_model (%v) must name the same seat", pairAgent.Model, seed["agent_model"])
	}
	// The table's own row is never mutated: the fill works on a copy, so a second
	// Resolve of the same Profile with the seat active still derives the vLLM seat.
	again, err := Resolve(profiles["blackwell-3x16"], "blackwell-3x16", Options{GOOS: "linux", Home: "/opt/offload", RAMTier: "high", VLLMSeatActive: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range again["layers"].([]config.LayerSpec) {
		if l.Name == "pair" {
			for _, s := range l.Seats {
				if s.Role == "agent" && s.Model != "agent-pool" {
					t.Fatalf("fillPairAgent mutated the table row: second resolve got %+v", s)
				}
			}
		}
	}
}

// TestDocValidateRefusesAnUnknownComposedTier: a composes id or a layer tier
// that is not in the table would seed a `tiers` list naming a tier no install
// can resolve, and health would advertise it to the fleet. Refused at parse so
// the embedded table cannot ship it.
func TestDocValidateRefusesAnUnknownComposedTier(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["profiles"].(map[string]any)["blackwell-3x16"].(map[string]any)["composes"] = []any{"blackwell-16", "blackwell-99"}
	b, _ := json.Marshal(doc)
	if _, err := ParseDoc(b); err == nil {
		t.Fatal("ParseDoc must refuse composes naming an unknown tier")
	}
	if _, err := Parse(b); err == nil {
		t.Fatal("Parse must refuse it too (same path)")
	}
}

// TestDocValidateRefusesTheCompositeShapesThatCannotSeed pins the other
// authoring mistakes Doc.Validate catches: a layer standing for a tier the table
// does not know, a tier composing itself, and composes without layers (nothing
// would be seeded, silently).
func TestDocValidateRefusesTheCompositeShapesThatCannotSeed(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		mut  func(p map[string]any)
		want string
	}{
		{"layer tier unknown", func(p map[string]any) {
			p["layers"].([]any)[0].(map[string]any)["tier"] = "blackwell-99"
		}, "blackwell-99"},
		{"composes itself", func(p map[string]any) {
			p["composes"] = []any{"blackwell-16", "blackwell-3x16"}
		}, "composes itself"},
		{"composes without layers", func(p map[string]any) {
			delete(p, "layers")
		}, "no layers"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var doc map[string]any
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatal(err)
			}
			tc.mut(doc["profiles"].(map[string]any)["blackwell-3x16"].(map[string]any))
			b, _ := json.Marshal(doc)
			_, err := ParseDoc(b)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want a refusal naming %q, got %v", tc.want, err)
			}
		})
	}
}

// TestConfigSeedMayNotWriteTheCompositeKeys: tier_profile, tiers and layers are
// written by the table's composes/layers through Resolve — a config_seed carrying
// them is a second writer, the exact way a binding and the seat it names drift
// apart (see the media_seat rule above it in validate).
func TestConfigSeedMayNotWriteTheCompositeKeys(t *testing.T) {
	for _, k := range []string{"tier_profile", "tiers", "layers"} {
		_, err := Resolve(Profile{Backend: "cuda", ConfigSeed: map[string]any{k: "x"}}, "t", Options{})
		if err == nil || !strings.Contains(err.Error(), k) {
			t.Fatalf("config_seed.%s must be refused by name, got %v", k, err)
		}
	}
}

// TestSeededLayersAreValidatedAtResolve: a layer set that config would refuse at
// load must die at authoring time (this package's whole reason to exist), not on
// the first install of the tier.
func TestSeededLayersAreValidatedAtResolve(t *testing.T) {
	profiles, err := Load(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	p := profiles["blackwell-3x16"]
	layers := append([]config.LayerSpec{}, p.Layers...)
	layers[0].Devices = nil
	p.Layers = layers
	_, err = Resolve(p, "blackwell-3x16", Options{GOOS: "windows", Home: "C:/x", VLLMSeatActive: true})
	if err == nil || !strings.Contains(err.Error(), "no devices") {
		t.Fatalf("an invalid seeded layer must be refused at resolve, got %v", err)
	}
}

// TestParseDocRefusesAMisspeltLayerKey is the review's probe, kept as the pin:
// respell host_ram_gib → host_ram_gb (and prefill_tps → prefill_tp,
// footprint_gib → footprint_gb) in the shipped table and, before this, ParseDoc
// returned nil, Resolve returned nil, and the seeded triple/long seat carried NO
// host_ram_gib — a host_ram guard of free ≥ 0 admitting the ~70 GB load on a
// box with 20 GB free, from one dropped character with zero signal. Every
// reader (ParseDoc, Parse — the installer's embedded copy — and Load) must
// refuse it, naming the tier and the JSON path. The quoted-key replacement
// keeps "display_footprint_gib" out of the "footprint_gib" case.
func TestParseDocRefusesAMisspeltLayerKey(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		from, to string
		want     []string
	}{
		{"host_ram_gib", "host_ram_gb", []string{`tier "blackwell-3x16"`, `layers[2] "triple"`, `seats[0] "long"`, `unknown key "host_ram_gb"`}},
		{"prefill_tps", "prefill_tp", []string{`tier "blackwell-3x16"`, `layers[1] "pair"`, `seats[1] "long"`, `unknown key "prefill_tp"`}},
		{"footprint_gib", "footprint_gb", []string{`tier "blackwell-3x16"`, `layers[0] "single"`, `unknown key "footprint_gb"`}},
		{"dormant", "dormnt", []string{`tier "blackwell-3x16"`, `layers[3] "display"`, `unknown key "dormnt"`}},
	}
	for _, tc := range cases {
		t.Run(tc.to, func(t *testing.T) {
			if !strings.Contains(string(raw), `"`+tc.from+`"`) {
				t.Fatalf("the table carries no key %q — the probe would test nothing", tc.from)
			}
			b := []byte(strings.ReplaceAll(string(raw), `"`+tc.from+`"`, `"`+tc.to+`"`))
			for name, parse := range map[string]func([]byte) error{
				"ParseDoc": func(b []byte) error { _, err := ParseDoc(b); return err },
				"Parse":    func(b []byte) error { _, err := Parse(b); return err },
			} {
				err := parse(b)
				if err == nil {
					t.Fatalf("%s must refuse %s → %s", name, tc.from, tc.to)
				}
				for _, w := range tc.want {
					if !strings.Contains(err.Error(), w) {
						t.Fatalf("%s: error must carry %q, got %v", name, w, err)
					}
				}
			}
		})
	}
	// And the one the strict pass cannot see — a key spelled right but left
	// out — dies in Validate, which runs config's own validator on the seeded
	// layers: a host_ram-guarded seat without its number is the same fail-open
	// guard by a different route.
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	triple := doc["profiles"].(map[string]any)["blackwell-3x16"].(map[string]any)["layers"].([]any)[2].(map[string]any)
	delete(triple["seats"].([]any)[0].(map[string]any), "host_ram_gib")
	b, _ := json.Marshal(doc)
	if _, err := ParseDoc(b); err == nil || !strings.Contains(err.Error(), "host_ram_gib is undeclared") {
		t.Fatalf("a host_ram-guarded seat without host_ram_gib must be refused at parse, got %v", err)
	}
}

// TestFieldsDocumentExactlyTheLayerKeys makes the table's _fields block a
// lint, not prose: every layers[].* / layers[].seats[].* entry must be a json
// tag on config.LayerSpec / config.LayerSeat and every tag must have an entry,
// so the documented spelling IS the accepted spelling — an author who copies a
// key from _fields cannot copy one the decoder will drop, and a field added to
// the struct cannot ship undocumented.
func TestFieldsDocumentExactlyTheLayerKeys(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	var top struct {
		Fields map[string]string `json:"_fields"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for _, k := range config.LayerKeysOf() {
		want[k] = true
	}
	if len(want) == 0 {
		t.Fatal("config.LayerKeysOf returned nothing — the lint would pass vacuously")
	}
	documented := map[string]bool{}
	for k, v := range top.Fields {
		if !strings.HasPrefix(k, "layers[].") {
			continue
		}
		if !want[k] {
			t.Errorf("_fields documents %q, which is not a layer or seat field — the decoder would drop it", k)
		}
		if strings.TrimSpace(v) == "" {
			t.Errorf("_fields %q is empty", k)
		}
		documented[k] = true
	}
	for k := range want {
		if !documented[k] {
			t.Errorf("_fields lacks %q — every layer/seat field the decoder accepts must be documented", k)
		}
	}
}
