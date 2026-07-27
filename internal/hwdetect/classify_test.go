package hwdetect

import "testing"

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
