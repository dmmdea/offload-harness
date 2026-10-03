package gpulease

import "testing"

// Node's fence must agree with Go's over a card-scoped directory: every live epoch
// passes (the higher one is not fenced out by the lower), a released one does not.
func TestNodeFenceAgreesWithGoOverAV2Directory(t *testing.T) {
	m := testManagerAt(t, t.TempDir())
	m.SetCardScoped(true)
	low := mustDevices(t, m, card0)
	high := mustDevices(t, m, card1)
	defer func() { _ = high.Release() }()

	for _, l := range []*Lease{low, high} {
		if res := runNode(t, m.leaseDir(), l.Epoch()); !res.FencePasses {
			t.Fatalf("Node's fence rejected the LIVE device lease at epoch %d - every render under it would refuse to run", l.Epoch())
		}
	}
	if res := runNode(t, m.leaseDir(), high.Epoch()+5); res.FencePasses {
		t.Fatal("Node's fence accepted an epoch nobody holds")
	}
	if err := low.Release(); err != nil {
		t.Fatal(err)
	}
	if res := runNode(t, m.leaseDir(), low.Epoch()); res.FencePasses {
		t.Fatal("Node's fence passed against a released device lease")
	}
	if res := runNode(t, m.leaseDir(), high.Epoch()); !res.FencePasses {
		t.Fatal("releasing the lower lease fenced out the higher one in Node")
	}
}
