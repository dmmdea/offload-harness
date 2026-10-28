package fleetnode

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestVendorArchFromProfile(t *testing.T) {
	cases := []struct{ profile, vendor, arch string }{
		{"amd-rdna3", "amd", "rdna3"},
		{"amd-rdna3-dgpu", "amd", "rdna3"},
		{"amd-gcn", "amd", "gcn"},
		{"blackwell-16", "nvidia", "blackwell"},
		{"blackwell-72", "nvidia", "blackwell"},
		{"ampere-8", "nvidia", "ampere"},
		{"maxwell-3", "nvidia", "maxwell"},
		{"volta-16", "nvidia", "volta"},
		{"dual-gpu", "nvidia", "blackwell"},
		{"rockchip-rk3588", "rockchip", "rk3588"},
		{"cpu", "none", "none"},
		{"", "unknown", "unknown"},
		{"martian-gpu", "unknown", "unknown"},
	}
	for _, tc := range cases {
		v, a := VendorArchFromProfile(tc.profile)
		if v != tc.vendor || a != tc.arch {
			t.Errorf("VendorArchFromProfile(%q) = (%q, %q), want (%q, %q)", tc.profile, v, a, tc.vendor, tc.arch)
		}
	}
}

func TestUMAFromProfile(t *testing.T) {
	cases := []struct {
		profile string
		uma, ok bool
	}{
		{"amd-rdna3", true, true},
		{"amd-gcn", true, true},
		{"amd-rdna3-dgpu", false, true},
		{"rockchip-rk3588", true, true}, // an SoC: no carve-out, GPU and NPU share the CPU's RAM
		{"blackwell-16", false, true},
		{"ampere-8", false, true},
		{"maxwell-3", false, true}, // a discrete 3 GB card, dedicated VRAM
		{"volta-16", false, true},
		{"dual-gpu", false, true},
		{"", false, false},    // no signal — caller falls back to the capacity heuristic
		{"cpu", false, false}, // no GPU class at all
		{"martian", false, false},
	}
	for _, tc := range cases {
		uma, ok := UMAFromProfile(tc.profile)
		if uma != tc.uma || ok != tc.ok {
			t.Errorf("UMAFromProfile(%q) = (%v, %v), want (%v, %v)", tc.profile, uma, ok, tc.uma, tc.ok)
		}
	}
}

