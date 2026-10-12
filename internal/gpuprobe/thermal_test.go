package gpuprobe

// F11: temperature and the active clock event reasons per card, read by a second, optional nvidia-smi query.
//
// The throttle bit values asserted below are the ones in NVML's own header (nvml.h, CUDA 13.4 include dir, the
// nvmlClocksEventReason* / nvmlClocksThrottleReason* defines), read from the file and not from memory:
//
//	0x1 GpuIdle   0x2 ApplicationsClocksSetting   0x4 SwPowerCap   0x8 HwSlowdown   0x10 SyncBoost
//	0x20 SwThermalSlowdown   0x40 HwThermalSlowdown   0x80 HwPowerBrakeSlowdown   0x100 DisplayClockSetting
//	0x200 BoardLimit   0x400 Reliability
//
// The query field is called clocks_throttle_reasons.active on older drivers and clocks_event_reasons.active on
// newer ones (nvidia-smi --help-query-gpu lists both names for one field on a current driver); the reader tries
// the older name first and the renamed one when the driver refuses it.

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const (
	thermUUID0 = "GPU-1111aaaa-2222-3333-4444-555566667777"
	thermUUID1 = "GPU-2222bbbb-3333-4444-5555-666677778888"
	thermUUID2 = "GPU-3333cccc-4444-5555-6666-777788889999"
	thermUUID3 = "GPU-4444dddd-5555-6666-7777-88889999aaaa"

	fmtArg  = "--format=csv,noheader,nounits"
	argOld  = "--query-gpu=index,uuid,temperature.gpu,clocks_throttle_reasons.active"
	argNew  = "--query-gpu=index,uuid,temperature.gpu,clocks_event_reasons.active"
	argTemp = "--query-gpu=index,uuid,temperature.gpu"
)

func intp(v int) *int { return &v }

func TestParseThermalReadsTemperatureAndNamesTheThrottleBits(t *testing.T) {
	out := "0, " + thermUUID0 + ", 83, 0x0000000000000060\r\n1, " + thermUUID1 + ", 41, 0x0000000000000000\r\n"
	rows, err := ParseThermal(out)
	if err != nil {
		t.Fatalf("ParseThermal: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2", rows)
	}
	r := rows[0]
	if r.Index != 0 || r.UUID != thermUUID0 || r.TemperatureC == nil || *r.TemperatureC != 83 {
		t.Fatalf("row 0 = %+v, want index 0, %s, 83 C", r, thermUUID0)
	}
	if want := []string{"sw_thermal_slowdown", "hw_thermal_slowdown"}; !reflect.DeepEqual(r.ThrottleReasons, want) {
		t.Fatalf("0x60 named %v, want both thermal bits %v", r.ThrottleReasons, want)
	}
	if rows[1].TemperatureC == nil || *rows[1].TemperatureC != 41 || len(rows[1].ThrottleReasons) != 0 {
		t.Fatalf("row 1 = %+v, want 41 C and no reason (mask 0)", rows[1])
	}
}

func TestThrottleReasonNamesAreTheNVMLHeaders(t *testing.T) {
	cases := []struct {
		mask uint64
		want []string
	}{
		{0x0, nil},
		{0x1, nil}, // gpu idle is not a throttle worth publishing
		{0x2, []string{"applications_clocks_setting"}},
		{0x4, []string{"sw_power_cap"}},
		{0x8, []string{"hw_slowdown"}},
		{0x10, []string{"sync_boost"}},
		{0x20, []string{"sw_thermal_slowdown"}},
		{0x40, []string{"hw_thermal_slowdown"}},
		{0x80, []string{"hw_power_brake_slowdown"}},
		{0x100, []string{"display_clock_setting"}},
		{0x200, []string{"board_limit"}},
		{0x400, []string{"reliability"}},
		{0x401, []string{"reliability"}},
		{0x60, []string{"sw_thermal_slowdown", "hw_thermal_slowdown"}},
		{0x84, []string{"sw_power_cap", "hw_power_brake_slowdown"}},
		// A bit the header does not name yet is published as itself, never dropped.
		{0x800, []string{"unknown:0x800"}},
		{0xc00, []string{"reliability", "unknown:0x800"}},
		{0x8000000000000000, []string{"unknown:0x8000000000000000"}},
	}
	for _, c := range cases {
		if got := ThrottleReasonNames(c.mask); !reflect.DeepEqual(got, c.want) {
			t.Errorf("ThrottleReasonNames(%#x) = %v, want %v", c.mask, got, c.want)
		}
	}
}

