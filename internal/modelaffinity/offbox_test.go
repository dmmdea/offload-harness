package modelaffinity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// Register C-41c. A cascade call that the remote lane sends to ANOTHER box loads
// nothing into this box's VRAM, so the machine-wide GPU lease has no say over it.
// Measured 2026-09-15 on 0.125.0: the lane fired ("cascade remote lane: gemma-4-e4b
// -> http://<node>:18811/fleet/chat (local GPU lease held)") and the call then sat
// the full two-minute lease bound in Admit → awaitCard → blocksLoad, which reads
// this box's lease whatever the endpoint, and deferred without ever dialling the
// node. An off-box admission keeps the in-process per-base queue and skips the
// card wait; the local base keeps waiting (the control).
func TestAdmitOffBoxIgnoresTheLocalLease(t *testing.T) {
	m := armLease(t)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "hero render", Origin: "pipeline"})
	if err != nil {
		t.Fatalf("acquire media: %v", err)
	}
	defer func() { _ = l.Release() }()

	start := time.Now()
	tk, aerr := AdmitOffBox(context.Background(), "http://node-b:18811", "gemma-4-e4b", 300*time.Millisecond)
	if aerr != nil {
		t.Fatalf("AdmitOffBox under a media lease = %v; a call bound for another box must not wait on this box's card", aerr)
	}
	if el := time.Since(start); el > leasePollInterval/2 {
		t.Fatalf("AdmitOffBox took %s — it waited on the local card", el)
	}
	tk.Release()

	// CONTROL: the same admission on THIS box's base still waits out its budget
	// and fails on the lease — the off-box exemption must not have widened.
	_, lerr := Admit(context.Background(), "http://127.0.0.1:11436", "gemma-4-e4b", 300*time.Millisecond)
	var le *LeaseError
	if !errors.As(lerr, &le) {
		t.Fatalf("Admit on the local base under a media lease = %v (%T), want a *LeaseError", lerr, lerr)
	}
}
