package fleetnode

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestParseSmiMemory locks the nvidia-smi CSV parse: MiB in, GiB out, tolerant
// of whitespace and CRLF (nvidia-smi on Windows emits \r\n). A total <= 0 is an
// error — the contract treats vram_total_gb <= 0 as a failed probe, so the
// parser refuses to produce a zero-total snapshot rather than letting one leak
// into /fleet/health.
func TestParseSmiMemory(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantTotal float64
		wantUsed  float64
		wantErr   bool
	}{
		{"plain", "16384, 1234", 16.0, 1234.0 / 1024, false},
		{"crlf", "16384, 1234\r\n", 16.0, 1234.0 / 1024, false},
		{"lf", "16384, 2048\n", 16.0, 2.0, false},
		{"extra whitespace", "  16384 ,  512  ", 16.0, 0.5, false},
		{"zero used", "8192, 0", 8.0, 0, false},
		{"multi-gpu takes first line", "16384, 1024\r\n8192, 512\r\n", 16.0, 1.0, false},
		{"empty", "", 0, 0, true},
		{"one field", "16384", 0, 0, true},
		{"three fields", "16384, 1, 2", 0, 0, true},
		{"non-numeric total", "N/A, 1234", 0, 0, true},
		{"non-numeric used", "16384, [N/A]", 0, 0, true},
		{"zero total refused", "0, 0", 0, 0, true},
		{"negative used refused", "16384, -5", 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			total, used, err := ParseSmiMemory(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseSmiMemory(%q) = (%v, %v, nil), want error", tc.in, total, used)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSmiMemory(%q) error: %v", tc.in, err)
			}
			if total != tc.wantTotal || used != tc.wantUsed {
				t.Errorf("ParseSmiMemory(%q) = (%v, %v), want (%v, %v)", tc.in, total, used, tc.wantTotal, tc.wantUsed)
			}
		})
	}
}

// Real <node-b> UUIDs (per the coordinator's UUID-pin follow-up, live-verified
// 2026-08-04) — reused across the multi-device AND the UUID-pin tests so the
// fixtures stay honest to the actual hardware that shipped this bug.
const (
	node5060TiUUID = "GPU-1111aaaa-2222-3333-4444-555566667777" // index 0, 16311 MiB total
	node5070TiUUID = "GPU-8888bbbb-9999-cccc-dddd-eeeeffff0000" // index 1, 16303 MiB total
)