func TestReadInstalledInfo(t *testing.T) {
	p := filepath.Join(t.TempDir(), "installed.json")
	if err := os.WriteFile(p, []byte(`{"profile":"amd-rdna3","backend":"vulkan","llama_cpp_tag":"b9934"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadInstalledInfo(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile != "amd-rdna3" || got.Backend != "vulkan" {
		t.Fatalf("got %+v", got)
	}
	if _, err := ReadInstalledInfo(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("missing manifest must error (zero info, handled by the caller)")
	}
}

func TestReadInstalledInfoCarriesAccelerators(t *testing.T) {
	p := filepath.Join(t.TempDir(), "installed.json")
	os.WriteFile(p, []byte(`{"profile":"blackwell-8","backend":"cuda","accelerators":["hailo-8l"]}`), 0o644)
	info, err := ReadInstalledInfo(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Accelerators) != 1 || info.Accelerators[0] != "hailo-8l" {
		t.Fatalf("accelerators = %v", info.Accelerators)
	}
	os.WriteFile(p, []byte(`{"profile":"blackwell-8","backend":"cuda"}`), 0o644)
	info, _ = ReadInstalledInfo(p)
	if info.Accelerators != nil {
		t.Fatalf("absent field must stay nil, got %v", info.Accelerators)
	}
}

func okProbe(total, used float64) MemProbe {
	return func() (float64, float64, error) { return total, used, nil }
}

func failProbe(msg string) MemProbe {
	return func() (float64, float64, error) { return 0, 0, errors.New(msg) }
}

// TestResolveProvider_SmiWins: a working nvidia-smi keeps every existing NVIDIA
// node byte-identical — smi source, smi-derived vendor/arch, generic never consulted.
func TestResolveProvider_SmiWins(t *testing.T) {
	prov, err := ResolveProvider(okProbe(16, 2), failProbe("must not be consulted"),
		func() (string, string) { return "nvidia", "blackwell" }, InstalledInfo{Profile: "amd-rdna3"})
	if err != nil {
		t.Fatal(err)
	}
	if prov.Source != "nvidia-smi" || prov.Vendor != "nvidia" || prov.Arch != "blackwell" {
		t.Fatalf("got %+v", prov)
	}
	if prov.TotalGiB != 16 || prov.UsedGiB != 2 {
		t.Fatalf("selection reading not carried: %+v", prov)
	}
}

// TestResolveProvider_GenericFallback: no nvidia-smi → the windows-generic
// source serves, with vendor/arch from the installer manifest (never re-derived
// from NVIDIA product names).
func TestResolveProvider_GenericFallback(t *testing.T) {
	smiIdentityCalls := 0
	prov, err := ResolveProvider(failProbe("exec: nvidia-smi not found"), okProbe(35.7, 3.2),
		func() (string, string) { smiIdentityCalls++; return "nvidia", "nvidia" }, InstalledInfo{Profile: "amd-rdna3"})
	if err != nil {
		t.Fatal(err)
	}
	if prov.Source != "windows-generic" || prov.Vendor != "amd" || prov.Arch != "rdna3" {
		t.Fatalf("got %+v", prov)
	}
	if prov.TotalGiB != 35.7 {
		t.Fatalf("selection reading not carried: %+v", prov)
	}
	if smiIdentityCalls != 0 {
		t.Fatalf("smi identity must be LAZY — called %d times on a non-smi box (it shells nvidia-smi)", smiIdentityCalls)
	}
}

// TestResolveProvider_BothFail: the new gate error names BOTH sources and their
// failures — "no working GPU memory source", not a brand requirement.
func TestResolveProvider_BothFail(t *testing.T) {
	_, err := ResolveProvider(failProbe("smi dead"), failProbe("wddm dead"), nil, InstalledInfo{})
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"no working GPU memory source", "smi dead", "wddm dead"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}

// TestResolveProvider_NilGeneric: off-Windows composition (nil generic) reports
// the absence honestly instead of a nil-call panic.
func TestResolveProvider_NilGeneric(t *testing.T) {
	_, err := ResolveProvider(failProbe("smi dead"), nil, nil, InstalledInfo{})
	if err == nil || !strings.Contains(err.Error(), "not available on this platform") {
		t.Fatalf("got %v", err)
	}
}

// A manifest recorded under an older layout reads clean: the three-card box classified as dual-gpu before
// blackwell-3x16 existed, and a two-card box that later gained a card is recorded as blackwell-2x16. Nothing in the
// read refuses the old id, drops a key the node advertises (the alternate backends, the accelerators) or changes the
// identity the node resolves, so the upgrade to blackwell-3x16 is a change of the recorded id and nothing else.
func TestOlderLayoutManifestsReadWithEverythingTheNodeAdvertises(t *testing.T) {
	for _, profile := range []string{"dual-gpu", "blackwell-2x16", "blackwell-3x16"} {
		t.Run(profile, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "installed.json")
			body := `{"profile":"` + profile + `","backend":"cuda","accelerators":["hailo-8l"],"alt_backends":["cpu"],` +
				`"components":{"llama-swap":"v1"},"models":[{"name":"seat-a","sha256":"00"},{"name":"seat-b","sha256":"11"}]}`
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			info, err := ReadInstalledInfo(p)
			if err != nil {
				t.Fatalf("a manifest recorded as %s must read: %v", profile, err)
			}
			if info.Profile != profile {
				t.Errorf("profile = %q, want %q", info.Profile, profile)
			}
			if got := info.Backends(); !slices.Equal(got, []string{"cuda", "cpu"}) {
				t.Errorf("backends = %v, want the primary and the alternate the install rendered", got)
			}
			if !slices.Equal(info.Accelerators, []string{"hailo-8l"}) {
				t.Errorf("accelerators = %v, want the list the manifest records", info.Accelerators)
			}
			if vendor, arch := VendorArchFromProfile(profile); vendor != "nvidia" || arch != "blackwell" {
				t.Errorf("VendorArchFromProfile(%q) = %s/%s, want nvidia/blackwell", profile, vendor, arch)
			}
			if uma, ok := UMAFromProfile(profile); uma || !ok {
				t.Errorf("UMAFromProfile(%q) = (%v, %v), want a discrete NVIDIA class (false, true)", profile, uma, ok)
			}
		})
	}
}
