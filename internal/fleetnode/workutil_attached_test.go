package fleetnode

import "testing"

// A host with card-scoped leases OFF advertises work_util_pct, and the delegator ranks nodes on
// it. A monitor that is merely plugged in (display_attached, true for the whole life of the box)
// is not a display in use, so it must not take its card out of that figure: a node whose only
// busy card is the monitor card, working for its own seat or lease, would advertise itself idle.
// The allocator reads display_attached (gpuprobe.ScreenCardUUIDs); the load figure does not.
func TestSamplerDoesNotTreatAnAttachedMonitorAsADisplayInUse(t *testing.T) {
	devices := nodeBDevices()
	devices[1].DisplayActive = false  // the screen sleeps ...
	devices[1].DisplayAttached = true // ... but the monitor is still plugged into card 1
	s := &Sampler{}
	s.sampleDevices(func() ([]GPUDevice, error) { return devices, nil })
	snap, ok := s.Load()
	if !ok {
		t.Fatal("no snapshot")
	}
	if len(snap.DisplayUUIDs) != 0 {
		t.Fatalf("display set = %v, want empty: an attached monitor is not a display in use", snap.DisplayUUIDs)
	}
}
