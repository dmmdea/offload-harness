package gpulease

import "testing"

// TestInheritedComparesTheEpoch: a process is inside a lease only when GPU_LEASE_EPOCH names that lease's
// epoch. Presence is not membership: an unset, malformed or zero variable, or another lease's epoch, is not
// the lease's own child.
func TestInheritedComparesTheEpoch(t *testing.T) {
	cases := []struct {
		env   string
		epoch uint64
		want  bool
	}{
		{"", 7, false},
		{"7", 7, true},
		{" 7 ", 7, true},
		{"8", 7, false},
		{"seven", 7, false},
		{"0", 0, false},
	}
	for _, c := range cases {
		t.Setenv("GPU_LEASE_EPOCH", c.env)
		if got := Inherited(Info{Held: true, Epoch: c.epoch}); got != c.want {
			t.Errorf("GPU_LEASE_EPOCH=%q, lease epoch %d: Inherited = %v, want %v", c.env, c.epoch, got, c.want)
		}
	}
}
