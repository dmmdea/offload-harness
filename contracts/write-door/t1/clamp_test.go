package clamp

import "testing"

func TestClamp(t *testing.T) {
	cases := []struct {
		name       string
		v, lo, hi  int
		want       int
	}{
		{name: "inside", v: 5, lo: 0, hi: 10, want: 5},
		{name: "below", v: -3, lo: 0, hi: 10, want: 0},
		{name: "above", v: 42, lo: 0, hi: 10, want: 10},
		{name: "at hi", v: 10, lo: 0, hi: 10, want: 10},
	}
	for _, tc := range cases {
		if got := Clamp(tc.v, tc.lo, tc.hi); got != tc.want {
			t.Errorf("%s: Clamp(%d, %d, %d) = %d, want %d", tc.name, tc.v, tc.lo, tc.hi, got, tc.want)
		}
	}
}
