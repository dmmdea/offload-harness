package fleetnode

import (
	"errors"
	"os"
	"path/filepath"
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
		{"volta-16", "nvidia", "volta"},
		{"dual-gpu", "nvidia", "blackwell"},
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
		{"blackwell-16", false, true},
		{"ampere-8", false, true},
		{"volta-16", false, true},
		{"dual-gpu", false, true},
		{"", false, false},        // no signal — caller falls back to the capacity heuristic
		{"cpu", false, false},     // no GPU class at all
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

func okProbe(total, used float64) MemProbe {
	return func() (float64, float64, error) { return total, used, nil }
}

func failProbe(msg string) MemProbe {
	return func() (float64, float64, error) { return 0, 0, errors.New(msg) }
}

// TestResolveProvider_SmiWins: a working nvidia-smi keeps every existing NVIDIA
// node byte-identical — smi source, smi-derived vendor/arch, generic never consulted.
func TestResolveProvider_SmiWins(t *testing.T) {
	prov, err := ResolveProvider(okProbe(16, 2), failProbe("must not be consulted"), "nvidia", "blackwell", InstalledInfo{Profile: "amd-rdna3"})
	if err != nil {
		t.Fatal(err)
	}
	if prov.Source != "nvidia-smi" || prov.Vendor != "nvidia" || prov.Arch != "blackwell" {
		t.Fatalf("got %+v", prov)
	}
	total, used, perr := prov.Probe()
	if perr != nil || total != 16 || used != 2 {
		t.Fatalf("probe = (%v, %v, %v)", total, used, perr)
	}
}

// TestResolveProvider_GenericFallback: no nvidia-smi → the windows-generic
// source serves, with vendor/arch from the installer manifest (never re-derived
// from NVIDIA product names).
func TestResolveProvider_GenericFallback(t *testing.T) {
	prov, err := ResolveProvider(failProbe("exec: nvidia-smi not found"), okProbe(35.7, 3.2), "nvidia", "nvidia", InstalledInfo{Profile: "amd-rdna3"})
	if err != nil {
		t.Fatal(err)
	}
	if prov.Source != "windows-generic" || prov.Vendor != "amd" || prov.Arch != "rdna3" {
		t.Fatalf("got %+v", prov)
	}
}

// TestResolveProvider_BothFail: the new gate error names BOTH sources and their
// failures — "no working GPU memory source", not a brand requirement.
func TestResolveProvider_BothFail(t *testing.T) {
	_, err := ResolveProvider(failProbe("smi dead"), failProbe("wddm dead"), "nvidia", "nvidia", InstalledInfo{})
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
	_, err := ResolveProvider(failProbe("smi dead"), nil, "nvidia", "nvidia", InstalledInfo{})
	if err == nil || !strings.Contains(err.Error(), "not available on this platform") {
		t.Fatalf("got %v", err)
	}
}
