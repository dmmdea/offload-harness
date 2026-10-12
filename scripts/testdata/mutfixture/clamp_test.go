package mutfixture

import "testing"

// TestClamp covers all three ways out of Clamp, so a mutation of either bound test goes red.
func TestClamp(t *testing.T) {
	cases := []struct{ v, lo, hi, want int }{
		{-5, 0, 10, 0},  // below the range
		{5, 0, 10, 5},   // inside it
		{15, 0, 10, 10}, // above it
	}
	for _, c := range cases {
		if got := Clamp(c.v, c.lo, c.hi); got != c.want {
			t.Errorf("Clamp(%d, %d, %d) = %d, want %d", c.v, c.lo, c.hi, got, c.want)
		}
	}
}

// TestBand deliberately never asks for the "small" band (1 to 9). That gap is real: the
// self-test's SURVIVED mutant moves that band's upper edge, and nothing here can notice.
func TestBand(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{-3, "negative"},
		{0, "zero"},
		{100, "large"},
	}
	for _, c := range cases {
		if got := Band(c.n); got != c.want {
			t.Errorf("Band(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}
