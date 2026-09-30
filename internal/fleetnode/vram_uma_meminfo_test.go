package fleetnode

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// meminfoBody is a /proc/meminfo lookalike carrying the two figures the provider
// reads, with the lines a real one interleaves between them. The values are the
// reference board's own (7.7 GiB of LPDDR4X with the host's stack already up):
// 8,112,688 kB total, 6,502,340 kB available.
func meminfoBody(totalKiB, availKiB int64) string {
	return "MemTotal:        " + strconv.FormatInt(totalKiB, 10) + " kB\n" +
		"MemFree:          3211588 kB\n" +
		"MemAvailable:     " + strconv.FormatInt(availKiB, 10) + " kB\n" +
		"Buffers:           238340 kB\n" +
		"Cached:           3103484 kB\n"
}

// fakeProc writes body to <root>/proc/meminfo and returns the root.
func fakeProc(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "proc", "meminfo"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

const (
	refTotalKiB = 8112688
	refAvailKiB = 6502340
	kibPerGiB   = 1 << 20
)

func TestMeminfoUMAProbe_ReserveComesOffBothTotalAndFree(t *testing.T) {
	total, used, err := MeminfoUMAProbe(fakeProc(t, meminfoBody(refTotalKiB, refAvailKiB)), 3)()
	if err != nil {
		t.Fatal(err)
	}
	wantTotal := float64(refTotalKiB)/kibPerGiB - 3
	wantFree := float64(refAvailKiB)/kibPerGiB - 3
	if abs(total-wantTotal) > 1e-9 {
		t.Errorf("total = %v GiB, want MemTotal - reserve = %v", total, wantTotal)
	}
	if abs(used-(wantTotal-wantFree)) > 1e-9 {
		t.Errorf("used = %v GiB, want total - free = %v", used, wantTotal-wantFree)
	}
	// The advertised budget must sit clear of the whole box: this is the number the
	// dispatcher places against, and MemTotal would hand the host's share to inference.
	if total >= float64(refTotalKiB)/kibPerGiB {
		t.Errorf("total = %v GiB is not below MemTotal — the reserve was ignored", total)
	}
}

func TestMeminfoUMAProbe_ZeroReserveAdvertisesTheWholeBox(t *testing.T) {
	total, used, err := MeminfoUMAProbe(fakeProc(t, meminfoBody(refTotalKiB, refAvailKiB)), 0)()
	if err != nil {
		t.Fatal(err)
	}
	if abs(total-float64(refTotalKiB)/kibPerGiB) > 1e-9 {
		t.Errorf("total = %v, want all of MemTotal", total)
	}
	// With nothing reserved, used is simply what the kernel says is not available.
	if want := float64(refTotalKiB-refAvailKiB) / kibPerGiB; abs(used-want) > 1e-9 {
		t.Errorf("used = %v, want MemTotal - MemAvailable = %v", used, want)
	}
}

// A reserve larger than what is available is the normal state of a busy host, not an
// error: nothing is free for inference, so free clamps to 0 and used reads as the
// whole budget. Advertising a negative free would be a lie the dispatcher believes.
func TestMeminfoUMAProbe_ReserveAboveMemAvailableClampsFreeToZero(t *testing.T) {
	// 2 GiB available against a 3 GiB reserve.
	total, used, err := MeminfoUMAProbe(fakeProc(t, meminfoBody(refTotalKiB, 2*kibPerGiB)), 3)()
	if err != nil {
		t.Fatal(err)
	}
	if abs(total-(float64(refTotalKiB)/kibPerGiB-3)) > 1e-9 {
		t.Errorf("total = %v, want it unaffected by how much is free", total)
	}
	if abs(used-total) > 1e-9 {
		t.Errorf("used = %v, want the whole budget (%v): free must clamp to 0, not go negative", used, total)
	}
}

// MemAvailable above MemTotal cannot be real; it must not read as spare capacity.
func TestMeminfoUMAProbe_FreeNeverExceedsTotal(t *testing.T) {
	total, used, err := MeminfoUMAProbe(fakeProc(t, meminfoBody(refTotalKiB, refTotalKiB+kibPerGiB)), 0)()
	if err != nil {
		t.Fatal(err)
	}
	if used != 0 || total <= 0 {
		t.Errorf("total/used = %v/%v, want a positive total and used clamped at 0", total, used)
	}
}

// A total at or below zero is the contract's failed probe: a node advertising
// nothing must refuse to start, not join the fleet as a 0 GiB machine.
func TestMeminfoUMAProbe_ReserveThatLeavesNothingIsAFailedProbe(t *testing.T) {
	root := fakeProc(t, meminfoBody(refTotalKiB, refAvailKiB))
	for name, reserve := range map[string]float64{
		"reserve equals MemTotal":  float64(refTotalKiB) / kibPerGiB,
		"reserve exceeds MemTotal": 9,
	} {
		_, _, err := MeminfoUMAProbe(root, reserve)()
		if err == nil || !strings.Contains(err.Error(), "leaves nothing to advertise") {
			t.Errorf("%s: want a failed probe naming the reserve, got %v", name, err)
		}
	}
	if _, _, err := MeminfoUMAProbe(fakeProc(t, meminfoBody(0, 0)), 0)(); err == nil {
		t.Error("a zero MemTotal must be a failed probe")
	}
}

func TestMeminfoUMAProbe_NegativeReserveIsRefused(t *testing.T) {
	_, _, err := MeminfoUMAProbe(fakeProc(t, meminfoBody(refTotalKiB, refAvailKiB)), -1)()
	if err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("a negative reserve would advertise more than the box has; want a refusal, got %v", err)
	}
}

