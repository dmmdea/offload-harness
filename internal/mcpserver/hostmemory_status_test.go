package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// offload_status and the host's memory (the paging incident of 2026-10-09). The gpu_lease block
// carries the reading, the headroom, what the live leases declared and the verdict OK / NEAR / OVER;
// the one-line brief names OVER first, in capitals, because "free" at the head of that line reads as
// "the box has room" while committed memory is above physical RAM.

func withHost(t *testing.T, mem gpuprobe.HostMemory, ok bool) {
	t.Helper()
	restore := gpuprobe.UseHostMemoryReader(func() (gpuprobe.HostMemory, bool) { return mem, ok })
	t.Cleanup(restore)
}

func hostBlock(t *testing.T, view map[string]any) map[string]any {
	t.Helper()
	hm, ok := view["host_memory"].(map[string]any)
	if !ok {
		t.Fatalf("the gpu_lease view carries no host_memory block: %v", view)
	}
	return hm
}

func freeLeaseConfig(t *testing.T) config.Config {
	t.Helper()
	statusSamplesGPU = false
	t.Cleanup(func() { statusSamplesGPU = true })
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	return cfg
}

func TestStatusCarriesTheHostMemoryBlock(t *testing.T) {
	withHost(t, gpuprobe.HostMemory{PhysicalGiB: 127.7, AvailableGiB: 69, CommitUsedGiB: 56, CommitLimitGiB: 187.7}, true)
	view := localLeaseView(context.Background(), freeLeaseConfig(t))
	hm := hostBlock(t, view)
	want := map[string]any{"read": true, "verdict": "OK", "physical_gib": 127.7, "available_gib": 69.0, "commit_used_gib": 56.0,
		"commit_limit_gib": 187.7, "headroom_gib": 8.0, "declared_live_gib": 0.0, "pending_gib": 0.0, "admits_up_to_gib": 119.7}
	for k, v := range want {
		if hm[k] != v {
			t.Errorf("host_memory.%s = %v, want %v", k, hm[k], v)
		}
	}
	// OK says nothing on the brief line.
	if line := gpuLeaseVerdictLine(view); strings.Contains(line, "host RAM") || strings.Contains(line, "HOST RAM") {
		t.Errorf("an OK host must not appear on the brief line:\n%s", line)
	}
}

// OVER: committed memory above physical RAM. The block says so with the note, and the brief LEADS
// with it, ahead of the lease verdict word.
func TestStatusNamesOverLoudlyOnTheBriefLine(t *testing.T) {
	withHost(t, gpuprobe.HostMemory{PhysicalGiB: 127.7, AvailableGiB: 3.2, CommitUsedGiB: 162.9, CommitLimitGiB: 187.7}, true)
	view := localLeaseView(context.Background(), freeLeaseConfig(t))
	hm := hostBlock(t, view)
	if hm["verdict"] != "OVER" {
		t.Fatalf("committed 162.9 GiB on 127.7 GiB physical is OVER, got %v", hm["verdict"])
	}
	if note, _ := hm["note"].(string); !strings.Contains(note, "paging") || !strings.Contains(note, "never an acceptable state") {
		t.Errorf("the block must say plainly that paging is not acceptable: %q", note)
	}
	line := gpuLeaseVerdictLine(view)
	if !strings.HasPrefix(line, "HOST RAM OVER (committed 162.9 of 127.7 GiB physical: the box is paging); ") {
		t.Fatalf("the brief line must lead with the OVER verdict, got:\n%s", line)
	}
	if !strings.Contains(line, "free") || !strings.Contains(line, "queue with:") {
		t.Errorf("the rest of the line is unchanged:\n%s", line)
	}
}

func TestStatusNamesNearAsATailAndUnknownNotAtAll(t *testing.T) {
	withHost(t, gpuprobe.HostMemory{PhysicalGiB: 100, AvailableGiB: 9, CommitUsedGiB: 95, CommitLimitGiB: 160}, true)
	view := localLeaseView(context.Background(), freeLeaseConfig(t))
	if hostBlock(t, view)["verdict"] != "NEAR" {
		t.Fatalf("95 of 100 GiB committed with 8 GiB of headroom is NEAR, got %v", hostBlock(t, view)["verdict"])
	}
	line := gpuLeaseVerdictLine(view)
	if strings.HasPrefix(line, "HOST RAM") || !strings.Contains(line, "host RAM NEAR the limit (committed 95.0 of 100.0 GiB physical, 8.0 GiB headroom)") {
		t.Errorf("NEAR is a tail, not a lead:\n%s", line)
	}

	withHost(t, gpuprobe.HostMemory{}, false)
	view = localLeaseView(context.Background(), freeLeaseConfig(t))
	hm := hostBlock(t, view)
	if hm["verdict"] != "unknown" || hm["read"] != false {
		t.Fatalf("an unreadable host is unknown, got %v", hm)
	}
	if _, has := hm["physical_gib"]; has {
		t.Error("a reading that was not taken must not print numbers")
	}
	if line := gpuLeaseVerdictLine(view); strings.Contains(line, "RAM") {
		t.Errorf("unknown says nothing on the brief line:\n%s", line)
	}
}

// What the live leases declared: the sum, and the part still to load.
func TestStatusSumsTheDeclaredNeedsOfTheLiveLeases(t *testing.T) {
	withHost(t, gpuprobe.HostMemory{PhysicalGiB: 127.7, AvailableGiB: 60, CommitUsedGiB: 70, CommitLimitGiB: 187.7}, true)
	cfg := freeLeaseConfig(t)
	m, err := gpulease.OpenAt("", cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "krea2 lane", TTL: time.Hour, HostRAMGiB: 30})
	if err != nil {
		t.Fatalf("a 30 GiB lease on a host with 70 of 127.7 committed: %v", err)
	}
	defer func() { _ = l.Release() }()
	hm := hostBlock(t, localLeaseView(context.Background(), cfg))
	if hm["declared_live_gib"] != 30.0 {
		t.Fatalf("declared_live_gib = %v, want 30", hm["declared_live_gib"])
	}
	pending, _ := hm["pending_gib"].(float64)
	if pending <= 0 || pending > 30 {
		t.Fatalf("pending_gib = %v: nothing below this process holds the 30 GiB yet, so most of it is still to load", pending)
	}
}
