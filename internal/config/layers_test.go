package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNoLayersIsNotCompositeAndRoundTripsWithoutTheKeys(t *testing.T) {
	c := Default()
	if c.Composite() {
		t.Fatal("Default() must not be composite")
	}
	if _, ok := c.LayerSeat("pair", "agent"); ok {
		t.Fatal("no layers → no layer seat")
	}
	b, _ := json.Marshal(c)
	for _, k := range []string{`"tier_profile"`, `"tiers"`, `"layers"`, `"operator_presence"`, `"operator_idle_sec"`} {
		if strings.Contains(string(b), k) {
			t.Fatalf("zero config must omit %s", k)
		}
	}
	if c.PresenceMode() != "present" || c.OperatorIdle() != 15*time.Minute {
		t.Fatalf("defaults: %q %v", c.PresenceMode(), c.OperatorIdle())
	}
	if c.AgentContextCapBytes() != 256<<10 {
		t.Fatalf("plain cap = %d", c.AgentContextCapBytes())
	}
}

func TestCompositeAccessorsResolveLayersSeatsAndTheCap(t *testing.T) {
	c := CompositeFixture()
	if !c.Composite() {
		t.Fatal("layers declared → composite")
	}
	s, ok := c.LayerSeat("pair", "long")
	if !ok || s.Model != "qwen3.8-27b-262k" || s.CtxTokens != 262144 {
		t.Fatalf("LayerSeat(pair,long) = %+v %v", s, ok)
	}
	if d := s.DeviceList(); len(d) != 2 || d[0] != "0" || d[1] != "2" {
		t.Fatalf("DeviceList = %v", d)
	}
	if _, ok := c.LayerSeat("pair", "stt"); ok {
		t.Fatal("undeclared role must not resolve")
	}
	if got := c.AgentContextCapBytes(); got != 262144*3 {
		t.Fatalf("composite cap = %d, want 786432", got)
	}
	if err := c.ValidateLayers(); err != nil {
		t.Fatalf("fixture must validate: %v", err)
	}
	c.OperatorPresence = "auto"
	if c.PresenceMode() != "auto" {
		t.Fatal("explicit auto must win")
	}
}

