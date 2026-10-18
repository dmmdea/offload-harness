package gpucards

import (
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// The room keys of the host_memory block tell an operator what a NEW lease could declare now, so each must subtract what the
// running leases have yet to load, in its own quantity, and the headroom. The reading: 100 GiB of RAM, a 160 GiB commit limit,
// 60 committed, 50 available; running leases declared 25 to touch and 30 to be charged, none of it loaded.
//
//	touch_room_gib            = available - pending touch - headroom                  = 50 - 25 - 8   = 17
//	commit_room_gib           = commit limit - committed - pending commit - headroom   = 160 - 60 - 30 - 8 = 62
//	unstated_commit_room_gib  = physical - committed - pending touch - headroom        = 100 - 60 - 25 - 8 = 7
//
// the last being the room under the line a lease that states no commit is held to.
func TestTheRoomKeysSubtractWhatRunningLeasesHaveYetToLoad(t *testing.T) {
	mem := gpuprobe.HostMemory{PhysicalGiB: 100, AvailableGiB: 50, CommitUsedGiB: 60, CommitLimitGiB: 160}
	declared := gpuprobe.HostLoad{TouchGiB: 25, CommitGiB: 30}
	got := NewHostView(mem, true, 8, declared, declared).Map()
	for k, want := range map[string]float64{"touch_room_gib": 17, "commit_room_gib": 62, "unstated_commit_room_gib": 7} {
		if got[k] != want {
			t.Errorf("%s = %v, want %v", k, got[k], want)
		}
	}
	// Nothing pending: the rooms are the plain differences.
	idle := NewHostView(mem, true, 8, gpuprobe.HostLoad{}, gpuprobe.HostLoad{}).Map()
	for k, want := range map[string]float64{"touch_room_gib": 42, "commit_room_gib": 92, "unstated_commit_room_gib": 32} {
		if idle[k] != want {
			t.Errorf("idle: %s = %v, want %v", k, idle[k], want)
		}
	}
}

// A room never goes below zero, and a reading whose commit limit is not enforced has no commit room (the key is absent), while the
// room under the line a lease that states no commit is held to is there on every reading.
func TestTheRoomKeysFloorAtZeroAndTheCommitRoomNeedsAnEnforcedLimit(t *testing.T) {
	mem := gpuprobe.HostMemory{PhysicalGiB: 100, AvailableGiB: 20, CommitUsedGiB: 150, CommitLimitGiB: 160}
	pending := gpuprobe.HostLoad{TouchGiB: 30, CommitGiB: 30}
	got := NewHostView(mem, true, 8, pending, pending).Map()
	for _, k := range []string{"touch_room_gib", "commit_room_gib", "unstated_commit_room_gib"} {
		if got[k] != 0.0 {
			t.Errorf("%s = %v, want 0 (never negative)", k, got[k])
		}
	}
	mem.CommitLimitSoft = true
	soft := NewHostView(mem, true, 8, pending, pending).Map()
	if _, has := soft["commit_room_gib"]; has {
		t.Errorf("a limit the OS does not enforce has no commit room, got %v", soft["commit_room_gib"])
	}
	if v, has := soft["unstated_commit_room_gib"]; !has || v != 0.0 {
		t.Errorf("the room under physical RAM is on every reading, got %v (present=%v)", v, has)
	}
}
