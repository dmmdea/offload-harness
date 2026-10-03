package modelaffinity

import (
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// See delegate.TestForeignFenceExemptsOnlyTheLeaseTheProcessRunsUnder: the same rule at
// the admission gate. The exemption is per lease, and the gate must look at every live
// lease, not only the lowest.

func twoLeases(lo, hi gpulease.Info) gpulease.Info {
	lo.Held, hi.Held = true, true
	lo.Epochs, hi.Epochs = []uint64{lo.Epoch}, []uint64{hi.Epoch}
	sum := lo
	sum.Epochs = []uint64{lo.Epoch, hi.Epoch}
	sum.Leases = []gpulease.Info{lo, hi}
	return sum
}

func TestGateExemptsOnlyTheLeaseTheProcessRunsUnder(t *testing.T) {
	media7 := gpulease.Info{Epoch: 7, Class: gpulease.ClassMedia, Devices: []string{"card0"}}
	media9 := gpulease.Info{Epoch: 9, Class: gpulease.ClassMedia, Devices: []string{"card1"}}
	info := twoLeases(media7, media9)
	for _, env := range []string{"7", "9", "8", ""} {
		t.Setenv("GPU_LEASE_EPOCH", env)
		if !blocksLoad(info) || !BlocksNewRun(info) {
			t.Errorf("GPU_LEASE_EPOCH=%q: a process under one media lease is still blocked by the other", env)
		}
	}
	// Only one of the two leases blocks: its own child passes, everyone else is blocked.
	plain9 := gpulease.Info{Epoch: 9, Class: gpulease.ClassText, Devices: []string{"card1"}}
	info = twoLeases(media7, plain9)
	t.Setenv("GPU_LEASE_EPOCH", "7")
	if blocksLoad(info) || BlocksNewRun(info) {
		t.Error("a child of the only blocking lease must be admitted")
	}
	t.Setenv("GPU_LEASE_EPOCH", "9")
	if !blocksLoad(info) {
		t.Error("a child of the plain text reservation is blocked by the media lease")
	}
	t.Setenv("GPU_LEASE_EPOCH", "")
	if !blocksLoad(info) {
		t.Error("a foreign caller is blocked by the media lease")
	}
}

// The summary is only the lowest lease; a blocking lease above it must still block.
func TestGateSeesABlockingLeaseThatIsNotTheLowest(t *testing.T) {
	plain7 := gpulease.Info{Epoch: 7, Class: gpulease.ClassText, Devices: []string{"card0"}}
	media9 := gpulease.Info{Epoch: 9, Class: gpulease.ClassMedia, Devices: []string{"card1"}}
	info := twoLeases(plain7, media9)
	t.Setenv("GPU_LEASE_EPOCH", "")
	if !blocksLoad(info) || !BlocksNewRun(info) {
		t.Fatal("a media lease at epoch 9 must block even when the lowest live lease does not")
	}
	// A draining hold above the lowest also stops NEW runs only.
	drain9 := gpulease.Info{Epoch: 9, Class: gpulease.ClassText, Draining: true, Devices: []string{"card1"}}
	info = twoLeases(plain7, drain9)
	if blocksLoad(info) {
		t.Fatal("a draining hold never blocks a load")
	}
	if !BlocksNewRun(info) {
		t.Fatal("a draining hold above the lowest lease must stop new runs")
	}
}

func TestGateSingleLeaseIsUnchanged(t *testing.T) {
	info := gpulease.Info{Held: true, Class: gpulease.ClassMedia, Epoch: 7, Epochs: []uint64{7}}
	t.Setenv("GPU_LEASE_EPOCH", "7")
	if blocksLoad(info) {
		t.Fatal("the holder's own child is admitted under its own lease")
	}
	t.Setenv("GPU_LEASE_EPOCH", "8")
	if !blocksLoad(info) {
		t.Fatal("a stale epoch exempts nothing")
	}
}

// The refusal must NAME the lease that refuses, not the lowest one.
func TestBlockingLeaseNamesTheRefusingLease(t *testing.T) {
	plain7 := gpulease.Info{Epoch: 7, Class: gpulease.ClassText, PID: 70, Devices: []string{"card0"}}
	media9 := gpulease.Info{Epoch: 9, Class: gpulease.ClassMedia, PID: 90, Devices: []string{"card1"}}
	info := twoLeases(plain7, media9)
	t.Setenv("GPU_LEASE_EPOCH", "")
	got := blockingLease(info, blocksLoad)
	if got.Epoch != 9 || got.PID != 90 {
		t.Fatalf("the error must name the blocking lease (epoch 9), got %+v", got)
	}
	single := gpulease.Info{Held: true, Epoch: 7, Class: gpulease.ClassMedia, Epochs: []uint64{7}}
	if got := blockingLease(single, blocksLoad); got.Epoch != 7 {
		t.Fatalf("one lease names itself, got %+v", got)
	}
}
