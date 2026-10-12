package pipeline

// A media call that queued was decided against the display card's cap and the VRAM in use at ENQUEUE.
// The line can be long, so the grant puts the same rule again from fresh readings
// (gpulease.Options.GrantCheck): a card that no longer qualifies is given back and the call chooses
// again, never starting its runner on the operator's screen. A media call declares no VRAM footprint,
// so the cap keeps the screen's card out of its choice altogether: it is never queued on that card, which
// the test below pins, and the grant-time half of the rule for the display card is exercised where a
// footprint is declared (gpu reserve --cards N --vram X, gpu_reserve_grantcheck_test.go).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

func TestAMediaCallThatDeclaresNoFootprintIsNotQueuedOnTheDisplayCard(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: func(c *config.Config) { c.GPUWaitMs = 300 }})

	// Only the display card could take it: the other two are quarantined (a previous holder's tree could
	// not be stopped), and the display card is free.
	for _, u := range []string{admitUUIDA, admitUUIDC} {
		if err := os.WriteFile(filepath.Join(f.m.Dir(), "quarantine."+leaseIDOf(u)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	res := f.await(f.image(nil))
	if res.OK {
		t.Fatalf("the call ran with only the screen's card to run on: %s", res.Reason)
	}
	if !strings.Contains(res.Reason, "card 1 vram (display card)") {
		t.Errorf("the answer must say it is the display card that was refused on VRAM, not a bare 'vram': %q", res.Reason)
	}
	if ws := f.m.Waiters(); len(ws) != 0 {
		t.Errorf("a call that cannot be shown to stay under the cap must not hold a place on the display card: %+v", ws)
	}
	if got := f.started(); len(got) != 0 {
		t.Fatalf("a runner started on %v: the display card must not have been granted", got)
	}
	if ls := f.m.Leases(); len(ls) != 0 {
		t.Fatalf("no lease may be left on the display card: %+v", ls)
	}
}
