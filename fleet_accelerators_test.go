package main

import (
	"reflect"
	"testing"
)

// fleet-serve advertises the installer manifest's accelerator list when it has
// one, else the harness config's (Coral design D6). The Lenovo is a hand-built
// node with NO installed.json — before this fallback its health could never
// list a device, and a delegator could never route to it.
func TestFleetAcceleratorsManifestThenConfig(t *testing.T) {
	cases := []struct {
		name          string
		manifest, cfg []string
		want          []string
	}{
		{"manifest wins when it lists anything", []string{"hailo-8l"}, []string{"coral-edgetpu"}, []string{"hailo-8l"}},
		{"empty manifest falls back to config", nil, []string{"coral-edgetpu"}, []string{"coral-edgetpu"}},
		{"empty manifest slice (not nil) still falls back", []string{}, []string{"coral-edgetpu"}, []string{"coral-edgetpu"}},
		{"neither lists a device", nil, nil, nil},
	}
	for _, c := range cases {
		if got := fleetAccelerators(c.manifest, c.cfg); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
