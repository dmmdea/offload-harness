package main

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/fleetnode"
)

// amdDRM writes a /sys/class/drm lookalike: a non-AMD card (skipped) and one amdgpu APU, the shape
// the Vega box reports. Files named in drop are left out, to break a read.
func amdDRM(t *testing.T, drop ...string) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"vendor":              "0x1002",
		"device":              "0x15e7",
		"mem_info_vram_total": "536870912",   // 512 MiB carve-out
		"mem_info_vram_used":  "134217728",   // 128 MiB
		"mem_info_gtt_total":  "16240345088", // the GTT pool
		"mem_info_gtt_used":   "1073741824",  // 1 GiB
	}
	for _, d := range drop {
		delete(files, d)
	}
	for card, fs := range map[string]map[string]string{"card0": {"vendor": "0x10de"}, "card1": files} {
		dev := filepath.Join(root, card, "device")
		if err := os.MkdirAll(dev, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, v := range fs {
			if err := os.WriteFile(filepath.Join(dev, name), []byte(v+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

// The ongoing health sampler a linux-amdgpu node runs must be the per-device one: the diagnosis
// found gpu_devices[] absent on every non-NVIDIA node although ADR 0053 and FLEET-NODE.md say a
// Linux AMD node publishes it, because chooseSamplerKind only knew nvidia-smi. This goes through
// startVRAMSampler, the function runFleetServe calls, over a fake sysfs tree.
func TestStartVRAMSamplerPublishesGpuDevicesOnAnAmdgpuNode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	headline := func() (float64, float64, error) { return 99, 98, nil } // must not be what a device sampler reads

	t.Run("linux-amdgpu: one device row, composed like the memory probe", func(t *testing.T) {
		drm := amdDRM(t)
		prov := fleetnode.ResolvedProvider{Source: "linux-amdgpu", Probe: headline}
		s := startVRAMSampler(ctx, time.Hour, prov, true, drm, "")
		snap, ok := s.Load()
		if !ok {
			t.Fatal("Load() not ok")
		}
		if len(snap.Devices) != 1 {
			t.Fatalf("Devices = %+v, want exactly the one amdgpu card", snap.Devices)
		}
		d := snap.Devices[0]
		wantTotal := 0.5 + 15.125 // carve-out + GTT, an APU
		if d.Name != "amdgpu 0x15e7" || math.Abs(d.TotalGiB-wantTotal) > 1e-6 {
			t.Errorf("device = %+v, want amdgpu 0x15e7 totalling %v GiB", d, wantTotal)
		}
		if math.Abs(snap.TotalGiB-wantTotal) > 1e-6 {
			t.Errorf("headline total = %v, want the device's %v (not the stub memory probe's 99)", snap.TotalGiB, wantTotal)
		}
	})

	t.Run("uma=false (a discrete card) reports its VRAM alone", func(t *testing.T) {
		prov := fleetnode.ResolvedProvider{Source: "linux-amdgpu", Probe: headline}
		snap, ok := startVRAMSampler(ctx, time.Hour, prov, false, amdDRM(t), "").Load()
		if !ok || len(snap.Devices) != 1 || math.Abs(snap.Devices[0].TotalGiB-0.5) > 1e-6 {
			t.Fatalf("snapshot = %+v (ok %v), want one device of 0.5 GiB", snap, ok)
		}
	})

	t.Run("a per-device read that fails keeps the single reading, never loses the node's memory", func(t *testing.T) {
		// vram_used is gone: the device probe fails to compose; the gate probe (stubbed) worked.
		drm := amdDRM(t, "mem_info_vram_used")
		prov := fleetnode.ResolvedProvider{Source: "linux-amdgpu", Probe: headline}
		snap, ok := startVRAMSampler(ctx, time.Hour, prov, true, drm, "").Load()
		if !ok {
			t.Fatal("the node lost its memory reading to the per-device path")
		}
		if snap.Devices != nil || snap.TotalGiB != 99 {
			t.Errorf("snapshot = %+v, want the single-value reading (total 99) and no devices", snap)
		}
	})

	for _, source := range []string{"windows-generic", "linux-meminfo"} {
		t.Run(source+": no per-device signal, gpu_devices omitted", func(t *testing.T) {
			prov := fleetnode.ResolvedProvider{Source: source, Probe: headline}
			// A real amdgpu tree is present and must be ignored: the source decides, not the filesystem.
			snap, ok := startVRAMSampler(ctx, time.Hour, prov, true, amdDRM(t), "").Load()
			if !ok || snap.Devices != nil || snap.TotalGiB != 99 {
				t.Fatalf("snapshot = %+v (ok %v), want the single-value reading and no devices", snap, ok)
			}
		})
	}
}

// The wiring is only worth its name if the source string the Linux provider carries is the one the
// chooser recognises: genericMemProvider names it, chooseSamplerKind routes on it.
func TestTheLinuxGenericSourceIsTheOneTheSamplerChooserRoutesOn(t *testing.T) {
	g := genericMemProvider("linux", "amd-gcn", true, 0, t.TempDir())
	if g.Source == "" || chooseSamplerKind(g.Source) != samplerKindAmdgpuDevice {
		t.Fatalf("genericMemProvider(linux, amd-gcn).Source = %q routes to %v, want samplerKindAmdgpuDevice", g.Source, chooseSamplerKind(g.Source))
	}
	if !strings.Contains(g.Source, "amdgpu") {
		t.Errorf("source %q does not name amdgpu", g.Source)
	}
}

// runFleetServe is too large to run in a test, so this pins the one thing the tests above cannot
// see: that it starts its sampler through startVRAMSampler, and that no second sampler start is left
// in main.go to bypass the chooser (the diagnosis' defect was exactly such a branch).
func TestFleetServeStartsItsSamplerThroughTheChooser(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if n := strings.Count(src, "sampler := startVRAMSampler(ctx, 2*time.Second, prov, uma, \"/sys/class/drm\", cfg.PrimaryGPUUUID)"); n != 1 {
		t.Errorf("runFleetServe must start its sampler with startVRAMSampler(ctx, 2*time.Second, prov, uma, \"/sys/class/drm\", cfg.PrimaryGPUUUID) exactly once, found %d", n)
	}
	for _, direct := range []string{"fleetnode.StartDeviceProbeSampler(", "fleetnode.StartProbeSampler("} {
		// Inside startVRAMSampler only: two device starts (nvidia, amdgpu) and two single starts (the amdgpu fallback, the default).
		want := map[string]int{"fleetnode.StartDeviceProbeSampler(": 2, "fleetnode.StartProbeSampler(": 2}[direct]
		if n := strings.Count(src, direct); n != want {
			t.Errorf("main.go calls %s %d times, want %d (all inside startVRAMSampler): a sampler started elsewhere bypasses chooseSamplerKind", direct, n, want)
		}
	}
}
