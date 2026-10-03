package gpuprobe

import "testing"

func TestRAMHeadroom(t *testing.T) {
	cases := []struct {
		name           string
		free           float64
		freeOK         bool
		need, headroom float64
		wantOK         bool
	}{
		{"fits with headroom to spare", 20, true, 8, 4, true},
		{"exactly the need plus headroom", 12, true, 8, 4, true},
		{"headroom eaten by the need", 11.9, true, 8, 4, false},
		{"no declared need still keeps the headroom", 3, true, 0, 4, false},
		{"no declared need and plenty free", 30, true, 0, 4, true},
		{"unknown free RAM with a declared need fails closed", 0, false, 8, 4, false},
		{"unknown free RAM with no need is not a reason to refuse", 0, false, 0, 4, true},
		{"zero headroom, zero need is always fine", 0.1, true, 0, 0, true},
	}
	for _, c := range cases {
		ok, why := RAMHeadroom(c.free, c.freeOK, c.need, c.headroom)
		if ok != c.wantOK {
			t.Errorf("%s: ok=%v want %v (%s)", c.name, ok, c.wantOK, why)
		}
		if !ok && why == "" {
			t.Errorf("%s: a refusal must say why", c.name)
		}
	}
}
