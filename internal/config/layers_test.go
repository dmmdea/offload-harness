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
