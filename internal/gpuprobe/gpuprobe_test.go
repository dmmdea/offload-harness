package gpuprobe

import (
	"strings"
	"testing"
)

// The fixtures below mirror the live Qube/<node-b> shapes the fleetnode parser
// was born from: two near-twin 16 GiB Blackwell cards at PCI index 0/1. The
// UUIDs are the real ones the composite tier's display-card pin names (the
// 5070 Ti, GPU-2a44…, is the display card that must never host a compute
// seat), so a FreeGiB lookup by UUID prefix is tested against the exact key
// the placement package will pass.
const (
	qube5060TiUUID = "GPU-3ee161b5-c188-495b-eaeb-291e6e6e1d97" // index 0, 16311 MiB total
	qube5070TiUUID = "GPU-2a44210f-6739-2d89-0e21-44cd5143faf7" // index 1, 16303 MiB total

	node5060TiUUID = "GPU-1111aaaa-2222-3333-4444-555566667777" // index 0, 16311 MiB total
	node5070TiUUID = "GPU-8888bbbb-9999-cccc-dddd-eeeeffff0000" // index 1, 16303 MiB total
)

// TestFreeGiBMatchesByIndexOrUUID pins the two key forms the placement guards
// use to find the display card: a bare nvidia-smi index ("1") and a UUID
// prefix ("GPU-2a44210f"). The Qube's live config pins the display card by
// UUID because the board reorders indices on power loss, so a UUID lookup
// must resolve to the same card an index lookup does — and an index that no
// device carries must read as "unknown", never as a number.
func TestFreeGiBMatchesByIndexOrUUID(t *testing.T) {
	devs, err := ParseSmiMemoryDevices("0, GPU-3ee161b5-c188-495b-eaeb-291e6e6e1d97, NVIDIA GeForce RTX 5060 Ti, 16311, 261, 0\n1, GPU-2a44210f-6739-2d89-0e21-44cd5143faf7, NVIDIA GeForce RTX 5070 Ti, 16303, 1498, 1\n")
	if err != nil {
		t.Fatal(err)
	}
	if f, ok := FreeGiB(devs, "1"); !ok || f < 14.4 || f > 14.5 {
		t.Fatalf("by index: %v %v", f, ok)
	}
	if f, ok := FreeGiB(devs, "GPU-2a44210f"); !ok || f < 14.4 {
		t.Fatalf("by uuid prefix: %v %v", f, ok)
	}
	if _, ok := FreeGiB(devs, "7"); ok {
		t.Fatal("unknown index must not read as a number")
	}
}

// TestFreeGiBKeyEdgeCases pins the lookup rules a caller could get wrong:
// the full UUID matches (a prefix of itself), the match is case-insensitive
// (SelectHeadlineDevice already compares UUIDs with EqualFold, so the two
// lookups must agree), surrounding whitespace on the key is tolerated (config
// values are hand-typed), and an empty key or an empty device list is
// "unknown" — a guard that reads unknown as 0 GiB would refuse every load,
// one that reads it as infinite would admit onto a card it cannot see, so the
// bool is the only honest answer.
func TestFreeGiBKeyEdgeCases(t *testing.T) {
	devs, err := ParseSmiMemoryDevices("0, " + qube5060TiUUID + ", NVIDIA GeForce RTX 5060 Ti, 16311, 261\n" +
		"1, " + qube5070TiUUID + ", NVIDIA GeForce RTX 5070 Ti, 16303, 1498\n")
	if err != nil {
		t.Fatal(err)
	}
	if f, ok := FreeGiB(devs, qube5070TiUUID); !ok || f < 14.4 || f > 14.5 {
		t.Fatalf("full uuid: %v %v", f, ok)
	}
	if f, ok := FreeGiB(devs, strings.ToLower("GPU-2A44210F")); !ok || f < 14.4 {
		t.Fatalf("lower-case uuid prefix: %v %v", f, ok)
	}
	if f, ok := FreeGiB(devs, "GPU-2A44210F"); !ok || f < 14.4 {
		t.Fatalf("upper-case uuid prefix: %v %v", f, ok)
	}
	if f, ok := FreeGiB(devs, " 0 "); !ok || f < 15.6 || f > 15.7 {
		t.Fatalf("padded index: %v %v", f, ok)
	}
	if _, ok := FreeGiB(devs, ""); ok {
		t.Fatal("empty key must be unknown")
	}
	if _, ok := FreeGiB(devs, "GPU-"); ok {
		t.Fatal("a prefix shared by every card is ambiguous and must be unknown")
	}
	if _, ok := FreeGiB(nil, "0"); ok {
		t.Fatal("no devices must be unknown")
	}
}

