// deal_local_room_test.go: how many more runs the local seat's run-cap line takes
// (localRunCapRoom) - the number the spread deal counts against the seat the way it
// counts a remote's headroom.

package delegate

import (
	"path/filepath"
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
