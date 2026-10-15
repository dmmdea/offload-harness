package main

// Round 5, item 3. The hold stamp on a lease's record is three different moments: the outer bound a detached reserve keeps moving while
// it clears the seat (the host has not been asked yet, and a live reserve never lets the lease go then), the end of the wait for the
// host, and the push-out of a reserve that is releasing the lease itself. `gpu status` read all of them as "released at HH:MM if the
// host still lacks the room", which during the drain and the unload is false on both counts. The record now says which phase the stamp
// is, and status and the JSON row say it.

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

func TestStatusDuringTheDrainAndTheUnloadDoesNotPromiseAReleaseForAHostThatWasNeverAsked(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useQuietStatus(t, statusCards(), nil)
	useHostCommit(t, newHostCommit(10))
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "film", TTL: time.Hour, Devices: []string{boxCard2}, HostRAMWaitGiB: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	statusText := func() string {
		return captureStdout(t, func() {
			if err := runGPUStatus([]string{"--config", cfg}); err != nil {
				t.Fatal(err)
			}
		})
	}
	phaseOfRow := func() (phase, until string) {
		row := statusJSON(t, cfg)["leases"].([]any)[0].(map[string]any)
		phase, _ = row["host_ram_hold_phase"].(string)
		until, _ = row["host_ram_hold_until"].(string)
		return phase, until
	}

	// The outer bound: a reserve that is draining and unloading keeps it moving.
	outer := startOuterBound(m, l.Epoch(), true, 10*time.Minute, time.Time{}, io.Discard)
	out := statusText()
	at := outer.Stop()
	if at.IsZero() {
		t.Fatal("the outer bound must have been stamped")
	}
	for _, want := range []string{"the seat is cleared first, then the host is asked", "if the reserve dies the holder releases about " + at.Format(time.Kitchen)} {
		if !strings.Contains(out, want) {
			t.Errorf("status during the drain and the unload must say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "if the host still lacks the room") || strings.Contains(out, "is released at") {
		t.Errorf("the host has not been asked yet, and a live reserve never releases the lease then; status must not promise a release for a short host:\n%s", out)
	}
	if phase, until := phaseOfRow(); phase != gpulease.HoldPhaseOuter || until == "" {
		t.Errorf("the lease row says which phase the stamp is: host_ram_hold_phase = %q (want %q), host_ram_hold_until = %q", phase, gpulease.HoldPhaseOuter, until)
	}

	// The wait begins: the stamp is the end of the wait, and the sentence is the one that was always true then.
	waitEnd := time.Now().Add(7 * time.Minute)
	if err := m.StampHostRAMHoldPhase(l.Epoch(), waitEnd, gpulease.HoldPhaseWait); err != nil {
		t.Fatal(err)
	}
	out = statusText()
	for _, want := range []string{"the lease is released at " + waitEnd.Format(time.Kitchen), "if the host still lacks the room"} {
		if !strings.Contains(out, want) {
			t.Errorf("status of a lease in the wait must say %q:\n%s", want, out)
		}
	}
	if phase, _ := phaseOfRow(); phase != gpulease.HoldPhaseWait {
		t.Errorf("host_ram_hold_phase = %q, want %q", phase, gpulease.HoldPhaseWait)
	}

	// The reserve is releasing the lease itself and has pushed the stamp out.
	pushed := time.Now().Add(15 * time.Minute)
	if err := m.StampHostRAMHoldPhase(l.Epoch(), pushed, gpulease.HoldPhaseRelease); err != nil {
		t.Fatal(err)
	}
	out = statusText()
	for _, want := range []string{"the reserve is releasing the lease", "if it dies the holder releases about " + pushed.Format(time.Kitchen)} {
		if !strings.Contains(out, want) {
			t.Errorf("status of a lease its reserve is releasing must say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "if the host still lacks the room") {
		t.Errorf("the wait is over; status must not say the lease waits on the host:\n%s", out)
	}
	if phase, _ := phaseOfRow(); phase != gpulease.HoldPhaseRelease {
		t.Errorf("host_ram_hold_phase = %q, want %q", phase, gpulease.HoldPhaseRelease)
	}

	// A stamp an older binary wrote carries no phase: it was always the end of the wait.
	if err := m.Restamp(l.Epoch(), func(mt *gpulease.Meta) { mt.HostRAMHoldUntilMs = waitEnd.UnixMilli(); mt.HostRAMHoldPhase = "" }); err != nil {
		t.Fatal(err)
	}
	if phase, _ := phaseOfRow(); phase != gpulease.HoldPhaseWait {
		t.Errorf("a stamp with no phase is the wait's: host_ram_hold_phase = %q, want %q", phase, gpulease.HoldPhaseWait)
	}
}
