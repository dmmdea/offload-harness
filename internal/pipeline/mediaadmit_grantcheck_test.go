package pipeline

// A media call that queued on the display card while the operator was away was decided against the
// presence guard and the desktop floor at ENQUEUE. The line can be long, so the grant puts the same
// rule again from fresh readings (gpulease.Options.GrantCheck): a card that no longer qualifies is
// given back and the call chooses again, never starting its runner on the operator's screen.

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

func TestAQueuedMediaCallIsNotGrantedTheDisplayCardOnceTheOperatorIsBack(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: func(c *config.Config) { c.GPUWaitMs = 3000 }})
	var away atomic.Bool
	away.Store(true)
	f.p.alloc.Presence = func(config.Config) (bool, bool) { return true, away.Load() }

	// Only the display card qualifies: the other two are quarantined (a previous holder's tree could
	// not be stopped), so the call queues on the display card, held by another lease.
	for _, u := range []string{admitUUIDA, admitUUIDC} {
		if err := os.WriteFile(filepath.Join(f.m.Dir(), "quarantine."+leaseIDOf(u)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	holder, err := f.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "other render", Devices: []string{leaseIDOf(admitUUIDB)}, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	ch := f.image(nil)
	deadline := time.Now().Add(10 * time.Second)
	for len(f.m.Waiters()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(f.m.Waiters()) == 0 {
		t.Fatal("the call never joined the line for the display card")
	}
	away.Store(false) // the operator sits back down while the call waits
	if err := holder.Release(); err != nil {
		t.Fatal(err)
	}

	res := f.await(ch)
	if res.OK {
		t.Fatalf("the call ran on the operator's screen after they came back: %s", res.Reason)
	}
	if !strings.Contains(res.Reason, "display") {
		t.Errorf("the answer must say the display card is why: %q", res.Reason)
	}
	if got := f.started(); len(got) != 0 {
		t.Fatalf("a runner started on %v: the display card must not have been granted", got)
	}
	if ls := f.m.Leases(); len(ls) != 0 {
		t.Fatalf("no lease may be left on the display card: %+v", ls)
	}
}