func TestParseThermalLeavesOutWhatTheDriverDoesNotSupport(t *testing.T) {
	out := strings.Join([]string{
		"0, " + thermUUID0 + ", [N/A], [Not Supported]",
		"1, " + thermUUID1 + ", 55, [Not Supported]",
		"2, " + thermUUID2 + ", [Not Supported], 0x0000000000000004",
		"3, " + thermUUID3 + ", 61", // the temperature-only query
	}, "\n")
	rows, err := ParseThermal(out)
	if err != nil {
		t.Fatalf("ParseThermal: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %+v, want 4 (a card with nothing readable still has its row)", rows)
	}
	if rows[0].TemperatureC != nil || len(rows[0].ThrottleReasons) != 0 {
		t.Errorf("row 0 = %+v, want neither value", rows[0])
	}
	if rows[1].TemperatureC == nil || *rows[1].TemperatureC != 55 || len(rows[1].ThrottleReasons) != 0 {
		t.Errorf("row 1 = %+v, want 55 C and no reason: [Not Supported] is not a mask", rows[1])
	}
	if rows[2].TemperatureC != nil || !reflect.DeepEqual(rows[2].ThrottleReasons, []string{"sw_power_cap"}) {
		t.Errorf("row 2 = %+v, want no temperature but sw_power_cap: one unreadable column must not cost the other", rows[2])
	}
	if rows[3].TemperatureC == nil || *rows[3].TemperatureC != 61 || len(rows[3].ThrottleReasons) != 0 {
		t.Errorf("row 3 = %+v, want 61 C from the three-column query", rows[3])
	}
}

func TestParseThermalSkipsGarbageAndErrorsWhenNothingParses(t *testing.T) {
	rows, err := ParseThermal("garbage\n\n0, " + thermUUID0 + ", 70, 0x0\nx, y\n")
	if err != nil || len(rows) != 1 || rows[0].UUID != thermUUID0 {
		t.Fatalf("ParseThermal = %+v, %v, want the one good row", rows, err)
	}
	if rows, err := ParseThermal("garbage\n\nnot, a, row, at all, here\n"); err == nil {
		t.Fatalf("ParseThermal of no valid row = %+v, nil; want an error (an empty success reads as a cool node)", rows)
	}
}

func TestParseThermalDoesNotPublishAnImplausibleTemperature(t *testing.T) {
	for _, bad := range []string{"0", "-5", "400"} {
		rows, err := ParseThermal("0, " + thermUUID0 + ", " + bad + ", 0x0\n")
		if err != nil || len(rows) != 1 {
			t.Fatalf("temperature %s: %+v, %v", bad, rows, err)
		}
		if rows[0].TemperatureC != nil {
			t.Errorf("temperature %s published as %d C: a garbled counter is not a reading", bad, *rows[0].TemperatureC)
		}
	}
}

func TestMergeThermalMatchesByUUIDNotIndex(t *testing.T) {
	devs := []Device{{Index: 0, UUID: thermUUID0}, {Index: 1, UUID: thermUUID1}, {Index: 2, UUID: thermUUID2}}
	rows := []Thermal{
		{Index: 0, UUID: thermUUID1, TemperatureC: intp(70), ThrottleReasons: []string{"sw_power_cap"}}, // the board reordered
		{Index: 1, UUID: thermUUID0, TemperatureC: intp(50)},
		{Index: 2, UUID: "GPU-ffff0000-0000-0000-0000-000000000000", TemperatureC: intp(99)}, // no such card here
	}
	MergeThermal(devs, rows)
	if devs[0].TemperatureC == nil || *devs[0].TemperatureC != 50 {
		t.Errorf("device 0 = %+v, want the 50 C of its UUID, not of index 0's row", devs[0])
	}
	if devs[1].TemperatureC == nil || *devs[1].TemperatureC != 70 || !reflect.DeepEqual(devs[1].ThrottleReasons, []string{"sw_power_cap"}) {
		t.Errorf("device 1 = %+v, want 70 C and sw_power_cap", devs[1])
	}
	if devs[2].TemperatureC != nil || len(devs[2].ThrottleReasons) != 0 {
		t.Errorf("device 2 = %+v, want untouched: its UUID has no row", devs[2])
	}
}

