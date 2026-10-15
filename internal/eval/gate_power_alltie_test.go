package eval

import (
	"math"
	"testing"
)

// An all-tie pass/fail record is read on Tango's interval, whose half-width at n tasks is
// z^2/(n+z^2), not a curve in 1/sqrt(n). The power note has to project with the interval the gate
// itself reads: 16 tied tasks read +-0.1936, and a 0.05 margin is first resolved at 73 ties (72 give
// 0.0507), so the note asks for 73 tasks. Projected the 1/sqrt(n) way it asked for 240, more than three
// times what the gate needs, and the next pair over-collects for nothing.
func TestGatePowerNoteAllTiePassFailUsesTheGatesOwnInterval(t *testing.T) {
	const n = 16
	p := PreReg{
		Mode:       ModeNonInferiority,
		Primary:    PreRegPrimary{Name: "pass_rate", Class: ClassUnitInterval, Margin: f64(0.05)},
		Judge:      Judge{Kind: "deterministic", Identity: scorerID},
		Efficiency: []PreRegEfficiency{{Name: tokensName, Class: EffTokens}},
		Tasks:      taskIDs(n),
	}
	b, h := freeze(t, p, nil)
	r := mustGate(t, b, h, inputFor(h, arm("i", repeatF(1, n), repeatF(1000, n)), arm("c", repeatF(1, n), repeatF(900, n))))
	z2 := normal975 * normal975
	tie := func(m float64) float64 { return z2 / (m + z2) }
	if math.Abs(r.Power.HalfWidth-tie(n)) > 1e-9 {
		t.Fatalf("setup: 16 ties read half-width %v, want z^2/(n+z^2) = %v", r.Power.HalfWidth, tie(n))
	}
	if r.Power.TasksNeeded.Unbounded || r.Power.TasksNeeded.N != 73 {
		t.Errorf("tasks_needed = %+v, want 73: the gate resolves a 0.05 margin at 73 ties (z^2/(73+z^2) = %.5f, at 72 %.5f)",
			r.Power.TasksNeeded, tie(73), tie(72))
	}
	if r.Power.TasksToAdd.Unbounded || r.Power.TasksToAdd.N != 73-n {
		t.Errorf("tasks_to_add = %+v, want %d", r.Power.TasksToAdd, 73-n)
	}
	for _, c := range []struct {
		m   float64
		got float64
	}{{16, r.Power.Projected.N16}, {24, r.Power.Projected.N24}, {120, r.Power.Projected.N120}} {
		if math.Abs(c.got-tie(c.m)) > 1e-9 {
			t.Errorf("projected[%v] = %v, want the gate's own all-tie half-width %v", c.m, c.got, tie(c.m))
		}
	}
}