// TestParseSmiMemoryDevices_MultiDevice locks the multi-device parse this bug
// fix adds: EVERY line becomes a device, in nvidia-smi's own enumeration
// order, and the case that actually shipped the bug — the largest card is
// NOT first — must not silently collapse back to "first line wins".
func TestParseSmiMemoryDevices_MultiDevice(t *testing.T) {
	// index 0 is the SMALLER card; index 1 is the LARGER card. The old
	// first-line-wins code would headline index 0 — this fix must not.
	in := "0, GPU-aaaaaaaa-0000-0000-0000-000000000000, NVIDIA GeForce RTX 3070, 8192, 1024\n" +
		"1, GPU-bbbbbbbb-0000-0000-0000-000000000000, NVIDIA GeForce RTX 4090, 24576, 2048\n"
	devices, err := ParseSmiMemoryDevices(in)
	if err != nil {
		t.Fatalf("ParseSmiMemoryDevices: %v", err)
	}
	if len(devices) != 2 {
		t.Fatalf("got %d devices, want 2: %+v", len(devices), devices)
	}
	want := []GPUDevice{
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
// numbers: two near-twin 16 GiB Blackwell cards where nvidia-smi index 0 (the
// RTX 5060 Ti, the auxiliary/donor card) is NOT the compute card — the RTX
// 5070 Ti at index 1 is, per ComfyUI's /system_stats (CUDA FASTEST_FIRST
// ordering binds cuda:0 to the 5070 Ti). nvidia-smi carries no signal that
// exposes CUDA device order, so HeadlineDevice cannot "know" which card
// ComfyUI will actually use — it applies the documented, deterministic
// largest-total rule instead. For THESE exact numbers, index 0 has marginally
// more total memory (16311 > 16303 MiB — an 8 MiB gap from per-SKU
// driver/firmware reserve, not a real capacity difference), so it wins on the
// "largest total" branch, not the tie-break branch. This test exists to make
// that outcome explicit and intentional, not accidental: the fix's honesty is
// in gpu_devices carrying BOTH cards' real numbers, not in the headline pick
// being "correct" for near-twin hardware — see HeadlineDevice's doc comment.
// (The deterministic fix for THIS box is primary_gpu_uuid — see
// TestSelectHeadlineDevice_PinnedUUIDBeatsLargerTotal below.)
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
	// Deliberate, documented tie-break outcome: index 0 wins by 8 MiB of raw
	// total — NOT because it is (or is believed to be) the compute card.
	if head.Index != 0 {
		t.Fatalf("HeadlineDevice = %+v, want index 0 (16311 MiB > 16303 MiB total — the documented largest-total rule)", head)
	}
}

// TestSelectHeadlineDevice_PinnedUUIDBeatsLargerTotal is the actual fix for
// <node-b>: pinning primary_gpu_uuid to the RTX 5070 Ti (the real compute card,
// smaller total by 8 MiB) must win over the RTX 5060 Ti even though
// HeadlineDevice alone would pick the 5060 Ti (see the NodeBShape test above).
// This is the deterministic override the largest-total rule cannot provide on
// near-twin hardware.
func TestSelectHeadlineDevice_PinnedUUIDBeatsLargerTotal(t *testing.T) {
	devices := []GPUDevice{
		{Index: 0, UUID: node5060TiUUID, Name: "NVIDIA GeForce RTX 5060 Ti", TotalGiB: 16311.0 / 1024, FreeGiB: (16311.0 - 867) / 1024},
		{Index: 1, UUID: node5070TiUUID, Name: "NVIDIA GeForce RTX 5070 Ti", TotalGiB: 16303.0 / 1024, FreeGiB: (16303.0 - 2187) / 1024},
	}
	head, warning := SelectHeadlineDevice(devices, node5070TiUUID)
	if warning != "" {
		t.Fatalf("warning = %q, want none (UUID was found)", warning)
	}
	if head.Index != 1 || head.UUID != node5070TiUUID || head.Name != "NVIDIA GeForce RTX 5070 Ti" {
		t.Fatalf("SelectHeadlineDevice = %+v, want the pinned 5070 Ti (index 1), not the larger-total 5060 Ti", head)
	}
}

// TestSelectHeadlineDevice_UUIDNotFoundFallsBackAndWarns locks the
// typo/removed-card safety net: a pinned UUID absent from the parsed devices
// falls back to HeadlineDevice's largest-total rule AND returns a non-empty
// warning naming the missing UUID — never a silent fallback.
func TestSelectHeadlineDevice_UUIDNotFoundFallsBackAndWarns(t *testing.T) {
	devices := []GPUDevice{
		{Index: 0, UUID: node5060TiUUID, Name: "NVIDIA GeForce RTX 5060 Ti", TotalGiB: 16311.0 / 1024, FreeGiB: 15},
		{Index: 1, UUID: node5070TiUUID, Name: "NVIDIA GeForce RTX 5070 Ti", TotalGiB: 16303.0 / 1024, FreeGiB: 13},
	}
	const typoUUID = "GPU-00000000-0000-0000-0000-000000000000"
	head, warning := SelectHeadlineDevice(devices, typoUUID)
	if warning == "" || !strings.Contains(warning, typoUUID) {
		t.Fatalf("warning = %q, want a non-empty warning naming %q", warning, typoUUID)
	}
	want := HeadlineDevice(devices)
	if head != want {
		t.Fatalf("SelectHeadlineDevice fallback = %+v, want HeadlineDevice's pick %+v", head, want)
	}
}

// TestSelectHeadlineDevice_UnsetUUIDUnchanged locks the default (no pin):
// identical to HeadlineDevice alone, no warning — today's behavior verbatim.
func TestSelectHeadlineDevice_UnsetUUIDUnchanged(t *testing.T) {
	devices := []GPUDevice{
		{Index: 0, UUID: node5060TiUUID, Name: "small-total-wise-bigger", TotalGiB: 16311.0 / 1024, FreeGiB: 15},
		{Index: 1, UUID: node5070TiUUID, Name: "5070 Ti", TotalGiB: 16303.0 / 1024, FreeGiB: 13},
	}
	head, warning := SelectHeadlineDevice(devices, "")
	if warning != "" {
		t.Fatalf("warning = %q, want none when primary_gpu_uuid is unset", warning)
	}
	if want := HeadlineDevice(devices); head != want {
		t.Fatalf("SelectHeadlineDevice(unset) = %+v, want HeadlineDevice's pick %+v", head, want)
	}
}

// TestSelectHeadlineDevice_PinnedUUIDOnSingleGPUBox locks the trivial case: a
// pin that matches the box's only device just works, same as no pin would.
func TestSelectHeadlineDevice_PinnedUUIDOnSingleGPUBox(t *testing.T) {
	devices := []GPUDevice{{Index: 0, UUID: node5060TiUUID, Name: "solo", TotalGiB: 16, FreeGiB: 15}}
	head, warning := SelectHeadlineDevice(devices, node5060TiUUID)
	if warning != "" {
		t.Fatalf("warning = %q, want none", warning)
	}
	if head != devices[0] {
		t.Fatalf("SelectHeadlineDevice = %+v, want the sole device %+v", head, devices[0])
	}
}

// TestSelectHeadlineDevice_CaseInsensitiveUUIDMatch locks the second half of
// the copy-paste safety net (the first half, whitespace trimming, is
// config.Load's job — internal/config/config_test.go
// TestLoadTrimsPrimaryGPUUUID). nvidia-smi emits UUIDs lowercase, but not
// every tool an operator might copy one from does — a differently-cased but
// otherwise identical UUID must still match, not silently fall back with a
// warning.
func TestSelectHeadlineDevice_CaseInsensitiveUUIDMatch(t *testing.T) {
	devices := []GPUDevice{
		{Index: 0, UUID: node5060TiUUID, Name: "RTX 5060 Ti", TotalGiB: 16311.0 / 1024, FreeGiB: 15},
		{Index: 1, UUID: node5070TiUUID, Name: "RTX 5070 Ti", TotalGiB: 16303.0 / 1024, FreeGiB: 13},
	}
	uppered := strings.ToUpper(node5070TiUUID)
	head, warning := SelectHeadlineDevice(devices, uppered)
	if warning != "" {
		t.Fatalf("warning = %q, want none — %q should case-insensitively match %q", warning, uppered, node5070TiUUID)
	}
	if head.Index != 1 || head.UUID != node5070TiUUID {
		t.Fatalf("SelectHeadlineDevice = %+v, want the pinned 5070 Ti (case-insensitive match)", head)
	}
}

// TestParseSmiMemoryDevicesSingleLine is the regression guard: a single
// device line behaves exactly like today's single-GPU box — one device,
// headline == that device.
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
// a single garbled row (a transient nvidia-smi hiccup) must not take down
// every other card's reading.
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

// TestParseSmiMemoryDevicesAllInvalidIsError locks the contract: if every
// line is malformed/blank/zero-total (nothing valid parses), that is the same
// class of failure as an empty probe — vram_total_gb <= 0 must never leak
// through as a publishable snapshot.
func TestParseSmiMemoryDevicesAllInvalidIsError(t *testing.T) {
	cases := []string{
		"",
		"\n\n\n",
		"garbage\nnot,valid\n",
		"0, " + node5060TiUUID + ", GPU, 0, 0\n", // total <= 0 is a failed probe, per device too
	}
	for _, in := range cases {
		if _, err := ParseSmiMemoryDevices(in); err == nil {
			t.Errorf("ParseSmiMemoryDevices(%q) = nil error, want error (no valid device lines)", in)
		}
	}
}

// TestParseSmiMemoryDevicesUsedExceedsTotalSkipped locks the corruption guard:
// a device line reporting more used than total memory is impossible for a
// real card and must be SKIPPED — not clamped to 0 free and published as if
// it were a legitimate "card is full" reading. A skipped device must not take
// down its neighbors on the same box (mirrors the malformed-line contract).
func TestParseSmiMemoryDevicesUsedExceedsTotalSkipped(t *testing.T) {
	in := "0, " + node5060TiUUID + ", NVIDIA GeForce RTX 5060 Ti, 16311, 867\n" + // valid
		"1, " + node5070TiUUID + ", NVIDIA GeForce RTX 5070 Ti, 16303, 99999\n" // used > total: corrupt
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

	// If EVERY line is used>total, that is zero valid devices — same failed-
	// probe contract as any other all-invalid input.
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

// TestStartDeviceProbeSampler locks the sampler wiring: an injected
// DeviceProbe publishes a Snapshot whose TotalGiB/FreeGiB come from
// HeadlineDevice (not enumeration order) and whose Devices carries every
// parsed card.
func TestStartDeviceProbeSampler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := func() ([]GPUDevice, error) {
		return []GPUDevice{
			{Index: 0, Name: "small", TotalGiB: 8, FreeGiB: 7},
			{Index: 1, Name: "big", TotalGiB: 16, FreeGiB: 2},
		}, nil
	}
	s := StartDeviceProbeSampler(ctx, 10*time.Millisecond, probe, "") // no UUID pin
	snap, ok := s.Load()
	if !ok {
		t.Fatal("Load() not ok after start")
	}
	if snap.TotalGiB != 16 || snap.FreeGiB != 2 {
		t.Errorf("headline = (%v, %v), want (16, 2) — the LARGER device, not index 0", snap.TotalGiB, snap.FreeGiB)
	}
	if len(snap.Devices) != 2 {
		t.Fatalf("Devices = %+v, want 2 entries", snap.Devices)
	}
}

// TestStartDeviceProbeSamplerPinnedUUID locks the end-to-end sampler wiring
// for primary_gpu_uuid: pinning the SMALLER device by UUID must still
// headline it over the larger-total device — the same <node-b>-shaped override
// TestSelectHeadlineDevice_PinnedUUIDBeatsLargerTotal proves at the pure-
// function level, now proven through the actual sampler goroutine.
func TestStartDeviceProbeSamplerPinnedUUID(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := func() ([]GPUDevice, error) {
		return []GPUDevice{
			{Index: 0, UUID: node5060TiUUID, Name: "RTX 5060 Ti", TotalGiB: 16311.0 / 1024, FreeGiB: 15},
			{Index: 1, UUID: node5070TiUUID, Name: "RTX 5070 Ti", TotalGiB: 16303.0 / 1024, FreeGiB: 13},
		}, nil
	}
	s := StartDeviceProbeSampler(ctx, 10*time.Millisecond, probe, node5070TiUUID)
	snap, ok := s.Load()
	if !ok {
		t.Fatal("Load() not ok after start")
	}
	if snap.TotalGiB != 16303.0/1024 || snap.FreeGiB != 13 {
		t.Errorf("headline = (%v, %v), want the PINNED 5070 Ti's numbers (%v, 13), not the larger-total 5060 Ti", snap.TotalGiB, snap.FreeGiB, 16303.0/1024)
	}
}

// TestStartDeviceProbeSamplerErrorKeepsLast mirrors
// TestStartGlobalSamplerErrorKeepsLast for the device-aware sampler: a
// failing probe must never publish zeros or an empty snapshot.
func TestStartDeviceProbeSamplerErrorKeepsLast(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	fail := false
	calls := 0
	probe := func() ([]GPUDevice, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if fail {
			return nil, errors.New("nvidia-smi exploded")
		}
		return []GPUDevice{{Index: 0, Name: "only", TotalGiB: 16, FreeGiB: 15}}, nil
	}
	s := StartDeviceProbeSampler(ctx, 10*time.Millisecond, probe, "")
	if _, ok := s.Load(); !ok {
		t.Fatal("initial sample should have published")
	}
	mu.Lock()
	fail = true
	base := calls
	mu.Unlock()
	if !waitFor(t, 2*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return calls >= base+2 }) {
		t.Fatal("sampler stopped sampling after errors")
	}
	snap, ok := s.Load()
	if !ok || snap.TotalGiB != 16 || snap.FreeGiB != 15 {
		t.Errorf("after probe failures snapshot = %+v ok=%v, want last good (16, 15) true", snap, ok)
	}
}

