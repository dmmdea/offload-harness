package config

import (
	"reflect"
	"strings"
	"testing"
)

// TestLocalOnlyAcceleratorIsDecidedPerDevice (register E-08): the standalone Hailo-8L stays on
// the box that carries it, and ONLY that device does. The Coral and the RKNPU are published,
// accepted and advertised over the fleet (another box advertises each of them), so a guard that
// is keyed on "any accelerator" or "any standalone box" strips a device the fleet depends on.
func TestLocalOnlyAcceleratorIsDecidedPerDevice(t *testing.T) {
	for _, c := range []struct {
		id   string
		want bool
	}{
		{"hailo-8l", true},
		{"  Hailo-8L ", true}, // a hand-edited spelling must not walk past the guard
		{"coral-edgetpu", false},
		{"rknpu", false},
		{"", false},
		{"tpu", false},
	} {
		if got := LocalOnlyAccelerator(c.id); got != c.want {
			t.Errorf("LocalOnlyAccelerator(%q) = %v, want %v", c.id, got, c.want)
		}
	}
}

// TestFleetVisibleAcceleratorsKeepsTheOrderAndDropsOnlyTheLocalOnlyDevice: the order is the
// shared-name rule's (ADR 0037, first-listed owner), and nothing left means nil — not an empty
// slice — so `omitempty` keeps the health key absent and a DeepEqual on "no device" holds.
func TestFleetVisibleAcceleratorsKeepsTheOrderAndDropsOnlyTheLocalOnlyDevice(t *testing.T) {
	for _, c := range []struct {
		name string
		in   []string
		want []string
	}{
		{"nil stays nil", nil, nil},
		{"empty becomes nil", []string{}, nil},
		{"the local-only device alone leaves nothing", []string{"hailo-8l"}, nil},
		{"the local-only device beside a fleet device", []string{"hailo-8l", "coral-edgetpu"}, []string{"coral-edgetpu"}},
		{"the same, listed the other way round", []string{"coral-edgetpu", "hailo-8l"}, []string{"coral-edgetpu"}},
		{"three devices keep the order of the other two", []string{"coral-edgetpu", "hailo-8l", "rknpu"}, []string{"coral-edgetpu", "rknpu"}},
		{"the Coral alone is published", []string{"coral-edgetpu"}, []string{"coral-edgetpu"}},
		{"the RKNPU alone is published", []string{"rknpu"}, []string{"rknpu"}},
		{"both fleet devices are published in order", []string{"rknpu", "coral-edgetpu"}, []string{"rknpu", "coral-edgetpu"}},
	} {
		if got := FleetVisibleAccelerators(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: FleetVisibleAccelerators(%v) = %#v, want %#v", c.name, c.in, got, c.want)
		}
	}
}

// The filter reads the caller's slice (config.Accelerators, Options.Accelerators); it must never
// reorder or shorten it, or the box's own local tool registration would lose the device.
func TestFleetVisibleAcceleratorsDoesNotMutateItsInput(t *testing.T) {
	in := []string{"hailo-8l", "coral-edgetpu", "rknpu"}
	_ = FleetVisibleAccelerators(in)
	if want := []string{"hailo-8l", "coral-edgetpu", "rknpu"}; !reflect.DeepEqual(in, want) {
		t.Fatalf("the input was changed: %v, want %v", in, want)
	}
}

// TestLocalOnlyAcceleratorFindings: a box that lists the standalone device in fleet_accelerators
// is asking the fleet for a device the fleet never serves, so every forwarded call would defer.
// That loads (a finding, never a refusal) and is named once per entry, with its index. The box
// that CARRIES the device (accelerators) is the normal case and says nothing.
func TestLocalOnlyAcceleratorFindings(t *testing.T) {
	for _, c := range []struct {
		name    string
		cfg     Config
		want    int
		mention []string
		absent  []string
	}{
		{"fleet_accelerators names the local-only device", Config{FleetAccelerators: []string{"hailo-8l"}}, 1,
			[]string{"fleet_accelerators[0]", "hailo-8l", "local-only"}, nil},
		{"only the local-only entry is named", Config{FleetAccelerators: []string{"coral-edgetpu", "hailo-8l"}}, 1,
			[]string{"fleet_accelerators[1]", "hailo-8l"}, []string{"coral-edgetpu"}},
		{"a hand-edited spelling is still named", Config{FleetAccelerators: []string{" Hailo-8L"}}, 1,
			[]string{"fleet_accelerators[0]"}, nil},
		{"the Coral in fleet_accelerators is the supported shape", Config{FleetAccelerators: []string{"coral-edgetpu"}}, 0, nil, nil},
		{"the RKNPU in fleet_accelerators is the supported shape", Config{FleetAccelerators: []string{"rknpu"}}, 0, nil, nil},
		{"the box that carries the device says nothing", Config{Accelerators: []string{"hailo-8l"}}, 0, nil, nil},
		{"a box with neither list says nothing", Config{}, 0, nil, nil},
	} {
		got := LocalOnlyAcceleratorFindings(c.cfg)
		if len(got) != c.want {
			t.Errorf("%s: %d findings %q, want %d", c.name, len(got), got, c.want)
			continue
		}
		joined := strings.Join(got, "\n")
		for _, m := range c.mention {
			if !strings.Contains(joined, m) {
				t.Errorf("%s: the finding must name %q, got %q", c.name, m, joined)
			}
		}
		for _, m := range c.absent {
			if strings.Contains(joined, m) {
				t.Errorf("%s: the finding must not name %q, got %q", c.name, m, joined)
			}
		}
	}
}

// The finding rides the loaded value (doctor reads Findings()) and is printed once at load, the
// same two doors as agent_call_deadline_sec — and the default config still has none.
func TestLoadAndDoctorReportAFleetAcceleratorThatIsLocalOnly(t *testing.T) {
	var cfg Config
	var err error
	stderr := captureStderr(t, func() {
		cfg, err = Load(writeShapeCfg(t, `{"fleet_accelerators":["hailo-8l"]}`))
	})
	if err != nil {
		t.Fatalf("a local-only device in fleet_accelerators must LOAD (warn, never refuse): %v", err)
	}
	if !strings.Contains(strings.Join(cfg.Findings(), "\n"), "fleet_accelerators[0]") {
		t.Fatalf("Findings() = %q, want doctor to carry the finding", cfg.Findings())
	}
	if !strings.Contains(stderr, "warning: fleet_accelerators[0]") {
		t.Fatalf("stderr at load = %q, want the startup warning", stderr)
	}
	if f := Default().Findings(); len(f) != 0 {
		t.Fatalf("the default config must produce no finding; got %q", f)
	}
	quiet := captureStderr(t, func() {
		cfg, err = Load(writeShapeCfg(t, `{"fleet_accelerators":["coral-edgetpu","rknpu"]}`))
	})
	if err != nil || len(cfg.Findings()) != 0 || strings.Contains(quiet, "fleet_accelerators") {
		t.Fatalf("the Coral and the RKNPU in fleet_accelerators are the supported shape: err=%v findings=%q stderr=%q", err, cfg.Findings(), quiet)
	}
}
