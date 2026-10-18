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
// "the box has room" while the host has no memory available or its commit charge is at an enforced limit.

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
		"commit_limit_gib": 187.7, "headroom_gib": 8.0, "declared_live_gib": 0.0, "declared_live_commit_gib": 0.0, "pending_gib": 0.0, "pending_commit_gib": 0.0,
		"admits_up_to_gib": 119.7, "touch_room_gib": 61.0, "commit_room_gib": 123.7, "commit_limit_enforced": true}
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

// OVER: a line the grant applies is gone, here the commit charge at an enforced commit limit (and the available memory under the
// headroom). The block says so with the note, and the brief LEADS with it, ahead of the lease verdict word.
func TestStatusNamesOverLoudlyOnTheBriefLine(t *testing.T) {
	withHost(t, gpuprobe.HostMemory{PhysicalGiB: 127.7, AvailableGiB: 3.2, CommitUsedGiB: 187.9, CommitLimitGiB: 187.7}, true)
	view := localLeaseView(context.Background(), freeLeaseConfig(t))
	hm := hostBlock(t, view)
	if hm["verdict"] != "OVER" {
		t.Fatalf("a commit charge of 187.9 GiB on a 187.7 GiB commit limit is OVER, got %v", hm["verdict"])
	}
	// The block says what the reading shows (which line, and its numbers) and admits what it cannot
	// know: whether the box is paging now needs page-file growth, which nothing here reads.
	if note, _ := hm["note"].(string); !strings.Contains(note, "committed 187.9 of the 187.7 GiB commit limit") ||
		!strings.Contains(note, "cannot say whether the box is paging now") || strings.Contains(note, "never an acceptable state") {
		t.Errorf("the block must state the excess and nothing it cannot know: %q", note)
	}
	line := gpuLeaseVerdictLine(view)
	if !strings.HasPrefix(line, "HOST RAM OVER (3.2 of 127.7 GiB physical RAM is available; committed 187.9 of the 187.7 GiB commit limit, 8.0 GiB headroom); ") {
		t.Fatalf("the brief line must lead with the OVER verdict, got:\n%s", line)
	}
	if strings.Contains(line, "paging") {
		t.Errorf("the brief line asserts paging the code cannot know: %s", line)
	}
	if !strings.Contains(line, "free") || !strings.Contains(line, "queue with:") {
		t.Errorf("the rest of the line is unchanged:\n%s", line)
	}
}

func TestStatusNamesNearAsATailAndUnknownNotAtAll(t *testing.T) {
	// 5 GiB available with an 8 GiB headroom: any lease that declares host RAM waits. Commit is far from its limit.
	withHost(t, gpuprobe.HostMemory{PhysicalGiB: 100, AvailableGiB: 5, CommitUsedGiB: 95, CommitLimitGiB: 160}, true)
	view := localLeaseView(context.Background(), freeLeaseConfig(t))
	if hostBlock(t, view)["verdict"] != "NEAR" {
		t.Fatalf("5 GiB available with 8 GiB of headroom is NEAR, got %v", hostBlock(t, view)["verdict"])
	}
	line := gpuLeaseVerdictLine(view)
	if strings.HasPrefix(line, "HOST RAM") || !strings.Contains(line, "host RAM NEAR the limit (5.0 of 100.0 GiB physical RAM is available, 8.0 GiB headroom)") {
		t.Errorf("NEAR is a tail, not a lead:\n%s", line)
	}

	// The commit line: 155 of a 160 GiB limit is inside the headroom; with the limit not enforced (Linux heuristic overcommit) it is no line.
	withHost(t, gpuprobe.HostMemory{PhysicalGiB: 100, AvailableGiB: 60, CommitUsedGiB: 155, CommitLimitGiB: 160}, true)
	view = localLeaseView(context.Background(), freeLeaseConfig(t))
	if hostBlock(t, view)["verdict"] != "NEAR" || !strings.Contains(gpuLeaseVerdictLine(view), "host RAM NEAR the limit (committed 155.0 of the 160.0 GiB commit limit, 8.0 GiB headroom)") {
		t.Errorf("a commit charge inside the headroom of an enforced limit is NEAR:\n%s", gpuLeaseVerdictLine(view))
	}
	withHost(t, gpuprobe.HostMemory{PhysicalGiB: 100, AvailableGiB: 60, CommitUsedGiB: 155, CommitLimitGiB: 160, CommitLimitSoft: true}, true)
	view = localLeaseView(context.Background(), freeLeaseConfig(t))
	if hm := hostBlock(t, view); hm["verdict"] != "OK" || hm["commit_limit_enforced"] != false {
		t.Errorf("the same charge against a limit the OS does not enforce is OK, got %v", hm)
	}

	// The reading of 2026-10-09 (3.2 GiB available, 162.9 GiB committed of a 187.7 GiB limit) is NEAR: the available line is gone
	// under the headroom, the commit limit is not reached.
	withHost(t, gpuprobe.HostMemory{PhysicalGiB: 127.7, AvailableGiB: 3.2, CommitUsedGiB: 162.9, CommitLimitGiB: 187.7}, true)
	view = localLeaseView(context.Background(), freeLeaseConfig(t))
	if hostBlock(t, view)["verdict"] != "NEAR" || !strings.Contains(gpuLeaseVerdictLine(view), "host RAM NEAR the limit (3.2 of 127.7 GiB physical RAM is available, 8.0 GiB headroom)") {
		t.Errorf("the incident reading is NEAR on the available line:\n%s", gpuLeaseVerdictLine(view))
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