// waitFor polls until cond() or the deadline. Sampler tests are timing-based by
// nature (goroutine + ticker); the injected runner keeps them GPU-free.
func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// TestStartGlobalSampler locks the snapshot pipeline: injected runner output is
// parsed and published (Free = Total - Used), Load reports ok, and the sampler
// keeps refreshing on its interval.
func TestStartGlobalSampler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	calls := 0
	run := func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return "16384, 4096", nil
	}
	s := StartGlobalSampler(ctx, 10*time.Millisecond, run)

	snap, ok := s.Load()
	if !ok {
		t.Fatal("Load() not ok after start — the initial sample must publish before StartGlobalSampler returns")
	}
	if snap.TotalGiB != 16.0 || snap.FreeGiB != 12.0 {
		t.Errorf("snapshot = %+v, want Total 16 Free 12", snap)
	}
	if snap.At.IsZero() {
		t.Error("snapshot At is zero, want a real timestamp")
	}

	if !waitFor(t, 2*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return calls >= 3 }) {
		t.Fatalf("sampler did not keep sampling: %d calls", calls)
	}
}

// TestStartGlobalSamplerErrorKeepsLast locks the never-emit-zeros rule: runner
// errors and unparseable output leave the last good snapshot in place, and a
// sampler that has never succeeded reports !ok rather than a zero snapshot.
func TestStartGlobalSamplerErrorKeepsLast(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	fail := false
	calls := 0
	run := func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if fail {
			return "", errors.New("nvidia-smi exploded")
		}
		return "16384, 1024", nil
	}
	s := StartGlobalSampler(ctx, 10*time.Millisecond, run)
	if _, ok := s.Load(); !ok {
		t.Fatal("initial sample should have published")
	}
	mu.Lock()
	fail = true
	base := calls
	mu.Unlock()
	if !waitFor(t, 2*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return calls >= base+2 }) {
		t.Fatal("sampler stopped sampling after errors")
	}
	snap, ok := s.Load()
	if !ok || snap.TotalGiB != 16.0 || snap.FreeGiB != 15.0 {
		t.Errorf("after runner failures snapshot = %+v ok=%v, want last good (16, 15) true", snap, ok)
	}
}

// TestStartGlobalSamplerNeverOK locks the all-failures path: Load stays !ok.
func TestStartGlobalSamplerNeverOK(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := StartGlobalSampler(ctx, 10*time.Millisecond, func() (string, error) {
		return "", errors.New("no GPU")
	})
	if snap, ok := s.Load(); ok {
		t.Errorf("Load() = %+v ok=true, want !ok when no sample ever succeeded", snap)
	}
}

// TestStartGlobalSamplerStops locks ctx cancellation: no further runner calls
// after cancel (modulo one in-flight tick).
func TestStartGlobalSamplerStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	calls := 0
	run := func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return "16384, 1024", nil
	}
	StartGlobalSampler(ctx, 5*time.Millisecond, run)
	cancel()
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	after := calls
	mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	final := calls
	mu.Unlock()
	if final > after+1 { // allow at most one racing tick around cancel
		t.Errorf("sampler kept running after cancel: %d -> %d calls", after, final)
	}
}