func TestMergeThermalCopiesTheRowsItIsGiven(t *testing.T) {
	rows := []Thermal{{UUID: thermUUID0, TemperatureC: intp(60), ThrottleReasons: []string{"hw_slowdown"}}}
	devs := []Device{{UUID: thermUUID0}}
	MergeThermal(devs, rows)
	if devs[0].TemperatureC == nil {
		t.Fatal("nothing merged")
	}
	*devs[0].TemperatureC = 1
	devs[0].ThrottleReasons[0] = "changed"
	if *rows[0].TemperatureC != 60 || rows[0].ThrottleReasons[0] != "hw_slowdown" {
		t.Fatalf("a device edit reached the cached row: %+v", rows[0])
	}
}

// The guard: a card with no thermal reading marshals exactly as it did before the fields existed.
func TestThermalUnavailableLeavesTheDeviceJSONByteIdentical(t *testing.T) {
	const want = `{"index":0,"uuid":"GPU-1111aaaa-2222-3333-4444-555566667777","name":"NVIDIA GeForce RTX 5060 Ti","vram_total_gb":16,"vram_free_gb":15.5,"util_pct":3,"util_known":true}`
	d := Device{Index: 0, UUID: thermUUID0, Name: "NVIDIA GeForce RTX 5060 Ti", TotalGiB: 16, FreeGiB: 15.5, UtilPct: 3, UtilKnown: true}
	for name, rows := range map[string][]Thermal{
		"no rows":        nil,
		"a row of nils":  {{UUID: thermUUID0}},
		"another card's": {{UUID: thermUUID1, TemperatureC: intp(80)}},
	} {
		devs := []Device{d}
		MergeThermal(devs, rows)
		b, err := json.Marshal(devs[0])
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != want {
			t.Errorf("%s: device JSON = %s\nwant            %s", name, b, want)
		}
	}
}

// thermalFake answers a scripted driver and records the args of every call.
type thermalFake struct {
	calls [][]string
	reply func(args []string) (string, error)
}

func (f *thermalFake) run(_ context.Context, args []string) (string, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	return f.reply(args)
}

func (f *thermalFake) queries() []string {
	var q []string
	for _, c := range f.calls {
		q = append(q, c[0])
	}
	return q
}

func refusal(field string) string {
	return "Field \"" + field + "\" is not a valid field to query.\n\n"
}

const goodRow = "0, " + thermUUID0 + ", 77, 0x0000000000000020\n"

func resetThermal(t *testing.T) {
	t.Helper()
	ResetThermalState()
	t.Cleanup(ResetThermalState)
}

func TestReadThermalAsksForTheOlderFieldNameFirst(t *testing.T) {
	resetThermal(t)
	f := &thermalFake{reply: func(args []string) (string, error) {
		if args[0] == argOld {
			return goodRow, nil
		}
		return refusal("clocks_event_reasons.active"), errExit2
	}}
	rows, err := readThermalWith(context.Background(), f.run)
	if err != nil || len(rows) != 1 || rows[0].TemperatureC == nil || *rows[0].TemperatureC != 77 {
		t.Fatalf("readThermalWith = %+v, %v", rows, err)
	}
	if !reflect.DeepEqual(rows[0].ThrottleReasons, []string{"sw_thermal_slowdown"}) {
		t.Errorf("reasons = %v, want sw_thermal_slowdown", rows[0].ThrottleReasons)
	}
	if want := []string{argOld}; !reflect.DeepEqual(f.queries(), want) {
		t.Errorf("queries = %v, want %v", f.queries(), want)
	}
	if f.calls[0][1] != fmtArg {
		t.Errorf("format arg = %q, want %q", f.calls[0][1], fmtArg)
	}
}

func TestReadThermalFallsBackToTheRenamedFieldAndRemembersIt(t *testing.T) {
	resetThermal(t)
	f := &thermalFake{reply: func(args []string) (string, error) {
		switch args[0] {
		case argNew:
			return goodRow, nil
		case argOld:
			return refusal("clocks_throttle_reasons.active"), errExit2
		}
		return transientOut, errTransient
	}}
	for i := 0; i < 2; i++ {
		rows, err := readThermalWith(context.Background(), f.run)
		if err != nil || len(rows) != 1 || !reflect.DeepEqual(rows[0].ThrottleReasons, []string{"sw_thermal_slowdown"}) {
			t.Fatalf("call %d: %+v, %v", i, rows, err)
		}
	}
	// One doomed spawn to learn the name, then one spawn per read: an old driver costs one process per window.
	if want := []string{argOld, argNew, argNew}; !reflect.DeepEqual(f.queries(), want) {
		t.Errorf("queries = %v, want %v", f.queries(), want)
	}
}