// TestParseSmiMemoryDevices_MultiDevice locks the multi-device parse: EVERY
// line becomes a device, in nvidia-smi's own enumeration order, and the case
// that shipped the original bug — the largest card is NOT first — must not
// silently collapse back to "first line wins".
func TestParseSmiMemoryDevices_MultiDevice(t *testing.T) {
	in := "0, GPU-aaaaaaaa-0000-0000-0000-000000000000, NVIDIA GeForce RTX 3070, 8192, 1024\n" +
		"1, GPU-bbbbbbbb-0000-0000-0000-000000000000, NVIDIA GeForce RTX 4090, 24576, 2048\n"
	devices, err := ParseSmiMemoryDevices(in)
	if err != nil {
		t.Fatalf("ParseSmiMemoryDevices: %v", err)
	}
	if len(devices) != 2 {
		t.Fatalf("got %d devices, want 2: %+v", len(devices), devices)
	}
	want := []Device{
		{Index: 0, UUID: "GPU-aaaaaaaa-0000-0000-0000-000000000000", Name: "NVIDIA GeForce RTX 3070", TotalGiB: 8192.0 / 1024, FreeGiB: (8192.0 - 1024) / 1024},
		{Index: 1, UUID: "GPU-bbbbbbbb-0000-0000-0000-000000000000", Name: "NVIDIA GeForce RTX 4090", TotalGiB: 24576.0 / 1024, FreeGiB: (24576.0 - 2048) / 1024},
	}
	for i, d := range devices {
		if d != want[i] {
			t.Errorf("devices[%d] = %+v, want %+v", i, d, want[i])
		}
	}
	head := HeadlineDevice(devices)
	if head.Index != 1 || head.Name != "NVIDIA GeForce RTX 4090" {
		t.Fatalf("HeadlineDevice = %+v, want index 1 (the LARGER card, not the first line)", head)
	}
}

// TestParseSmiMemoryDevices_NodeBShape locks the exact live-verified <node-b>
// numbers: two near-twin 16 GiB Blackwell cards where index 0 has 8 MiB more
// raw total (16311 > 16303, a per-SKU driver reserve, not a real capacity
// difference), so the documented largest-total rule picks index 0 — a
// deliberate, explicit outcome, not an accident of enumeration order.
func TestParseSmiMemoryDevices_NodeBShape(t *testing.T) {
	in := "0, " + node5060TiUUID + ", NVIDIA GeForce RTX 5060 Ti, 16311, 867\n" +
		"1, " + node5070TiUUID + ", NVIDIA GeForce RTX 5070 Ti, 16303, 2187\n"
	devices, err := ParseSmiMemoryDevices(in)
	if err != nil {
		t.Fatalf("ParseSmiMemoryDevices: %v", err)
	}
	if len(devices) != 2 {
		t.Fatalf("got %d devices, want 2: %+v", len(devices), devices)
	}
	if devices[0].Index != 0 || devices[0].UUID != node5060TiUUID || devices[0].Name != "NVIDIA GeForce RTX 5060 Ti" {
		t.Errorf("devices[0] = %+v", devices[0])
	}
	if devices[1].Index != 1 || devices[1].UUID != node5070TiUUID || devices[1].Name != "NVIDIA GeForce RTX 5070 Ti" {
		t.Errorf("devices[1] = %+v", devices[1])
	}
	head := HeadlineDevice(devices)
	if head.Index != 0 {
		t.Fatalf("HeadlineDevice = %+v, want index 0 (16311 MiB > 16303 MiB total — the documented largest-total rule)", head)
	}
}

