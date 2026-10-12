package gpuprobe

// F11, fix round: throttle_reasons tells a card that was READ with nothing limiting its clocks from a card whose throttle
// column was not read.
//
// The key used to be omitted for both, so a healthy, queried card looked exactly like a failed query, an "[N/A]" or a driver
// with no throttle field at all, although the docs said an absent key is never "not throttled". Now a read with nothing
// active publishes an empty array, and absent means what the docs say: not read.

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestParseThermalTellsAReadWithNothingActiveFromAColumnNotRead: a mask of 0 and a mask of the idle bit alone are reads that
// found nothing limiting the clocks (non-nil, empty); "[Not Supported]", "[N/A]" and the temperature-only query's missing
// column are not reads (nil).
func TestParseThermalTellsAReadWithNothingActiveFromAColumnNotRead(t *testing.T) {
	out := strings.Join([]string{
		"0, " + thermUUID0 + ", 41, 0x0000000000000000",
		"1, " + thermUUID1 + ", 42, 0x0000000000000001", // idle alone is not a limit
		"2, " + thermUUID2 + ", 43, [Not Supported]",
		"3, " + thermUUID3 + ", 44, [N/A]",
		"4, GPU-5555eeee-6666-7777-8888-9999aaaabbbb, 45", // the temperature-only query
	}, "\n")
	rows, err := ParseThermal(out)
	if err != nil {
		t.Fatalf("ParseThermal: %v", err)
	}
	if len(rows) != 5 {
		t.Fatalf("rows = %+v, want 5", rows)
	}
	for i, wantRead := range []bool{true, true, false, false, false} {
		got := rows[i].ThrottleReasons
		if wantRead && (got == nil || len(got) != 0) {
			t.Errorf("row %d throttle reasons = %#v, want an empty non-nil slice: the card was read and nothing is active", i, got)
		}
		if !wantRead && got != nil {
			t.Errorf("row %d throttle reasons = %#v, want nil: the column was not read", i, got)
		}
	}
}

// TestMergeThermalCarriesAReadWithNothingActive: the empty read reaches the device (as a copy), and a row that read nothing
// leaves the device's field nil.
func TestMergeThermalCarriesAReadWithNothingActive(t *testing.T) {
	devs := []Device{{UUID: thermUUID0}, {UUID: thermUUID1}}
	rows := []Thermal{
		{UUID: thermUUID0, TemperatureC: intp(41), ThrottleReasons: []string{}},
		{UUID: thermUUID1, TemperatureC: intp(42)},
	}
	MergeThermal(devs, rows)
	if devs[0].ThrottleReasons == nil || len(devs[0].ThrottleReasons) != 0 {
		t.Errorf("device 0 throttle reasons = %#v, want an empty non-nil slice", devs[0].ThrottleReasons)
	}
	if devs[1].ThrottleReasons != nil {
		t.Errorf("device 1 throttle reasons = %#v, want nil: its row read no throttle column", devs[1].ThrottleReasons)
	}
	devs[0].ThrottleReasons = append(devs[0].ThrottleReasons, "changed")
	if len(rows[0].ThrottleReasons) != 0 {
		t.Errorf("a device edit reached the cached row: %#v", rows[0].ThrottleReasons)
	}
}

// TestDeviceJSONTellsAClearReadFromNoRead: the wire. [] when read and clear, no key when not read, and a delegator decoding
// the row sees the same two states.
func TestDeviceJSONTellsAClearReadFromNoRead(t *testing.T) {
	devs := []Device{{UUID: thermUUID0}, {UUID: thermUUID1}, {UUID: thermUUID2}}
	MergeThermal(devs, []Thermal{
		{UUID: thermUUID0, TemperatureC: intp(41), ThrottleReasons: []string{}},
		{UUID: thermUUID1, TemperatureC: intp(42)},
		{UUID: thermUUID2, TemperatureC: intp(83), ThrottleReasons: []string{"sw_thermal_slowdown"}},
	})
	wire := make([]string, len(devs))
	for i, d := range devs {
		b, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		wire[i] = string(b)
	}
	if !strings.Contains(wire[0], `"temperature_c":41,"throttle_reasons":[]`) {
		t.Errorf("a card read with nothing active marshalled as %s, want temperature_c and an empty throttle_reasons array", wire[0])
	}
	if strings.Contains(wire[1], "throttle_reasons") || !strings.Contains(wire[1], `"temperature_c":42`) {
		t.Errorf("a card whose throttle column was not read marshalled as %s, want temperature_c and no throttle_reasons key", wire[1])
	}
	if !strings.Contains(wire[2], `"throttle_reasons":["sw_thermal_slowdown"]`) {
		t.Errorf("a throttled card marshalled as %s, want its reason", wire[2])
	}
	var back [3]Device
	for i := range back {
		if err := json.Unmarshal([]byte(wire[i]), &back[i]); err != nil {
			t.Fatal(err)
		}
	}
	if back[0].ThrottleReasons == nil || len(back[0].ThrottleReasons) != 0 {
		t.Errorf("the delegator decoded %#v for a clear read, want an empty non-nil slice", back[0].ThrottleReasons)
	}
	if back[1].ThrottleReasons != nil {
		t.Errorf("the delegator decoded %#v for a column not read, want nil", back[1].ThrottleReasons)
	}
	if !reflect.DeepEqual(back[2].ThrottleReasons, []string{"sw_thermal_slowdown"}) {
		t.Errorf("the delegator decoded %#v for a throttled card", back[2].ThrottleReasons)
	}
}
