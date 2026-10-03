package gpulock

import (
	"os"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// A directory holding only card-scoped (v2) leases is HELD for the read-only vision
// gate. The gate has no device set, so it takes the conservative reading: any live
// lease fences it, exactly as a whole-node lease does today. Reading it as free
// (what a pre-v2 reader does) would fire a doomed vision call into a busy card.
func TestInspectSeesADeviceOnlyDirectoryAsHeld(t *testing.T) {
	m, err := gpulease.OpenAt("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.SetCardScoped(true)
	a, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "a", Devices: []string{"gpu-test-0"}, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "b", Devices: []string{"gpu-test-1"}, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	dir := m.Root() + string(os.PathSeparator) + "gpu" + string(os.PathSeparator) + "lease"

	info := Inspect(dir)
	if !info.Held || info.Class != string(gpulease.ClassMedia) || info.PID != os.Getpid() {
		t.Fatalf("Inspect over a v2-only directory = %+v, want held", info)
	}
	// The per-epoch fence the callers apply to an inherited lease: every live epoch
	// passes, a released one does not.
	if !EpochIsCurrent(dir, a.Epoch()) || !EpochIsCurrent(dir, b.Epoch()) {
		t.Fatal("EpochIsCurrent must pass for every live epoch")
	}
	_ = a.Release()
	if EpochIsCurrent(dir, a.Epoch()) {
		t.Fatal("a released epoch must not read current")
	}
	if !Inspect(dir).Held {
		t.Fatal("the surviving lease must still read held")
	}
	_ = b.Release()
	if Inspect(dir).Held {
		t.Fatal("released directory reads held")
	}
}
