package pipeline

import (
	"strconv"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// The fence an inherited lease passes at the process boundary (ambientLeaseEnv) is per
// epoch. With card-scoped leases a directory holds several leases at once, and Info.Epoch
// is only the lowest of them: comparing against it fenced out every higher-epoch lease's
// child, so a render started under the SECOND device lease refused to run.
func TestPipelineInheritedLeaseFencePerEpoch(t *testing.T) {
	m, err := gpulease.OpenAt("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.SetCardScoped(true)
	low, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "low", Devices: []string{"gpu-test-0"}, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	high, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "high", Devices: []string{"gpu-test-1"}, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	set := func(l *gpulease.Lease) {
		t.Setenv("GPU_LEASE_DIR", l.Dir())
		t.Setenv("GPU_LEASE_EPOCH", strconv.FormatUint(l.Epoch(), 10))
		t.Setenv("GPU_LEASE_CLASS", string(l.Class()))
	}

	set(high)
	if env, err := ambientLeaseEnv(); err != nil || len(env) != 3 {
		t.Fatalf("a child of the higher-epoch device lease was fenced out: env=%v err=%v", env, err)
	}
	set(low)
	if env, err := ambientLeaseEnv(); err != nil || len(env) != 3 {
		t.Fatalf("the lower-epoch lease: env=%v err=%v", env, err)
	}
	_ = low.Release()
	if _, err := ambientLeaseEnv(); err == nil {
		t.Fatal("a released lease must fence its late child out")
	}
	set(high)
	if _, err := ambientLeaseEnv(); err != nil {
		t.Fatalf("releasing the other lease fenced the survivor: %v", err)
	}
	_ = high.Release()
}
