package main

import (
	"reflect"
	"testing"
)

// fleet-serve advertises the installer manifest's accelerator list when it has
// one, else the harness config's (Coral design D6). <node-c> is a hand-built
// node with NO installed.json — before this fallback its health could never
// list a device, and a delegator could never route to it.
func TestFleetAcceleratorsManifestThenConfig(t *testing.T) {
	cases := []struct {
		name          string
		manifest, cfg []string
		want          []string
	}{
		{"manifest wins when it lists anything", []string{"rknpu"}, []string{"coral-edgetpu"}, []string{"rknpu"}},
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

// TestFleetAcceleratorsNeverPublishTheStandaloneDevice (register E-08, operator decision J-13):
// the Hailo-8L stays on the box that carries it, so whichever source supplies the list, it never
// reaches /fleet/health. The decision is per device: the Coral and the RKNPU are still published
// (another box advertises each), and the choice of source is untouched — the manifest still wins
// when it lists anything, and the filter then applies to what the winner listed.
func TestFleetAcceleratorsNeverPublishTheStandaloneDevice(t *testing.T) {
	cases := []struct {
		name          string
		manifest, cfg []string
		want          []string
	}{
		{"manifest lists only the standalone device", []string{"hailo-8l"}, nil, nil},
		{"config lists only the standalone device", nil, []string{"hailo-8l"}, nil},
		{"manifest drops it and keeps the Coral", []string{"hailo-8l", "coral-edgetpu"}, nil, []string{"coral-edgetpu"}},
		{"manifest drops it wherever it sits", []string{"coral-edgetpu", "hailo-8l", "rknpu"}, nil, []string{"coral-edgetpu", "rknpu"}},
		{"config fallback drops it and keeps the RKNPU", nil, []string{"hailo-8l", "rknpu"}, []string{"rknpu"}},
		{"the manifest still wins when its only device is the standalone one", []string{"hailo-8l"}, []string{"coral-edgetpu"}, nil},
		{"the Coral alone is published", []string{"coral-edgetpu"}, nil, []string{"coral-edgetpu"}},
		{"the RKNPU alone is published", nil, []string{"rknpu"}, []string{"rknpu"}},
	}
	for _, c := range cases {
		if got := fleetAccelerators(c.manifest, c.cfg); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %#v, want %#v", c.name, got, c.want)
		}
	}
}
