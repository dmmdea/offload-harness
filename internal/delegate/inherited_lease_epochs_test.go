package delegate

import (
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// With card-scoped leases several are live at once and Info.Epoch is only the lowest.
// The exemption for "this process runs under the lease" is PER LEASE: a child of lease A
// is exempt from A's fence and from nothing else. Reading it as "inside ANY live lease"
// let a child of one device lease skip the fence of another lease on other cards (an
// exclusive or draining text reservation), the permissive direction.

// twoLeases builds the Info a reader returns while two leases are live: the summary is
// the lowest epoch, Leases lists both.
func twoLeases(lo, hi gpulease.Info) gpulease.Info {
	lo.Held, hi.Held = true, true
	lo.Epochs, hi.Epochs = []uint64{lo.Epoch}, []uint64{hi.Epoch}
	sum := lo
	sum.Epochs = []uint64{lo.Epoch, hi.Epoch}
	sum.Leases = []gpulease.Info{lo, hi}
	return sum
}

func TestForeignFenceExemptsOnlyTheLeaseTheProcessRunsUnder(t *testing.T) {
	media7 := gpulease.Info{Epoch: 7, Class: gpulease.ClassMedia, Devices: []string{"card0"}}
	excl9 := gpulease.Info{Epoch: 9, Class: gpulease.ClassText, Exclusive: true, Devices: []string{"card1"}}
	info := twoLeases(media7, excl9)

	cases := []struct {
		env    string
		fenced bool
		why    string
	}{
		{"9", true, "a child of lease 9 is still fenced by lease 7 (a media render on other cards)"},
		{"7", true, "a child of lease 7 is still fenced by lease 9 (an exclusive text hold)"},
		{"8", true, "an epoch that is not live exempts nothing"},
		{"", true, "no inherited lease exempts nothing"},
	}
	for _, c := range cases {
		t.Setenv("GPU_LEASE_EPOCH", c.env)
		if fenced, _ := ForeignFence(info); fenced != c.fenced {
			t.Errorf("GPU_LEASE_EPOCH=%q: fenced=%v, want %v: %s", c.env, fenced, c.fenced, c.why)
		}
	}

	// A second lease that does not fence (a plain text reservation) leaves the first
	// lease's own child unfenced, and still fences everyone else.
	plain9 := gpulease.Info{Epoch: 9, Class: gpulease.ClassText, Devices: []string{"card1"}}
	info = twoLeases(media7, plain9)
	t.Setenv("GPU_LEASE_EPOCH", "7")
	if fenced, why := ForeignFence(info); fenced {
		t.Errorf("a child of the only FENCING lease must run (a plain text reservation does not fence): %s", why)
	}
	t.Setenv("GPU_LEASE_EPOCH", "9")
	if fenced, _ := ForeignFence(info); !fenced {
		t.Error("a child of the plain text reservation is still fenced by the media lease")
	}
	t.Setenv("GPU_LEASE_EPOCH", "")
	if fenced, _ := ForeignFence(info); !fenced {
		t.Error("a foreign caller is fenced by the media lease")
	}
}

// The summary is only the LOWEST lease. A fence held by a higher one must still fence.
func TestFenceSeesALeaseThatIsNotTheLowest(t *testing.T) {
	plain7 := gpulease.Info{Epoch: 7, Class: gpulease.ClassText, Devices: []string{"card0"}}
	media9 := gpulease.Info{Epoch: 9, Class: gpulease.ClassMedia, Devices: []string{"card1"}}
	info := twoLeases(plain7, media9)
	t.Setenv("GPU_LEASE_EPOCH", "")
	fenced, why := Fenced(info)
	if !fenced || why != "media render holds the cards" {
		t.Fatalf("a media lease at epoch 9 must fence even when the lowest live lease (a plain text hold) does not: %v %q", fenced, why)
	}
	if fenced, _ := ForeignFence(info); !fenced {
		t.Fatal("ForeignFence must see the higher lease too")
	}
}

func TestReservedIsPerLease(t *testing.T) {
	text7 := gpulease.Info{Epoch: 7, Class: gpulease.ClassText, Devices: []string{"card0"}}
	text9 := gpulease.Info{Epoch: 9, Class: gpulease.ClassText, Devices: []string{"card1"}}
	info := twoLeases(text7, text9)
	t.Setenv("GPU_LEASE_EPOCH", "7")
	if !Reserved(info) {
		t.Fatal("a child of text lease 7 is still reserved against by text lease 9")
	}
	t.Setenv("GPU_LEASE_EPOCH", "")
	if !Reserved(info) {
		t.Fatal("a foreign caller is reserved against")
	}
	// One lease: unchanged.
	one := gpulease.Info{Held: true, Epoch: 7, Class: gpulease.ClassText, Epochs: []uint64{7}}
	t.Setenv("GPU_LEASE_EPOCH", "7")
	if Reserved(one) {
		t.Fatal("the holder's own child is exempt from its own reservation")
	}
}

// One lease is exactly as before: the inherited epoch is compared, never presence-checked.
func TestInheritedLeaseSingleLeaseIsUnchanged(t *testing.T) {
	info := gpulease.Info{Held: true, Class: gpulease.ClassMedia, Epoch: 7, Epochs: []uint64{7}}
	t.Setenv("GPU_LEASE_EPOCH", "7")
	if fenced, _ := ForeignFence(info); fenced {
		t.Fatal("the holder's own child must not be fenced by its own lease")
	}
	t.Setenv("GPU_LEASE_EPOCH", "8")
	if fenced, _ := ForeignFence(info); !fenced {
		t.Fatal("a stale epoch exempts nothing")
	}
}
