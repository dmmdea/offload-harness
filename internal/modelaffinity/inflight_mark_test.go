package modelaffinity

import (
	"context"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/seatinflight"
)

// TestAdmitMarksHarnessRequest pins the PAIR seat watcher's dedupe at its
// source: every on-box admission is one seatinflight marker for exactly as
// long as the ticket is held, and an off-box admission (the request loads
// nothing here) marks nothing.
func TestAdmitMarksHarnessRequest(t *testing.T) {
	d := t.TempDir()
	seatinflight.ArmAt(d)
	t.Cleanup(seatinflight.Disarm)
	alive := func(int) bool { return true }
	count := func() int { return seatinflight.Count(d, time.Now(), alive)["agent-pool"] }

	base := testBase(t)
	tk, err := Admit(context.Background(), base, "agent-pool", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Fatalf("markers while the on-box ticket is held = %d, want 1", n)
	}
	tk.Release()
	if n := count(); n != 0 {
		t.Fatalf("markers after Release = %d, want 0", n)
	}

	off, err := AdmitOffBox(context.Background(), testBase(t)+"/off", "agent-pool", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer off.Release()
	if n := count(); n != 0 {
		t.Fatalf("an off-box admission wrote %d marker(s)", n)
	}
}
