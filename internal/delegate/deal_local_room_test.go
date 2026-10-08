// deal_local_room_test.go: how many more runs the local seat's run-cap line takes
// (localRunCapRoom) - the number the spread deal counts against the seat the way it
// counts a remote's headroom.

package delegate

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
)

// TestLocalRunCapRoom: how many more runs the seat's line takes.
func TestLocalRunCapRoom(t *testing.T) {
	t.Run("no run cap configured is unlimited", func(t *testing.T) {
		r := &runner{cfg: testCfg(t)}
		r.cfg.FleetMaxConcurrentJobs = -1
		if room, _ := r.localRunCapRoom(); room != unlimitedHeadroom {
			t.Fatalf("room = %d, want unlimited", room)
		}
	})
	t.Run("an unset seat cannot be counted and reads as empty", func(t *testing.T) {
		r := &runner{}
		r.cfg.FleetMaxConcurrentJobs = 3
		if room, _ := r.localRunCapRoom(); room != 3 {
			t.Fatalf("room = %d, want the whole cap of 3", room)
		}
	})
	t.Run("registered runs take slots and a full line has none", func(t *testing.T) {
		r := &runner{cfg: testCfg(t)}
		r.cfg.FleetMaxConcurrentJobs = 2
		seat := r.cfg.AgentPlannerModel("")
		for want := 1; want >= 0; want-- {
			run := gpuactivity.Start(r.cfg.GPULockPath, r.cfg.StateDir, gpuactivity.Run{Seat: seat, Kind: "contract", Goal: "x", Phase: gpuactivity.PhaseRunning})
			if run == nil {
				t.Fatal("fixture: could not register a run")
			}
			t.Cleanup(run.End)
			room, note := r.localRunCapRoom()
			if room != want || !strings.Contains(note, "cap 2") {
				t.Fatalf("room = %d (%q), want %d", room, note, want)
			}
		}
		if free, _ := r.localSlotAhead(); free {
			t.Fatal("localSlotAhead reads a slot free on a full line")
		}
	})
	t.Run("an unreadable registry reads as an empty line, says so, and is logged once", func(t *testing.T) {
		logs := captureLog(t)
		r := &runner{cfg: testCfg(t)}
		r.cfg.FleetMaxConcurrentJobs = 3
		// A cloud-sync root is refused as a lease directory, so the registry will not open.
		r.cfg.GPULockPath = filepath.Join(t.TempDir(), "dropbox", "lease")
		for i := 0; i < 2; i++ {
			room, note := r.localRunCapRoom()
			if room != 3 || !strings.Contains(note, "unreadable") {
				t.Fatalf("room = %d note = %q, want the whole cap of 3 and a note that the registry was unreadable", room, note)
			}
		}
		if free, _ := r.readLocalSlot(); !free {
			t.Fatal("an unreadable registry must fail open: the real gate skips its wait in line when the registry will not open")
		}
		if n := strings.Count(logs.String(), "run registry could not be opened"); n != 1 {
			t.Fatalf("the fail-open was logged %d times, want once per run: %s", n, logs.String())
		}
	})
}

// TestLocalSlotAheadIsMemoisedForTheProbeTTL: a dozen subtasks waiting on the capacity wait read the run-cap
// line once per tick between them, not a dozen times - and a reading older than the TTL is read again. (Every
// wait test zeroes the memo to compress its tick, so none of them ever reaches the memoised path.)
func TestLocalSlotAheadIsMemoisedForTheProbeTTL(t *testing.T) {
	old := fetchViewsMemoTTL
	t.Cleanup(func() { fetchViewsMemoTTL = old })
	fetchViewsMemoTTL = 150 * time.Millisecond
	r := &runner{cfg: testCfg(t)}
	r.cfg.FleetMaxConcurrentJobs = 1
	if free, _ := r.localSlotAhead(); !free {
		t.Fatal("fixture: an empty line must have a slot")
	}
	run := gpuactivity.Start(r.cfg.GPULockPath, r.cfg.StateDir, gpuactivity.Run{Seat: r.cfg.AgentPlannerModel(""), Kind: "contract", Goal: "x", Phase: gpuactivity.PhaseRunning})
	if run == nil {
		t.Fatal("fixture: could not register a run")
	}
	t.Cleanup(run.End)
	if free, _ := r.localSlotAhead(); !free {
		t.Fatal("the line filled inside the memo window and the reading changed: the registry was read again")
	}
	time.Sleep(200 * time.Millisecond)
	if free, _ := r.localSlotAhead(); free {
		t.Fatal("a reading older than the TTL was served from the memo")
	}
}

