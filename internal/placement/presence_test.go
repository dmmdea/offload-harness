package placement

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestClassifyCoversEveryPresenceBranch(t *testing.T) {
	th := 15 * time.Minute
	cases := []struct {
		name   string
		locked bool
		idle   time.Duration
		quns   int32
		away   bool
		note   string
	}{
		{"locked wins regardless of idle or shell state", true, 0, qunsBusy, true, "locked"},
		{"shell says not present", false, 0, qunsNotPresent, true, "not present"},
		{"shell busy blocks even a long idle", false, time.Hour, qunsBusy, false, "busy"},
		{"D3D fullscreen blocks even a long idle", false, time.Hour, qunsD3DFullScreen, false, "fullscreen"},
		{"presentation mode blocks even a long idle", false, time.Hour, qunsPresentationMode, false, "presentation"},
		{"idle at the threshold is away", false, th, qunsAcceptsNotifications, true, "idle"},
		{"idle past the threshold is away", false, th + time.Second, qunsQuietTime, true, "idle"},
		{"idle under the threshold is at the desk", false, th - time.Second, qunsAcceptsNotifications, false, "idle"},
		{"an unrecognised shell state neither blocks nor admits on its own", false, th, 99, true, "idle"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			away, note := classify(tc.locked, tc.idle, tc.quns, th)
			if away != tc.away {
				t.Fatalf("away=%v want %v (%q)", away, tc.away, note)
			}
			if !strings.Contains(strings.ToLower(note), tc.note) {
				t.Fatalf("note %q must mention %q", note, tc.note)
			}
		})
	}
	// The unlock-state constant the probe compares against is the INVERTED
	// WTS value: 0 means locked. Pinned so a "fix" to the obvious 1 fails here.
	if wtsSessionStateLock != 0 || wtsSessionStateUnlock != 1 {
		t.Fatalf("WTS_SESSIONSTATE_LOCK is 0 and UNLOCK is 1 (inverted): got %d/%d", wtsSessionStateLock, wtsSessionStateUnlock)
	}
}

func TestAutoProbeIsUnknownWhereNoConsoleReaderExists(t *testing.T) {
	p := ProbePresence("auto", 0)
	if p.Mode != "auto" {
		t.Fatalf("mode must be echoed: %+v", p)
	}
	if runtime.GOOS != "windows" {
		if p.Known || p.Away {
			t.Fatalf("no console-session reader on %s: Known must be false and never away, got %+v", runtime.GOOS, p)
		}
		return
	}
	// On Windows the probe reads the real console; the only invariants are
	// that an unknown reading never claims away and a known one carries a note.
	if !p.Known && p.Away {
		t.Fatalf("an unknown reading must never claim away: %+v", p)
	}
	if p.Note == "" {
		t.Fatalf("every reading carries a note for the operator: %+v", p)
	}
}

func TestIdleThresholdZeroReadsAsTheConfigDefault(t *testing.T) {
	// A 0 threshold would call every keystroke gap "away"; the probe applies
	// the 15-minute default so an operator who never set operator_idle_sec is
	// still protected.
	if got := idleThreshold(0); got != 15*time.Minute {
		t.Fatalf("threshold 0 → 15 min, got %v", got)
	}
	if got := idleThreshold(90 * time.Second); got != 90*time.Second {
		t.Fatalf("an explicit threshold is kept, got %v", got)
	}
}
