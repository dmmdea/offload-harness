package gpualloc

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestServingPinsReadsOnlyTheEnvPin(t *testing.T) {
	pins, err := ServingPins(writeServing(t, servingYAML))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		pins string // comma-joined, "" = no usable pin
		why  string // a fragment of the reason when there is no pin
	}{
		{"yaml-card0", "GPU-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee", ""},
		{"yaml-card0-alias", "GPU-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee", ""},
		{"yaml-card2", "GPU-cccc3333-dddd-eeee-ffff-000000000000", ""},
		{"yaml-idx0", "0", ""},
		{"yaml-idx-noorder", "", "PCI_BUS_ID"},
		{"yaml-pair", "GPU-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee,GPU-cccc3333-dddd-eeee-ffff-000000000000", ""},
		{"yaml-unpinned", "", "pins no CUDA_VISIBLE_DEVICES in env"},
		{"yaml-cmd-only", "", "pins no CUDA_VISIBLE_DEVICES in env"},
		{"yaml-proxy", "", "proxy: seat"},
		{"yaml-macro", "", "not a literal device list"},
	} {
		p, ok := pins[tc.name]
		if !ok {
			t.Errorf("%s: not read", tc.name)
			continue
		}
		if got := strings.Join(p.Pins, ","); got != tc.pins {
			t.Errorf("%s: pins = %q, want %q (why %q)", tc.name, got, tc.pins, p.Why)
		}
		if tc.why != "" && !strings.Contains(p.Why, tc.why) {
			t.Errorf("%s: why = %q, want it to mention %q", tc.name, p.Why, tc.why)
		}
	}
}

func TestServingPinsRefusesAnAliasTwoEntriesClaimDifferently(t *testing.T) {
	pins, err := ServingPins(writeServing(t, `
models:
  a:
    aliases: [shared]
    env: ["CUDA_VISIBLE_DEVICES=GPU-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee"]
  b:
    aliases: [shared]
    env: ["CUDA_VISIBLE_DEVICES=GPU-cccc3333-dddd-eeee-ffff-000000000000"]
`))
	if err != nil {
		t.Fatal(err)
	}
	if p := pins["shared"]; len(p.Pins) != 0 || !strings.Contains(p.Why, "two entries") {
		t.Errorf("an alias two entries claim with different pins is unknown, got %+v", p)
	}
	if len(pins["a"].Pins) != 1 || len(pins["b"].Pins) != 1 {
		t.Errorf("the ids themselves stay placed: a=%+v b=%+v", pins["a"], pins["b"])
	}
}

func TestServingPinsErrors(t *testing.T) {
	if _, err := ServingPins(""); err == nil {
		t.Error("an unset path is an error")
	}
	if _, err := ServingPins(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("a missing file is an error")
	}
	if _, err := ServingPins(writeServing(t, "models: [unclosed")); err == nil {
		t.Error("a file that is not YAML is an error")
	}
}

func TestExplainGroupsByReasonInOrder(t *testing.T) {
	lines := Explain([]Verdict{
		{Model: "a", Unload: true, Why: "placement unknown (x)"},
		{Model: "b", Why: "placed elsewhere by layers"},
		{Model: "c", Unload: true, Why: "placement unknown (x)"},
	})
	want := []string{"unloaded, placement unknown (x): a, c", "left resident, placed elsewhere by layers: b"}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Errorf("lines = %q, want %q", lines, want)
	}
}