// A seat pinned to ONE card shares that card with every other seat on it (plan P5): its run-cap
// line is the card's, so a run of another seat on the same card takes a slot, and a run on
// another card does not. A seat that spans cards keeps the per-seat line.
func TestRunCapCountsPerCardForSingleCardSeats(t *testing.T) {
	flagship := config.FlagshipFixture()
	newRunner := func(t *testing.T, planner string) *runner {
		t.Helper()
		cfg := testCfg(t)
		cfg.Layers, cfg.TierProfile, cfg.Tiers = flagship.Layers, flagship.TierProfile, flagship.Tiers
		cfg.AgentModel = planner
		cfg.FleetMaxConcurrentJobs = 3
		return &runner{cfg: cfg}
	}
	start := func(t *testing.T, r *runner, run gpuactivity.Run) {
		t.Helper()
		run.Kind, run.Goal, run.Phase = "contract", "x", gpuactivity.PhaseRunning
		h := gpuactivity.Start(r.cfg.GPULockPath, r.cfg.StateDir, run)
		if h == nil {
			t.Fatal("fixture: could not register a run")
		}
		t.Cleanup(h.End)
	}

	r := newRunner(t, "gemma-4-26b-agent") // the single layer's agent seat, pinned to card 0
	start(t, r, gpuactivity.Run{Seat: "gemma-4-26b-agent", Devices: []string{"0"}})
	start(t, r, gpuactivity.Run{Seat: "another-seat-on-card-0", Devices: []string{"0"}})
	start(t, r, gpuactivity.Run{Seat: "whisper-stt", Devices: []string{"2"}})
	room, note := r.localRunCapRoom()
	if room != 1 || !strings.Contains(note, "on card 0") || !strings.Contains(note, "2 run(s)") {
		t.Fatalf("room = %d (%q): the two runs on card 0 are the line, the run on card 2 is not", room, note)
	}

	// The flagship spans every card: per-seat, only its own run counts.
	r2 := newRunner(t, "agent-pool")
	start(t, r2, gpuactivity.Run{Seat: "agent-pool", Devices: []string{"0", "1", "2"}})
	start(t, r2, gpuactivity.Run{Seat: "another-seat-on-card-0", Devices: []string{"0"}})
	room, note = r2.localRunCapRoom()
	if room != 2 || !strings.Contains(note, "on seat agent-pool") {
		t.Fatalf("a seat that spans cards keeps the per-seat line: room = %d (%q), want 2", room, note)
	}
}

// The installer seeds fleet_max_concurrent_jobs 1 on a tier whose agent seat serves one slot
// (fleet_cap_seed_test.go in the root package derives which), and the same key is the delegator's
// own run-cap line on that box: a single-slot seat serves one request at a time, so the local seat
// takes ONE run and the rest of a call goes to the fleet. This is that second effect of the seed,
// pinned at the number the seed resolves to.
func TestACapOfOneGivesTheLocalSeatOneRunAtATime(t *testing.T) {
	r := &runner{cfg: testCfg(t)}
	r.cfg.FleetMaxConcurrentJobs = 1
	if room, note := r.localRunCapRoom(); room != 1 {
		t.Fatalf("an empty line at a cap of 1 has room %d (%q), want 1", room, note)
	}
	if r.atRunCap(0) || !r.atRunCap(1) {
		t.Fatalf("atRunCap(0)=%v atRunCap(1)=%v: the seat is idle with no run and at its line with one", r.atRunCap(0), r.atRunCap(1))
	}
	run := gpuactivity.Start(r.cfg.GPULockPath, r.cfg.StateDir, gpuactivity.Run{Seat: r.cfg.AgentPlannerModel(""), Kind: "contract", Goal: "x", Phase: gpuactivity.PhaseRunning})
	if run == nil {
		t.Fatal("fixture: could not register a run")
	}
	t.Cleanup(run.End)
	if room, note := r.localRunCapRoom(); room != 0 || !strings.Contains(note, "cap 1") {
		t.Fatalf("one run on the seat leaves room %d (%q), want 0: the second run of the call belongs to the fleet", room, note)
	}
	if free, _ := r.readLocalSlot(); free {
		t.Fatal("the local seat reads a slot free with one run on a cap of 1")
	}
}
