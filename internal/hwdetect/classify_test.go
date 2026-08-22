package hwdetect

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The tables below are ported from setup/detect.tests.ps1 verbatim. Two
// implementations of one classifier exist while the Windows installer still calls
// the PowerShell one, so they must agree on every case the shipped suite asserts —
// a machine's tier cannot depend on which code path asked.

func TestArchFromNameMatchesTheShippedTable(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"NVIDIA GeForce RTX 5060 Ti", "blackwell"},
		{"NVIDIA GeForce RTX 5090", "blackwell"},
		{"NVIDIA GeForce RTX 5060", "blackwell"},
		{"NVIDIA GeForce RTX 4090", "ada"},
		{"NVIDIA GeForce RTX 3070 Laptop GPU", "ampere"},
		{"NVIDIA GeForce RTX 3050", "ampere"},
		{"Tesla V100-PCIE-16GB", "volta"},
		{"NVIDIA RTX PRO 4500 Blackwell", "blackwell"},
		{"NVIDIA RTX PRO 5000 Blackwell", "blackwell"},
		{"NVIDIA RTX PRO 6000 Blackwell Workstation Edition", "blackwell"},
		{"AMD Radeon 780M Graphics", "rdna3"},
		{"AMD Radeon RX 7900 XTX", "rdna3"},
		{"AMD Radeon Vega 7 Graphics", "gcn"},
		{"", "none"},
	} {
		if got := ArchFromName(tc.name); got != tc.want {
			t.Errorf("ArchFromName(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestClassifyMatchesTheShippedTable(t *testing.T) {
	for _, tc := range []struct {
		label        string
		vendor, arch string
		vram         float64
		gpus, ram    int
		want         string
		wantBig      bool
	}{
		{"5060 Ti 16GB (cfg1)", "nvidia", "blackwell", 16, 1, 64, "blackwell-16", false},
		{"V100 16GB (cfg2)", "nvidia", "volta", 16, 1, 64, "volta-16", false},
		{"dual 5060Ti+V100 (cfg3)", "nvidia", "blackwell", 16, 2, 64, "dual-gpu", false},
		{"dual +128GB (cfg4)", "nvidia", "blackwell", 16, 2, 128, "dual-gpu", true},
		{"3070 8GB (cfg5)", "nvidia", "ampere", 8, 1, 16, "ampere-8", false},
		{"3070+64GB (cfg6)", "nvidia", "ampere", 8, 1, 64, "ampere-8", false},
		{"780M+64GB (cfg7)", "amd", "rdna3", 0.5, 1, 64, "amd-rdna3", false},
		{"780M 4GB carve-out", "amd", "rdna3", 4, 1, 64, "amd-rdna3", false},
		{"RX 7900 XTX 24GB", "amd", "rdna3", 24, 1, 64, "amd-rdna3-dgpu", false},
		{"RX 7700 XT 12GB", "amd", "rdna3", 12, 1, 32, "amd-rdna3-dgpu", false},
		{"5060 8GB (cfg8)", "nvidia", "blackwell", 8, 1, 32, "blackwell-8", false},
		{"3050 6GB (cfg10)", "nvidia", "ampere", 6, 1, 16, "ampere-6", false},
		{"3090 24GB (defensive)", "nvidia", "ampere", 24, 1, 64, "ampere-16", false},
		{"5090 32GB (cfg13)", "nvidia", "blackwell", 32, 1, 64, "blackwell-32", false},
		{"PRO 4500 32GB (cfg13)", "nvidia", "blackwell", 32, 1, 128, "blackwell-32", false},
		{"PRO 5000 48GB (cfg14)", "nvidia", "blackwell", 48, 1, 64, "blackwell-48", false},
		{"PRO 5000 72GB (cfg15)", "nvidia", "blackwell", 72, 1, 128, "blackwell-72", false},
		{"PRO 6000 96GB (cfg15)", "nvidia", "blackwell", 96, 1, 128, "blackwell-72", false},
		{"Vega7+32GB (cfg12)", "amd", "gcn", 0.5, 1, 32, "amd-gcn", false},
		{"no GPU", "none", "none", 0, 0, 32, "cpu", false},
	} {
		got := Classify(Facts{Vendor: tc.vendor, Arch: tc.arch, VRAMGb: tc.vram, GPUCount: tc.gpus, RAMGb: tc.ram})
		if got.Profile != tc.want {
			t.Errorf("%s: profile = %q, want %q", tc.label, got.Profile, tc.want)
		}
		if got.BigRAM != tc.wantBig {
			t.Errorf("%s: big_ram = %v, want %v", tc.label, got.BigRAM, tc.wantBig)
		}
		if got.Reason == "" {
			t.Errorf("%s: no reason given — an operator must see which band caught the machine", tc.label)
		}
	}
}

// TestHomogeneousBlackwellPairGetsItsOwnTier: two same-arch sm_120 cards are a
// different hardware class from the heterogeneous dual-gpu rig — one CUDA build,
// per-card pinning topology — and the rule must be STRICT: it fires only when
// every card's architecture was actually captured and is Blackwell. A pair whose
// second card is unknown must fall through to dual-gpu rather than claim a
// same-build tier the box may not be able to serve.
func TestHomogeneousBlackwellPairGetsItsOwnTier(t *testing.T) {
	for _, tc := range []struct {
		label   string
		archs   []string
		gpus    int
		ram     int
		want    string
		wantBig bool
	}{
		{"5060Ti+5070Ti 128GB (the reference box)", []string{"blackwell", "blackwell"}, 2, 127, "blackwell-2x16", true},
		{"2x blackwell, 64GB", []string{"blackwell", "blackwell"}, 2, 64, "blackwell-2x16", false},
		{"blackwell+volta (cfg3's real shape)", []string{"blackwell", "volta"}, 2, 64, "dual-gpu", false},
		{"pair counted but archs not captured", nil, 2, 128, "dual-gpu", true},
		{"pair with only ONE arch captured", []string{"blackwell"}, 2, 128, "dual-gpu", true},
		{"three blackwell cards", []string{"blackwell", "blackwell", "blackwell"}, 3, 128, "dual-gpu", true},
		{"single blackwell stays in its band", []string{"blackwell"}, 1, 128, "blackwell-16", false},
	} {
		got := Classify(Facts{Vendor: "nvidia", Arch: "blackwell", VRAMGb: 16,
			GPUCount: tc.gpus, RAMGb: tc.ram, Archs: tc.archs})
		if got.Profile != tc.want {
			t.Errorf("%s: profile = %q, want %q", tc.label, got.Profile, tc.want)
		}
		if got.Profile == "blackwell-2x16" && got.BigRAM != tc.wantBig {
			t.Errorf("%s: big_ram = %v, want %v", tc.label, got.BigRAM, tc.wantBig)
		}
	}
	// The "16" in the tier name is ENFORCED, not decorative: the template pins the
	// 26B and a ~10GB vision seat per card, so a pair outside the 16GB band must
	// fall through to dual-gpu rather than inherit a topology that OOMs it.
	for _, tc := range []struct {
		label string
		vram  float64
		want  string
	}{
		{"2x blackwell 8GB (5060 pair) is NOT the 16GB tier", 8, "dual-gpu"},
		{"2x blackwell 32GB (5090 pair) is NOT the 16GB tier", 32, "dual-gpu"},
		{"band floor: 12GB counts as 16-class", 12, "blackwell-2x16"},
		{"band ceiling: 24GB does not", 24, "dual-gpu"},
	} {
		got := Classify(Facts{Vendor: "nvidia", Arch: "blackwell", VRAMGb: tc.vram,
			GPUCount: 2, RAMGb: 128, Archs: []string{"blackwell", "blackwell"}})
		if got.Profile != tc.want {
			t.Errorf("%s: profile = %q, want %q", tc.label, got.Profile, tc.want)
		}
	}
}

// TestClassifyNeverGuessesCPUForAKnownVendor: calling an unclassifiable AMD box "cpu"
// would strip it of the entire Vulkan serving path, which is the failure mode a
// Windows-only detector could never surface.
func TestClassifyNeverGuessesCPUForAKnownVendor(t *testing.T) {
	got := Classify(Facts{Vendor: "amd", Arch: "other", VRAMGb: 0.5, GPUCount: 1, RAMGb: 64})
	if got.Profile != "amd-gcn" {
		t.Errorf("an unclassified AMD part = %q, want the weakest Vulkan path, never cpu", got.Profile)
	}
}

// TestDetectDescribesThisMachine is the only test that touches real hardware: the
// probe must at least know the OS and how much RAM it has, or a plan built from it
// would be fiction.
func TestDetectDescribesThisMachine(t *testing.T) {
	f := Detect()
	if f.OS == "" {
		t.Error("no OS reported")
	}
	if f.RAMGb <= 0 {
		t.Errorf("RAM = %d GB — the dual-gpu band keys on this", f.RAMGb)
	}
	if f.Vendor == "nvidia" || f.Vendor == "amd" {
		if f.GPUName == "" {
			t.Error("a detected GPU must be named")
		}
		if f.GPUCount < 1 {
			t.Error("a detected GPU must be counted")
		}
	}
	t.Logf("detected: %s %s %s %.1f GB VRAM x%d, %d GB RAM, driver %q -> %s",
		f.OS, f.Vendor, f.Arch, f.VRAMGb, f.GPUCount, f.RAMGb, f.DriverVersion, Classify(f).Profile)
}

func TestAcceleratorsFromHailortcli(t *testing.T) {
	scan := "Hailo Devices:\n[-] Device: 0000:03:00.0\n"
	ident := "Executing on device: 0000:03:00.0\nIdentifying board\nDevice Architecture: HAILO8L\nPart Number: HM21LB1C2KAE\n"
	if got := AcceleratorsFromHailortcli(scan, ident); len(got) != 1 || got[0] != "hailo-8l" {
		t.Fatalf("got %v, want [hailo-8l]", got)
	}
	// A full Hailo-8 is NOT an 8L: its HEFs are a different build and must not claim the tier.
	if got := AcceleratorsFromHailortcli(scan, "Device Architecture: HAILO8\n"); len(got) != 0 {
		t.Fatalf("HAILO8 classified as %v, want none", got)
	}
	if got := AcceleratorsFromHailortcli("Hailo Devices:\n", ""); len(got) != 0 {
		t.Fatalf("no device classified as %v, want none", got)
	}
}

func TestDetectAcceleratorsToleratesMissingTool(t *testing.T) {
	run := func(args ...string) (string, error) { return "", errors.New("exec: hailortcli: not found") }
	if got := DetectAccelerators(run); got != nil {
		t.Fatalf("got %v, want nil when hailortcli is absent", got)
	}
}

func TestVerdictCarriesAccelerators(t *testing.T) {
	v := Classify(Facts{Vendor: "nvidia", Arch: "blackwell", VRAMGb: 8, GPUCount: 1, RAMGb: 64})
	if v.Profile != "blackwell-8" {
		t.Fatalf("profile = %q", v.Profile)
	}
	if v.Accelerators != nil {
		t.Fatalf("Classify must not populate accelerators (detection is a separate probe); got %v", v.Accelerators)
	}
	b, _ := json.Marshal(v)
	if strings.Contains(string(b), `"accelerators":null`) {
		t.Fatalf("nil accelerators must be omitted from JSON, got %s", b)
	}
}