// TestParseSmiMemoryDevicesSingleLine is the single-GPU regression guard: one
// device line, headline == that device.
func TestParseSmiMemoryDevicesSingleLine(t *testing.T) {
	devices, err := ParseSmiMemoryDevices("0, " + node5060TiUUID + ", NVIDIA GeForce RTX 3070, 8192, 1024\n")
	if err != nil {
		t.Fatalf("ParseSmiMemoryDevices: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("got %d devices, want 1: %+v", len(devices), devices)
	}
	head := HeadlineDevice(devices)
	if head.TotalGiB != 8.0 || head.FreeGiB != 7.0 {
		t.Fatalf("HeadlineDevice = %+v, want Total 8 Free 7", head)
	}
}

// TestParseSmiMemoryDevicesMalformedLinesSkipped locks the no-panic contract:
// malformed/blank lines interleaved with valid ones are skipped, not fatal —
// one garbled row must not take down every other card's reading.
func TestParseSmiMemoryDevicesMalformedLinesSkipped(t *testing.T) {
	in := "\n0, " + node5060TiUUID + ", NVIDIA GeForce RTX 3070, 8192, 1024\n" +
		"not,a,valid,line,at,all,extra\n\n" +
		"1, " + node5070TiUUID + ", NVIDIA GeForce RTX 4090, 24576, 2048\ngarbage\n"
	devices, err := ParseSmiMemoryDevices(in)
	if err != nil {
		t.Fatalf("ParseSmiMemoryDevices: %v", err)
	}
	if len(devices) != 2 {
		t.Fatalf("got %d devices, want 2 (malformed/blank lines skipped): %+v", len(devices), devices)
	}
	if devices[0].Index != 0 || devices[1].Index != 1 {
		t.Fatalf("devices out of order or wrong: %+v", devices)
	}
}

// TestParseSmiMemoryDevicesAllInvalidIsError locks the failed-probe contract:
// if nothing valid parses, that is an error — vram_total_gb <= 0 must never
// leak through as a publishable snapshot.
func TestParseSmiMemoryDevicesAllInvalidIsError(t *testing.T) {
	cases := []string{
		"",
		"\n\n\n",
		"garbage\nnot,valid\n",
		"0, " + node5060TiUUID + ", GPU, 0, 0\n",
	}
	for _, in := range cases {
		if _, err := ParseSmiMemoryDevices(in); err == nil {
			t.Errorf("ParseSmiMemoryDevices(%q) = nil error, want error (no valid device lines)", in)
		}
	}
}

// TestParseSmiMemoryDevicesUsedExceedsTotalSkipped locks the corruption guard:
// used > total is impossible for a real card and is SKIPPED, never clamped to
// "0 free" and published as a plausible full card.
func TestParseSmiMemoryDevicesUsedExceedsTotalSkipped(t *testing.T) {
	in := "0, " + node5060TiUUID + ", NVIDIA GeForce RTX 5060 Ti, 16311, 867\n" +
		"1, " + node5070TiUUID + ", NVIDIA GeForce RTX 5070 Ti, 16303, 99999\n"
	devices, err := ParseSmiMemoryDevices(in)
	if err != nil {
		t.Fatalf("ParseSmiMemoryDevices: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("got %d devices, want 1 (the used>total line must be skipped, not clamped): %+v", len(devices), devices)
	}
	if devices[0].Index != 0 {
		t.Fatalf("surviving device = %+v, want the valid index-0 line", devices[0])
	}
	allBad := "0, " + node5060TiUUID + ", GPU, 100, 200\n1, " + node5070TiUUID + ", GPU, 50, 999\n"
	if _, err := ParseSmiMemoryDevices(allBad); err == nil {
		t.Errorf("ParseSmiMemoryDevices(all used>total) = nil error, want error (no valid device lines)")
	}
}

// TestParseSmiMemoryDevicesCRLF locks Windows CRLF tolerance across multiple
// device lines (nvidia-smi emits \r\n on Windows).
func TestParseSmiMemoryDevicesCRLF(t *testing.T) {
	in := "0, " + node5060TiUUID + ", NVIDIA GeForce RTX 3070, 8192, 1024\r\n" +
		"1, " + node5070TiUUID + ", NVIDIA GeForce RTX 4090, 24576, 2048\r\n"
	devices, err := ParseSmiMemoryDevices(in)
	if err != nil {
		t.Fatalf("ParseSmiMemoryDevices: %v", err)
	}
	if len(devices) != 2 {
		t.Fatalf("got %d devices, want 2: %+v", len(devices), devices)
	}
	if devices[0].Name != "NVIDIA GeForce RTX 3070" || devices[1].Name != "NVIDIA GeForce RTX 4090" {
		t.Fatalf("CRLF not stripped from name field: %+v", devices)
	}
}

// TestParseSmiMemoryDevices_SixFieldsCarriesUtilization: the optional 6th
// field (utilization.gpu) lands on the device and marks UtilKnown.
func TestParseSmiMemoryDevices_SixFieldsCarriesUtilization(t *testing.T) {
	out := "0, GPU-aaaa, NVIDIA GeForce RTX 5070 Ti, 16303, 2456, 37\r\n" +
		"1, GPU-bbbb, NVIDIA GeForce RTX 5060 Ti, 16311, 14100, 0\r\n"
	devs, err := ParseSmiMemoryDevices(out)
	if err != nil {
		t.Fatal(err)
	}
	if devs[0].UtilPct != 37 || devs[1].UtilPct != 0 {
		t.Fatalf("util: got %d,%d want 37,0", devs[0].UtilPct, devs[1].UtilPct)
	}
	if !devs[0].UtilKnown || !devs[1].UtilKnown {
		t.Fatal("6-field line must mark UtilKnown")
	}
}

// TestParseSmiMemoryDevices_FiveFieldsStillParses: an older 5-field query
// still yields the device, with utilization UNKNOWN (never "0 % = idle").
func TestParseSmiMemoryDevices_FiveFieldsStillParses(t *testing.T) {
	out := "0, GPU-aaaa, NVIDIA GeForce RTX 3050, 6144, 1024\n"
	devs, err := ParseSmiMemoryDevices(out)
	if err != nil {
		t.Fatal(err)
	}
	if devs[0].UtilKnown || devs[0].UtilPct != 0 {
		t.Fatal("5-field line must leave util UNKNOWN")
	}
}

// TestParseSmiMemoryDevices_MalformedUtilizationKeptWithValidMemory: a
// malformed or out-of-range 6th field must NOT drop the device — the memory
// figures are too valuable to lose over a transient utilization failure.
func TestParseSmiMemoryDevices_MalformedUtilizationKeptWithValidMemory(t *testing.T) {
	out := "0, GPU-aaaa, NVIDIA GeForce RTX 5070 Ti, 16303, 2456, [Not Supported]\r\n" +
		"1, GPU-bbbb, NVIDIA GeForce RTX 5060 Ti, 16311, 14100, 150\r\n"
	devs, err := ParseSmiMemoryDevices(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 2 {
		t.Fatalf("got %d devices, want 2 (both kept despite bad util)", len(devs))
	}
	if devs[0].UtilKnown || devs[0].UtilPct != 0 {
		t.Errorf("device 0: UtilKnown=%v UtilPct=%d, want false/0 (malformed field)", devs[0].UtilKnown, devs[0].UtilPct)
	}
	if devs[0].TotalGiB == 0 || devs[0].FreeGiB == 0 {
		t.Errorf("device 0 memory lost: TotalGiB=%v FreeGiB=%v", devs[0].TotalGiB, devs[0].FreeGiB)
	}
	if devs[1].UtilKnown || devs[1].UtilPct != 0 {
		t.Errorf("device 1: UtilKnown=%v UtilPct=%d, want false/0 (out of range)", devs[1].UtilKnown, devs[1].UtilPct)
	}
	if devs[1].TotalGiB == 0 || devs[1].FreeGiB == 0 {
		t.Errorf("device 1 memory lost: TotalGiB=%v FreeGiB=%v", devs[1].TotalGiB, devs[1].FreeGiB)
	}
}

// TestReadUsesTheInjectedRunnerAndHonoursItsError pins Read's two contracts:
// it parses whatever the runner returns (so a test never needs nvidia-smi on
// PATH) and a runner failure is returned, never swallowed into an empty
// device list that a guard could read as "no card is busy".
func TestReadUsesTheInjectedRunnerAndHonoursItsError(t *testing.T) {
	devs, err := ReadWith(func() (string, error) {
		return "0, " + qube5060TiUUID + ", NVIDIA GeForce RTX 5060 Ti, 16311, 261\n", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 1 || devs[0].UUID != qube5060TiUUID {
		t.Fatalf("devices = %+v", devs)
	}
	if _, err := ReadWith(func() (string, error) { return "", errExecFailed }); err == nil {
		t.Fatal("a runner error must surface as an error")
	}
	if _, err := ReadWith(nil); err == nil {
		t.Fatal("a nil runner must be an error, not a nil-deref or an empty success")
	}
}

// TestHostFreeRAMGiBIsPlausibleWhereSupported: on the platforms that have a
// reader (Windows, Linux) the figure is a positive number no larger than a
// sane physical ceiling; where there is no reader the bool is false. Either
// way a guard gets a truthful answer and never a silent zero.
func TestHostFreeRAMGiBIsPlausibleWhereSupported(t *testing.T) {
	free, ok := HostFreeRAMGiB()
	if !ok {
		if hostRAMSupported {
			t.Fatal("this platform has a reader but it reported unknown")
		}
		return
	}
	if free <= 0 || free > 16384 {
		t.Fatalf("free host RAM = %v GiB, not plausible", free)
	}
}