func TestReadThermalFallsBackToTheTemperatureAloneWhenNeitherNameIsKnown(t *testing.T) {
	resetThermal(t)
	f := &thermalFake{reply: func(args []string) (string, error) {
		switch args[0] {
		case argTemp:
			return "0, " + thermUUID0 + ", 64\n", nil
		case argOld:
			return refusal("clocks_throttle_reasons.active"), errExit2
		case argNew:
			return refusal("clocks_event_reasons.active"), errExit2
		}
		return transientOut, errTransient
	}}
	for i := 0; i < 2; i++ {
		rows, err := readThermalWith(context.Background(), f.run)
		if err != nil || len(rows) != 1 || rows[0].TemperatureC == nil || *rows[0].TemperatureC != 64 || len(rows[0].ThrottleReasons) != 0 {
			t.Fatalf("call %d: %+v, %v, want 64 C and no reasons", i, rows, err)
		}
	}
	if want := []string{argOld, argNew, argTemp, argTemp}; !reflect.DeepEqual(f.queries(), want) {
		t.Errorf("queries = %v, want %v", f.queries(), want)
	}
}

func TestReadThermalTriesTheOtherNamesWhenARememberedOneStopsWorking(t *testing.T) {
	resetThermal(t)
	newWorks := true
	f := &thermalFake{reply: func(args []string) (string, error) {
		switch {
		case args[0] == argNew && newWorks:
			return goodRow, nil
		case args[0] == argOld && !newWorks:
			return goodRow, nil
		case args[0] == argNew:
			return refusal("clocks_event_reasons.active"), errExit2
		}
		return refusal("clocks_throttle_reasons.active"), errExit2
	}}
	if _, err := readThermalWith(context.Background(), f.run); err != nil { // learns the renamed field
		t.Fatal(err)
	}
	newWorks = false // a driver downgrade
	rows, err := readThermalWith(context.Background(), f.run)
	if err != nil || len(rows) != 1 {
		t.Fatalf("after the downgrade: %+v, %v", rows, err)
	}
	if want := []string{argOld, argNew, argNew, argOld}; !reflect.DeepEqual(f.queries(), want) {
		t.Errorf("queries = %v, want %v", f.queries(), want)
	}
}

func TestReadThermalDoesNotWalkTheLadderOnAFailureThatIsNotARefusal(t *testing.T) {
	resetThermal(t)
	f := &thermalFake{reply: func(args []string) (string, error) { return transientOut, errTransient }}
	if rows, err := readThermalWith(context.Background(), f.run); err == nil {
		t.Fatalf("a failing nvidia-smi answered %+v, nil", rows)
	}
	if _, err := readThermalWith(context.Background(), f.run); err == nil {
		t.Fatal("a failing nvidia-smi answered on the second call")
	}
	// A transient fault is not the driver saying it lacks the field: one spawn per read, and nothing learned.
	if want := []string{argOld, argOld}; !reflect.DeepEqual(f.queries(), want) {
		t.Errorf("queries = %v, want %v", f.queries(), want)
	}
}

func TestReadThermalErrorsWhenEveryQueryIsRefused(t *testing.T) {
	resetThermal(t)
	f := &thermalFake{reply: func(args []string) (string, error) {
		switch args[0] {
		case argOld:
			return refusal("clocks_throttle_reasons.active"), errExit2
		case argNew:
			return refusal("clocks_event_reasons.active"), errExit2
		}
		return refusal("temperature.gpu"), errExit2
	}}
	if rows, err := readThermalWith(context.Background(), f.run); err == nil {
		t.Fatalf("readThermalWith = %+v, nil; want an error", rows)
	}
	if want := []string{argOld, argNew, argTemp}; !reflect.DeepEqual(f.queries(), want) {
		t.Errorf("queries = %v, want %v", f.queries(), want)
	}
}

func TestRefusesFieldNamesTheFieldItRefused(t *testing.T) {
	old := "clocks_throttle_reasons.active"
	if !refusesField(refusal(old), errExit2, old) {
		t.Error("the refusal text names the field and says it is not valid: that is a refusal")
	}
	if refusesField(refusal(old), errExit2, "clocks_event_reasons.active") {
		t.Error("a refusal of one field is not a refusal of another")
	}
	if refusesField(transientOut, errTransient, old) {
		t.Error("an unrelated failure is not a refusal")
	}
	if !refusesField("", errors.New("Field \""+old+"\" is not a valid field to query."), old) {
		t.Error("the text may arrive in the error instead of the output")
	}
}