func TestValidateLayersRefusesTheShapesThatWouldMisplaceWork(t *testing.T) {
	cases := []struct {
		name string
		mut  func(c *Config)
		want string
	}{
		{"duplicate layer name", func(c *Config) { c.Layers[1].Name = "single" }, "duplicate layer"},
		{"empty devices", func(c *Config) { c.Layers[0].Devices = nil }, "no devices"},
		{"duplicate role", func(c *Config) { c.Layers[1].Seats[1].Role = "agent" }, "duplicate role"},
		{"seat without device", func(c *Config) { c.Layers[1].Seats[0].Device = "" }, "needs a device"},
		{"unknown guard", func(c *Config) { c.Layers[2].Guards = []string{"moon_phase"} }, "unknown guard"},
		{"floor without device", func(c *Config) { c.Layers[2].DisplayDevice = "" }, "display_floor_gib needs display_device"},
		{"seat without model", func(c *Config) { c.Layers[1].Seats[0].Model = "" }, "role agent needs a model"},
		{"router with a model", func(c *Config) { c.Layers[0].Seats[0].Model = "x" }, "router carries no model"},
		{"bad presence mode", func(c *Config) { c.OperatorPresence = "maybe" }, "operator_presence"},
		{"tier_profile missing from tiers", func(c *Config) { c.Tiers = []string{"blackwell-16"} }, "tiers must include tier_profile"},
		{"cap needs a window", func(c *Config) {
			for i := range c.Layers {
				for j := range c.Layers[i].Seats {
					c.Layers[i].Seats[j].CtxTokens = 0
				}
			}
		}, "no layer seat declares ctx_tokens"},
		{"display_floor seat without a footprint (triple)", func(c *Config) { c.Layers[2].Seats[0].DisplayFootprintGiB = 0 }, "display_footprint_gib is undeclared"},
		{"display_floor seat without a footprint (display)", func(c *Config) { c.Layers[3].Seats[0].DisplayFootprintGiB = 0 }, "display_footprint_gib is undeclared"},
		// The host_ram guard is free ≥ host_ram_gib; a 0 admits every load, so a
		// seat on a guarded layer without the number is a fail-open guard.
		{"host_ram guard without host_ram_gib", func(c *Config) { c.Layers[2].Seats[0].HostRAMGiB = 0 }, "host_ram_gib is undeclared"},
		// tokens ÷ prefill_tps ≤ budget: a 0 rate is a division by zero on the
		// first long placement — both long seats (pair and triple) owe the number.
		{"long seat without prefill_tps (pair)", func(c *Config) { c.Layers[1].Seats[1].PrefillTPS = 0 }, "prefill_tps is undeclared"},
		{"long seat without prefill_tps (triple)", func(c *Config) { c.Layers[2].Seats[0].PrefillTPS = 0 }, "prefill_tps is undeclared"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := CompositeFixture()
			tc.mut(&c)
			err := c.ValidateLayers()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

// TestLoadRefusesAMisplacedLayerByName pins the WIRING, not the rule: a config
// file whose layer seat carries no device pin must fail at Load with the key
// named — a validator that exists but is never called from the one door every
// entry point funnels through would admit the shape at runtime.
func TestLoadRefusesAMisplacedLayerByName(t *testing.T) {
	c := CompositeFixture()
	c.Layers[1].Seats[0].Device = ""
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "cfg.json")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	bad, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "needs a device") {
		t.Fatalf("Load must refuse a layer seat without a device by name, got %v", err)
	}
	// Load callers proceed on the VALUE (LoadWithSource keeps it; main's loadCfg
	// drops the Source), so the refused layers must not ride along with the error.
	assertNotComposite(t, bad)
	good := CompositeFixture()
	raw, _ = json.Marshal(good)
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(p)
	if err != nil {
		t.Fatalf("the fixture must load: %v", err)
	}
	if !loaded.Composite() || len(loaded.Layers) != 4 || loaded.TierProfile != "blackwell-3x16" {
		t.Fatalf("composite keys must survive a round trip through Load: %+v", loaded)
	}
}

// TestLoadNeverReturnsPlaceableLayersWithAnError pins the door the binary uses,
// not the seam: LoadWithSource keeps the config Load returns WITH an error and
// main's loadCfg discards the Source, so an errored value that still carried
// layers would be placed onto by `local-offload mcp`. Two exits that are NOT
// ValidateLayers — a validator that runs before it (seat_endpoints, so the
// layers were never even checked) and Load's own fleet_queue_holder check after
// load() succeeded — must both hand back the non-composite shape.
func TestLoadNeverReturnsPlaceableLayersWithAnError(t *testing.T) {
	cases := []struct {
		name string
		mut  func(c *Config)
		want string
	}{
		{"a validator before ValidateLayers", func(c *Config) { c.SeatEndpoints = map[string]string{"remote": "http://8.8.8.8:80"} }, `seat_endpoints["remote"]`},
		{"Load's own exit after load()", func(c *Config) { c.FleetQueueHolder = "http://8.8.8.8:80" }, "fleet_queue_holder"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := CompositeFixture()
			tc.mut(&c)
			raw, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(t.TempDir(), "cfg.json")
			if err := os.WriteFile(p, raw, 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := Load(p)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error naming %q, got %v", tc.want, err)
			}
			assertNotComposite(t, got)
		})
	}
}

// assertNotComposite is the shape a failed Load must hand back: every one of
// the five composite keys zero, so Composite() is false and no layer resolves.
func assertNotComposite(t *testing.T, c Config) {
	t.Helper()
	if c.Composite() || len(c.Layers) != 0 || len(c.Tiers) != 0 || c.TierProfile != "" || c.OperatorPresence != "" || c.OperatorIdleSec != 0 {
		t.Fatalf("a config returned with an error must carry no composite key: layers=%d tiers=%v tier_profile=%q presence=%q idle=%d",
			len(c.Layers), c.Tiers, c.TierProfile, c.OperatorPresence, c.OperatorIdleSec)
	}
	if _, ok := c.LayerSeat("pair", "agent"); ok {
		t.Fatal("no layer seat may resolve on a config returned with an error")
	}
}

// TestDisplayFootprintIsRequiredOnlyWhereConfigCanResolveThePin pins the two
// halves of the display_floor closure: config refuses a 0 footprint when it can
// see the seat's pin includes the display device (a plain CUDA index), lets a
// seat that never touches the display card through, and leaves a UUID-pinned
// display device to admission time (placement resolves it against the probe and
// refuses "display footprint undeclared" there).
func TestDisplayFootprintIsRequiredOnlyWhereConfigCanResolveThePin(t *testing.T) {
	c := CompositeFixture()
	c.Layers[2].Seats[0].DisplayFootprintGiB = 0
	if err := c.ValidateLayers(); err == nil || !strings.Contains(err.Error(), "display_footprint_gib is undeclared") {
		t.Fatalf("triple/long pins 0,1,2 with display device 1: must refuse, got %v", err)
	}
	c.Layers[2].Seats[0].Device = "0,2"
	if err := c.ValidateLayers(); err != nil {
		t.Fatalf("a seat off the display card owes no footprint: %v", err)
	}
	c = CompositeFixture()
	c.Layers[2].DisplayDevice = "GPU-2a44210f-6739-2d89-0e21-44cd5143faf7"
	c.Layers[2].Seats[0].DisplayFootprintGiB = 0
	if err := c.ValidateLayers(); err != nil {
		t.Fatalf("a UUID pin is resolved at admission, not at load: %v", err)
	}
}