func TestMeminfoUMAProbe_MalformedInputFailsHonestly(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"no MemAvailable (zero would read as every byte in use)": {"MemTotal:  8112688 kB\nMemFree:  3211588 kB\n", "no MemAvailable"},
		"no MemTotal":             {"MemAvailable:  6502340 kB\n", "no MemTotal"},
		"empty file":              {"", "no MemTotal"},
		"non-numeric MemTotal":    {"MemTotal:  lots kB\nMemAvailable:  6502340 kB\n", "MemTotal"},
		"MemAvailable no value":   {"MemTotal:  8112688 kB\nMemAvailable:\n", "MemAvailable has no value"},
		"negative MemAvailable":   {"MemTotal:  8112688 kB\nMemAvailable:  -5 kB\n", "negative"},
		"key only as a substring": {"XMemTotal:  8112688 kB\nMemAvailable:  6502340 kB\n", "no MemTotal"},
	} {
		_, _, err := MeminfoUMAProbe(fakeProc(t, tc.body), 3)()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want an error containing %q, got %v", name, tc.want, err)
		}
	}
}

func TestMeminfoUMAProbe_UnreadableFileIsAnError(t *testing.T) {
	_, _, err := MeminfoUMAProbe(t.TempDir(), 3)() // no proc/meminfo under this root
	if err == nil || !strings.Contains(err.Error(), MeminfoSource) {
		t.Fatalf("a missing /proc/meminfo must fail the probe, naming the source; got %v", err)
	}
}

// TestResolveProviderNamed_LinuxMeminfo: an SoC has no nvidia-smi, so the meminfo
// source serves under its own label with vendor/arch from the installer manifest,
// and the gate error (when it fails too) names "linux-meminfo".
func TestResolveProviderNamed_LinuxMeminfo(t *testing.T) {
	prov, err := ResolveProviderNamed(failProbe("exec: nvidia-smi not found"),
		GenericProvider{Probe: MeminfoUMAProbe(fakeProc(t, meminfoBody(refTotalKiB, refAvailKiB)), 3), Source: MeminfoSource},
		func() (string, string) { t.Fatal("smi identity must not run on a non-smi box"); return "", "" },
		InstalledInfo{Profile: "rockchip-rk3588"})
	if err != nil {
		t.Fatal(err)
	}
	if prov.Source != "linux-meminfo" || prov.Vendor != "rockchip" || prov.Arch != "rk3588" {
		t.Fatalf("got %+v", prov)
	}
	if abs(prov.TotalGiB-(float64(refTotalKiB)/kibPerGiB-3)) > 1e-9 {
		t.Fatalf("selection reading not carried: %+v", prov)
	}
	_, err = ResolveProviderNamed(failProbe("smi dead"),
		GenericProvider{Probe: MeminfoUMAProbe(t.TempDir(), 3), Source: MeminfoSource}, nil, InstalledInfo{})
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"no working GPU memory source", "smi dead", "linux-meminfo (linux-meminfo: "} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}
