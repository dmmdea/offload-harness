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
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "needs a device") {
		t.Fatalf("Load must refuse a layer seat without a device by name, got %v", err)
	}
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
