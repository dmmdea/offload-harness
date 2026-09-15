// leasewindow_test.go pins the DECLARED WINDOW on a lease refusal (register D-110).
//
// The refusal is filed by the delegate path as a capacity defer ("gpu busy: " +
// this message), and a caller reading it has exactly one decision to make: retry
// now, route elsewhere, or come back later. "a text job holds the GPU" answers
// none of that. The holder stamped an end time when it acquired the lease, and
// gpulease.ErrHeld already renders it — so the wait's own refusal must too.

package modelaffinity

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// TestLeaseErrorNamesTheDeclaredWindow: an exclusive text hold with a declared
// end produces a refusal carrying that end AND what is left of it.
func TestLeaseErrorNamesTheDeclaredWindow(t *testing.T) {
	exp := time.Now().Add(37 * time.Minute)
	info := gpulease.Info{
		Held:      true,
		Class:     gpulease.ClassText,
		PID:       4242,
		Age:       2 * time.Minute,
		Reason:    "5070 Ti bench",
		Exclusive: true,
		ExpiresAt: exp,
	}
	msg := leaseError("http://127.0.0.1:1", "agent-pool", info, 5*time.Minute, 5*time.Minute, context.DeadlineExceeded).Error()
	for _, want := range []string{
		"gpu-lease timeout after 5m0s",
		"declared until " + exp.Local().Format(time.Kitchen),
		"~37m0s left",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("a refusal must carry %q so the caller knows when to retry: %q", want, msg)
		}
	}

	// The DRAINING shape carries it too: the cordon is the lane that hurts most
	// (no new run starts for the lease's whole length), so it is the one that
	// most needs to say how long that is.
	info.Exclusive, info.Draining = false, true
	dmsg := leaseError("http://127.0.0.1:1", "agent-pool", info, time.Minute, 5*time.Minute, context.DeadlineExceeded).Error()
	if !strings.Contains(dmsg, "draining the seat") || !strings.Contains(dmsg, "declared until "+exp.Local().Format(time.Kitchen)) {
		t.Errorf("the draining refusal must carry the declared window: %q", dmsg)
	}

	// A holder that declared nothing invents nothing: no window, no "~ left".
	info.ExpiresAt = time.Time{}
	nmsg := leaseError("http://127.0.0.1:1", "agent-pool", info, time.Minute, 5*time.Minute, context.DeadlineExceeded).Error()
	if strings.Contains(nmsg, "declared until") {
		t.Errorf("an undeclared window must not be rendered: %q", nmsg)
	}
}