// fixtureLayersJSON is the fixture's layers block as bytes, with one key
// respelt — the shape a hand-edited config.json or the tier table would carry
// after a typo. Quoted keys, so "footprint_gib" never matches inside
// "display_footprint_gib".
func fixtureLayersJSON(t *testing.T, from, to string) []byte {
	t.Helper()
	raw, err := json.Marshal(CompositeFixture().Layers)
	if err != nil {
		t.Fatal(err)
	}
	if from == "" {
		return raw
	}
	if !strings.Contains(string(raw), `"`+from+`"`) {
		t.Fatalf("fixture carries no key %q — the probe would test nothing", from)
	}
	return []byte(strings.ReplaceAll(string(raw), `"`+from+`"`, `"`+to+`"`))
}

// TestValidateLayerKeysRefusesAMisspeltKeyByPath pins the lint itself: a
// respelt seat key (the finding's host_ram_gb / prefill_tp / footprint_gb) and a
// respelt layer key are refused with the layer index, its name, the seat index,
// its role and the offending key — the path a table author edits — while the
// fixture's own spelling and an absent block both pass. The case-exact rule is
// pinned too: json would accept Host_RAM_GiB, this lint does not, so a key that
// passes here decodes on every reader.
func TestValidateLayerKeysRefusesAMisspeltKeyByPath(t *testing.T) {
	cases := []struct {
		from, to string
		want     []string
	}{
		{"host_ram_gib", "host_ram_gb", []string{`layers[2] "triple"`, `seats[0] "long"`, `unknown key "host_ram_gb"`, "fail-open"}},
		{"prefill_tps", "prefill_tp", []string{`layers[1] "pair"`, `seats[1] "long"`, `unknown key "prefill_tp"`}},
		{"footprint_gib", "footprint_gb", []string{`layers[0] "single"`, `seats[1] "agent"`, `unknown key "footprint_gb"`}},
		{"opt_in", "optin", []string{`layers[2] "triple"`, `unknown key "optin"`, "not a layer field"}},
		{"host_ram_gib", "Host_RAM_GiB", []string{`unknown key "Host_RAM_GiB"`}},
	}
	for _, tc := range cases {
		t.Run(tc.to, func(t *testing.T) {
			err := ValidateLayerKeys(fixtureLayersJSON(t, tc.from, tc.to))
			if err == nil {
				t.Fatalf("%s → %s must be refused", tc.from, tc.to)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Fatalf("error must carry %q, got %v", w, err)
				}
			}
		})
	}
	if err := ValidateLayerKeys(fixtureLayersJSON(t, "", "")); err != nil {
		t.Fatalf("the fixture's own keys must pass: %v", err)
	}
	if err := ValidateLayerKeys(nil); err != nil {
		t.Fatalf("no layers block is a plain box: %v", err)
	}
	if err := ValidateLayerKeys([]byte("null")); err != nil {
		t.Fatalf("a null layers block is a plain box: %v", err)
	}
	if err := ValidateLayerKeys([]byte(`{"name":"x"}`)); err == nil || !strings.Contains(err.Error(), "layers") {
		t.Fatalf("a layers block that is not an array must be refused, got %v", err)
	}
}

// TestLoadRefusesAMisspeltLayerKeyByName pins the WIRING on the config door:
// warnUnknownKeys reads only the top level, so before this a config.json whose
// triple seat said host_ram_gb loaded a well-formed layer with host_ram_gib 0 —
// a host_ram guard reading free ≥ 0 — and warned about nothing. Load must refuse
// it by name and hand back the non-composite shape, exactly like a layer
// ValidateLayers refuses.
func TestLoadRefusesAMisspeltLayerKeyByName(t *testing.T) {
	for _, tc := range []struct{ from, to string }{{"host_ram_gib", "host_ram_gb"}, {"prefill_tps", "prefill_tp"}} {
		t.Run(tc.to, func(t *testing.T) {
			c := CompositeFixture()
			raw, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			raw = []byte(strings.ReplaceAll(string(raw), `"`+tc.from+`"`, `"`+tc.to+`"`))
			p := filepath.Join(t.TempDir(), "cfg.json")
			if err := os.WriteFile(p, raw, 0o644); err != nil {
				t.Fatal(err)
			}
			bad, err := Load(p)
			if err == nil || !strings.Contains(err.Error(), `unknown key "`+tc.to+`"`) {
				t.Fatalf("Load must refuse the respelt key by name, got %v", err)
			}
			assertNotComposite(t, bad)
		})
	}
}
