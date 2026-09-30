// deal_local_room_test.go: how many more runs the local seat's run-cap line takes
// (localRunCapRoom) - the number the spread deal counts against the seat the way it
// counts a remote's headroom.

package delegate

import (
	"strings"
	"testing"

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
}
