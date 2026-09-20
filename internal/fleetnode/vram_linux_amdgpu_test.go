package fleetnode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDRM builds a /sys/class/drm lookalike. Values are bytes, as the kernel
// publishes them. card0 is an NVIDIA node with no amdgpu files (must be skipped),
// card1 is the APU under test — the binxarn shape on 2026-09-20: 512 MiB carve-out,
// 15,487 MiB GTT.
func fakeDRM(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mk := func(card string, files map[string]string) {
		dev := filepath.Join(root, card, "device")
		if err := os.MkdirAll(dev, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, v := range files {
			if err := os.WriteFile(filepath.Join(dev, name), []byte(v+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("card0", map[string]string{"vendor": "0x10de"})
	mk("card1", map[string]string{
		"vendor":              "0x1002",
		"device":              "0x15e7",
		"mem_info_vram_total": "536870912",   // 512 MiB
		"mem_info_vram_used":  "134217728",   // 128 MiB
		"mem_info_gtt_total":  "16240345088", // 15,487 MiB
		"mem_info_gtt_used":   "1073741824",  // 1 GiB
	})
	return root
}

func TestAmdgpuSysfsProbe_UMAAddsTheGTTBudget(t *testing.T) {
	total, used, err := AmdgpuSysfsProbe(fakeDRM(t), true)()
	if err != nil {
		t.Fatal(err)
	}
	// 0.5 GiB carve-out + 15.125 GiB GTT; 0.125 GiB + 1 GiB used.
	if got, want := total, 0.5+15.125; abs(got-want) > 1e-6 {
		t.Errorf("uma total = %v GiB, want %v", got, want)
	}
	if got, want := used, 0.125+1.0; abs(got-want) > 1e-6 {
		t.Errorf("uma used = %v GiB, want %v", got, want)
	}
}

func TestAmdgpuSysfsProbe_DiscreteReportsVRAMOnly(t *testing.T) {
	total, used, err := AmdgpuSysfsProbe(fakeDRM(t), false)()
	if err != nil {
		t.Fatal(err)
	}
	if total != 0.5 || used != 0.125 {
		t.Errorf("discrete = %v/%v GiB, want 0.5/0.125", total, used)
	}
}

func TestAmdgpuSysfsProbe_SkipsNonAMDAndFailsHonestlyWithNone(t *testing.T) {
	root := t.TempDir()
	dev := filepath.Join(root, "card0", "device")
	if err := os.MkdirAll(dev, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dev, "vendor"), []byte("0x10de\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := AmdgpuSysfsProbe(root, true)()
	if err == nil || !strings.Contains(err.Error(), "no amdgpu device") {
		t.Fatalf("want a no-device error naming the root, got %v", err)
	}
}

func TestAmdgpuSysfsProbe_ZeroTotalIsAFailedProbe(t *testing.T) {
	root := fakeDRM(t)
	dev := filepath.Join(root, "card1", "device")
	for _, f := range []string{"mem_info_vram_total", "mem_info_gtt_total"} {
		if err := os.WriteFile(filepath.Join(dev, f), []byte("0\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := AmdgpuSysfsProbe(root, true)(); err == nil {
		t.Fatal("a zero total must be a failed probe, not a 0 GiB node")
	}
}

func TestAmdgpuSysfsDeviceProbe_ListsEveryAMDCardWithTheSameComposition(t *testing.T) {
	devs, err := AmdgpuSysfsDeviceProbe(fakeDRM(t), true)()
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 1 || devs[0].Index != 0 || devs[0].Name != "amdgpu 0x15e7" {
		t.Fatalf("got %+v", devs)
	}
	if abs(devs[0].TotalGiB-(0.5+15.125)) > 1e-6 {
		t.Errorf("device total must match the MemProbe composition, got %v", devs[0].TotalGiB)
	}
	if abs(devs[0].FreeGiB-((0.5+15.125)-(0.125+1.0))) > 1e-6 {
		t.Errorf("device free must be total minus the same used composition, got %v", devs[0].FreeGiB)
	}
}

// TestResolveProviderNamed_LinuxAmdgpuFallback: no nvidia-smi → the named Linux
// source serves, vendor/arch from the manifest, and the gate error (when it
// fails too) names "linux-amdgpu", not the Windows source.
func TestResolveProviderNamed_LinuxAmdgpuFallback(t *testing.T) {
	prov, err := ResolveProviderNamed(failProbe("exec: nvidia-smi not found"),
		GenericProvider{Probe: AmdgpuSysfsProbe(fakeDRM(t), true), Source: "linux-amdgpu"},
		func() (string, string) { t.Fatal("smi identity must not run on a non-smi box"); return "", "" },
		InstalledInfo{Profile: "amd-gcn"})
	if err != nil {
		t.Fatal(err)
	}
	if prov.Source != "linux-amdgpu" || prov.Vendor != "amd" || prov.Arch != "gcn" {
		t.Fatalf("got %+v", prov)
	}
	if abs(prov.TotalGiB-(0.5+15.125)) > 1e-6 {
		t.Fatalf("selection reading not carried: %+v", prov)
	}
	_, err = ResolveProviderNamed(failProbe("smi dead"), GenericProvider{Probe: failProbe("sysfs dead"), Source: "linux-amdgpu"}, nil, InstalledInfo{})
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"no working GPU memory source", "smi dead", "linux-amdgpu (sysfs dead)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}

// TestResolveProvider_StillLabelsWindowsGeneric: the wrapper keeps the original
// label so every existing node's serve banner is unchanged.
func TestResolveProvider_StillLabelsWindowsGeneric(t *testing.T) {
	prov, err := ResolveProvider(failProbe("no smi"), okProbe(35.7, 3.2), nil, InstalledInfo{Profile: "amd-rdna3"})
	if err != nil {
		t.Fatal(err)
	}
	if prov.Source != "windows-generic" {
		t.Fatalf("got %+v", prov)
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
