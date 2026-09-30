package hwdetect

import (
	"errors"
	"os"
	"reflect"
	"testing"
)

// The `compatible` bytes a supported board publishes, exactly as the kernel writes
// them: NUL-separated and NUL-terminated, board token first, SoC token last.
const (
	// the vendor 6.1 BSP kernel's device tree
	compatVendorKernel = "rockchip,rk3588s-orangepi-5\x00rockchip,rk3588\x00"
	// a mainline 7.0 device tree
	compatMainline = "xunlong,orangepi-5\x00rockchip,rk3588s\x00"
)

// fakeDeviceTree stands in for /proc/device-tree/compatible. It fails the test if
// the probe reads anything else: the path IS the contract.
func fakeDeviceTree(t *testing.T, body string, err error) func(string) ([]byte, error) {
	t.Helper()
	return func(path string) ([]byte, error) {
		if path != socCompatiblePath {
			t.Fatalf("probe read %q, want %q", path, socCompatiblePath)
		}
		return []byte(body), err
	}
}

// TestDetectSoCRecognisesBothRK3588Spellings: the vendor kernel says
// `rockchip,rk3588` and the mainline kernel says `rockchip,rk3588s`, on the same
// physical board. Matching only one would classify the box "cpu" after a kernel
// swap — and hand it a CPU-inference serving config.
func TestDetectSoCRecognisesBothRK3588Spellings(t *testing.T) {
	want := Facts{
		Vendor: "rockchip", Arch: "rk3588", GPUName: "Mali-G610", GPUCount: 1,
		Archs: []string{"rk3588"}, UMA: true,
	}
	for name, compat := range map[string]string{
		"vendor 6.1 kernel": compatVendorKernel,
		"mainline 7.0":      compatMainline,
		// A property is always NUL-terminated, but the rule is the token, not the terminator.
		"no trailing NUL": "rockchip,rk3588",
	} {
		got, ok := DetectSoC(fakeDeviceTree(t, compat, nil))
		if !ok {
			t.Errorf("%s: %q not recognised", name, compat)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: facts = %+v, want %+v", name, got, want)
		}
		if got.VRAMGb != 0 {
			t.Errorf("%s: an SoC has no dedicated VRAM, got %v GB", name, got.VRAMGb)
		}
	}
}

// TestDetectSoCIsInertEverywhereElse: the probe runs on every box, so "no" must be
// the answer for everything that is not a supported SoC — and it must leave the
// machine exactly as the PCI probes found it.
func TestDetectSoCIsInertEverywhereElse(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		err  error
	}{
		// x86 Linux and Windows: no device tree, the read fails.
		"x86 or Windows (no device tree)": {err: os.ErrNotExist},
		"unreadable property":             {err: errors.New("permission denied")},
		"empty property":                  {},
		"another ARM board":               {body: "raspberrypi,4-model-b\x00brcm,bcm2711\x00"},
		// The sibling SoCs have a different NPU and GPU; borrowing this tier would
		// mis-serve them, so they stay unclassified until measured.
		"rk3399":             {body: "rockchip,rk3399\x00"},
		"rk3576":             {body: "rockchip,rk3576\x00"},
		"rk3568":             {body: "vendor,board\x00rockchip,rk3568\x00"},
		"a different vendor": {body: "vendor,board\x00vendor,rk3588\x00"},
		// EXACT tokens: the board token alone, or an SoC token with a suffix, is not
		// the SoC — a prefix match would admit both.
		"board token alone":         {body: "rockchip,rk3588s-orangepi-5\x00"},
		"suffixed SoC token":        {body: "rockchip,rk3588-evb1-v10\x00"},
		"unseparated tokens":        {body: "rockchip,rk3588rockchip,rk3588s"},
		"a variant nobody measured": {body: "rockchip,rk3588j\x00"},
	} {
		if got, ok := DetectSoC(fakeDeviceTree(t, tc.body, tc.err)); ok || !reflect.DeepEqual(got, Facts{}) {
			t.Errorf("%s: got (%+v, %v), want the zero Facts and false", name, got, ok)
		}
	}
}

// TestSoCFactsClassifyAsTheRockchipTier: both device-tree spellings, end to end
// through the classifier, on the 7.7 GiB the reference board reports (ramGb rounds
// the 8,112,688 kB down to 7, so the RAM tier is min).
func TestSoCFactsClassifyAsTheRockchipTier(t *testing.T) {
	for name, compat := range map[string]string{
		"vendor 6.1 kernel": compatVendorKernel,
		"mainline 7.0":      compatMainline,
	} {
		f, ok := DetectSoC(fakeDeviceTree(t, compat, nil))
		if !ok {
			t.Fatalf("%s: not recognised", name)
		}
		f.RAMGb = 7
		got := Classify(f)
		if got.Profile != "rockchip-rk3588" {
			t.Errorf("%s: profile = %q, want rockchip-rk3588", name, got.Profile)
		}
		if got.RAMTier != "min" || got.BigRAM {
			t.Errorf("%s: ram_tier = %q big_ram = %v, want min / false", name, got.RAMTier, got.BigRAM)
		}
		if got.Reason == "" {
			t.Errorf("%s: no reason given — an operator must see which band caught the machine", name)
		}
	}
}

// TestNoSoCClassifiesAsBefore: a box the probe declines stays exactly where it was.
// The x86 shapes below are the ones that reach the cpu fallthrough today.
func TestNoSoCClassifiesAsBefore(t *testing.T) {
	if _, ok := DetectSoC(fakeDeviceTree(t, "", os.ErrNotExist)); ok {
		t.Fatal("an x86 box (no device tree) was recognised as an SoC")
	}
	for _, tc := range []struct {
		label string
		facts Facts
		want  string
	}{
		{"x86, no GPU", Facts{Vendor: "none", Arch: "none", RAMGb: 32}, "cpu"},
		{"AMD APU stays amd", Facts{Vendor: "amd", Arch: "gcn", VRAMGb: 0.5, GPUCount: 1, RAMGb: 32}, "amd-gcn"},
		{"NVIDIA stays nvidia", Facts{Vendor: "nvidia", Arch: "blackwell", VRAMGb: 16, GPUCount: 1, RAMGb: 64}, "blackwell-16"},
		// A rockchip vendor on any other arch is not the RK3588 band.
		{"rockchip, unknown arch", Facts{Vendor: "rockchip", Arch: "other", GPUCount: 1, RAMGb: 8}, "cpu"},
	} {
		if got := Classify(tc.facts).Profile; got != tc.want {
			t.Errorf("%s: profile = %q, want %q", tc.label, got, tc.want)
		}
	}
}
