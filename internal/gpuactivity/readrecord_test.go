package gpuactivity

import (
	"os"
	"testing"
	"time"
)

// A record caught mid-write (empty or torn for a few milliseconds) is still a
// run: List waits it out instead of reporting no run — the read that made the
// drain print a run-less line between two steps (0.129.2).
func TestListWaitsOutARecordCaughtMidWrite(t *testing.T) {
	reg := OpenAt(t.TempDir())
	h, err := reg.Begin(Run{Seat: "seat", Kind: "contract"})
	if err != nil {
		t.Fatal(err)
	}
	defer h.End()
	good, err := os.ReadFile(h.path)
	if err != nil {
		t.Fatal(err)
	}
	// Truncate the record as an in-place rewrite would, and complete it a
	// moment later — inside readRecord's retry window.
	if err := os.WriteFile(h.path, nil, 0o666); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(6 * time.Millisecond)
		_ = os.WriteFile(h.path, good, 0o666)
	}()
	runs := reg.OnSeat(time.Now(), "seat")
	if len(runs) != 1 || runs[0].Seat != "seat" {
		t.Fatalf("a record mid-write must still be listed once it completes, got %+v", runs)
	}
	// A record that stays unreadable past the retry window is skipped, not
	// invented — and not deleted while it is younger than the heartbeat TTL.
	if err := os.WriteFile(h.path, []byte("{"), 0o666); err != nil {
		t.Fatal(err)
	}
	if runs := reg.OnSeat(time.Now(), "seat"); len(runs) != 0 {
		t.Fatalf("a torn record past the retry window must not be listed: %+v", runs)
	}
	if _, err := os.Stat(h.path); err != nil {
		t.Fatalf("a young torn record must not be removed: %v", err)
	}
}
