// eta_reference_test.go: ADR 0079 - the ranking eta is cold + the node's own wait + the time to produce a
// REFERENCE final at the seat's measured rate, and it stops at no wall. It used to price the final fitted to the
// contract's wall and clamp the sum at the wall, so every seat that could not finish inside the wall read as the
// wall and ordering among the slow seats fell to queue estimates and the near-tie draw. Since ADR 0055 decision
// 2 the wall is an expectation, not a kill. Feasibility (feasibleFinal) is untouched and still reads the wall.

package delegate

import (
	"fmt"
	"math"
	"testing"

	"github.com/dmmdea/offload-harness/internal/seatrate"
)

// etaSeat is a loaded remote at tokS whose configured step budget is 8,192 tokens, the shape that made the old
// eta fill the wall: a one-step contract on it configures an 8,192-token final and re-pack, which the wall fit
// narrowed to ~0.9 x wall for every rate and the cap then pinned at the wall.
func etaSeat(id string, tokS float64) NodeView {
	v := etaFixtureRemote(id, 8192, tokS, 0)
	v.SeatBudget = &SeatBudgetView{StepTokens: 8192, Thinking: "off"}
	return v
}

// referenceGen is the generation the reference final costs a schema contract: the final and the re-pack, each
// seatrate.FinalBudgetFloor tokens, at tokS.
func referenceGen(tokS float64) float64 {
	return math.Ceil(2 * float64(seatrate.FinalBudgetFloor) / tokS)
}

// Seats that cannot finish inside the wall are ordered by their rate. At a 300 s wall and the 2 x 1,024 token
// reference, the 6.57 and 3.9 tok/s seats (312 s and 525 s) are past the wall; the cap read both as 300 s, and
// at 120 s every seat at or below 17 tok/s tied. The same holds under timeout_auto.
func TestEtaForRanksSeatsByRateWhenNoneCanFinishInsideTheWall(t *testing.T) {
	rates := []float64{43, 20, 12, 6.57, 3.9}
	for _, tc := range []struct {
		name string
		wall int
		auto bool
	}{
		{"an explicit 300 s wall", 300, false},
		{"an explicit 120 s wall", 120, false},
		{"timeout_auto", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := oneStepSchemaContract(tc.wall, tc.auto)
			prev, pastWall := 0.0, 0
			for i, r := range rates {
				eta, ok := etaFor(st, etaSeat(fmt.Sprintf("seat-%v", r), r))
				if !ok {
					t.Fatalf("%v tok/s: a published rate must yield an eta", r)
				}
				if i > 0 && !(eta > prev) {
					t.Errorf("%v tok/s reads eta %.0f s, not above the %v tok/s seat's %.0f s: seats are ordered by their rate, not flattened at the wall", r, eta, rates[i-1], prev)
				}
				if tc.wall > 0 && eta > float64(tc.wall) {
					pastWall++
				}
				prev = eta
			}
			if tc.wall > 0 && pastWall < 2 {
				t.Errorf("fixture bug: only %d of the seats read past the %d s wall; the case needs at least two for the cap to have flattened", pastWall, tc.wall)
			}
		})
	}
}

// 22.5 and 40 tok/s both read 270 s under the cap (the final fitted to 0.9 x a 300 s wall) and tied, though one
// is nearly twice as fast. Placement now sees the difference.
func TestEtaForSeparatesTwoSeatsTheOldCapTiedAtTheWall(t *testing.T) {
	st := oneStepSchemaContract(300, false)
	slow, fast := etaSeat("slow", 22.5), etaSeat("fast", 40)
	es, _ := etaFor(st, slow)
	ef, _ := etaFor(st, fast)
	if es != referenceGen(22.5) || ef != referenceGen(40) {
		t.Fatalf("etas = %.0f / %.0f s, want the reference generation %.0f / %.0f s at 22.5 / 40 tok/s", es, ef, referenceGen(22.5), referenceGen(40))
	}
	if !(ef < es) || (es-ef)/es <= p2cNearTieFrac {
		t.Fatalf("22.5 tok/s reads %.0f s and 40 tok/s %.0f s: they must differ by more than the %.0f%% near-tie band", es, ef, p2cNearTieFrac*100)
	}
}

// The contract's wall is not an input. The same seat and contract at any timeout_sec, or under timeout_auto,
// give the same cold, wait and generation.
func TestEtaForDoesNotDependOnTheContractsWall(t *testing.T) {
	for _, tokS := range []float64{40, 5.4} {
		v := etaSeat("seat", tokS)
		loaded := false
		v.SeatLoaded = &loaded
		v.SeatRate.ColdLoadSec = 69
		wait := 12.0
		v.NewJobWaitSec = &wait
		c0, w0, g0, ok := etaParts(oneStepSchemaContract(60, false), v)
		if !ok {
			t.Fatalf("%v tok/s: a published rate must yield eta parts", tokS)
		}
		for _, st := range []Subtask{oneStepSchemaContract(300, false), oneStepSchemaContract(900, false), oneStepSchemaContract(0, true), oneStepSchemaContract(0, false)} {
			c, w, g, _ := etaParts(st, v)
			if c != c0 || w != w0 || g != g0 {
				t.Errorf("%v tok/s at timeout_sec %d (auto=%v): parts cold %.0f wait %.0f gen %.0f, want those of a 60 s wall: cold %.0f wait %.0f gen %.0f",
					tokS, st.Contract.TimeoutSec, st.Contract.TimeoutAuto, c, w, g, c0, w0, g0)
			}
		}
	}
}

// A seat that publishes no usable rate has no opinion: the caller keeps the window-only ordering.
func TestEtaForHasNoOpinionWithoutARate(t *testing.T) {
	st := oneStepSchemaContract(300, false)
	noRate := etaSeat("no-rate", 20)
	noRate.SeatRate = nil
	zero := etaSeat("zero-tok", 20)
	zero.SeatRate.TokS = 0
	noSamples := etaSeat("no-samples", 20)
	noSamples.SeatRate.Samples = 0
	for _, v := range []NodeView{noRate, zero, noSamples} {
		if eta, ok := etaFor(st, v); ok || eta != 0 {
			t.Errorf("%s: etaFor = %v, %v; want no opinion", v.NodeID, eta, ok)
		}
		if c, w, g, ok := etaParts(st, v); ok || c != 0 || w != 0 || g != 0 {
			t.Errorf("%s: etaParts = %v %v %v %v; want no opinion", v.NodeID, c, w, g, ok)
		}
		if got := chosenVerdictDetail(st, v); got != "(no rate published)" {
			t.Errorf("%s: chosenVerdictDetail = %q, want the no-rate word", v.NodeID, got)
		}
	}
}

// etaFor is the three parts summed, and every term is in it.
func TestEtaForIsTheSumOfItsParts(t *testing.T) {
	st := oneStepSchemaContract(300, false)
	v := etaFixtureRemote("busy-cold", 8192, 34, 15) // not loaded: a 15 s cold load
	wait := 33.0
	v.NewJobWaitSec = &wait
	cold, w, gen, ok := etaParts(st, v)
	if !ok || cold != 15 || w != 33 || gen != referenceGen(34) {
		t.Fatalf("parts = cold %v wait %v gen %v ok=%v; want 15, 33, %v", cold, w, gen, ok, referenceGen(34))
	}
	if eta, _ := etaFor(st, v); eta != cold+w+gen {
		t.Fatalf("eta = %v, want cold + wait + gen = %v", eta, cold+w+gen)
	}
}

// placement_reason prints the winner's eta from the SAME parts the ranking compared. The printed total is
// cold + wait + gen and the breakdown names the cold load and the generation as etaParts computed them, so a
// node with a backlog prints the generation, not the generation plus whatever the wait leaves over.
func TestChosenVerdictDetailReportsColdWaitAndGenerationFromTheSameParts(t *testing.T) {
	st := oneStepSchemaContract(300, false)
	v := etaFixtureRemote("busy-cold", 8192, 34, 15)
	wait := 33.0
	v.NewJobWaitSec = &wait
	cold, w, gen, ok := etaParts(st, v)
	if !ok {
		t.Fatal("fixture: the seat publishes a rate")
	}
	want := fmt.Sprintf("eta %.0f s (cold %.0f + %.0f gen)", cold+w+gen, cold, gen)
	if got := chosenVerdictDetail(st, v); got != want {
		t.Fatalf("chosenVerdictDetail = %q, want %q", got, want)
	}
	if want != "eta 109 s (cold 15 + 61 gen)" {
		t.Fatalf("fixture drifted: the parts print %q, want eta 109 s (cold 15 + 61 gen)", want)
	}
	// A contract with no wall at all gets the same breakdown (it used to print the bare total).
	if got := chosenVerdictDetail(oneStepSchemaContract(0, false), v); got != want {
		t.Fatalf("a contract with no wall prints %q, want the same breakdown %q: the wall is no input", got, want)
	}
}

// End to end through Place: two seats that cannot finish inside the wall are not a coin flip. Under the cap both
// read the wall and the near-tie draw spread 100 job ids across them; now the faster seat takes them all.
func TestPlaceRanksTwoSeatsThatCannotFinishInsideTheWallByRate(t *testing.T) {
	st := oneStepSchemaContract(300, false)
	faster, slower := etaSeat("faster", 6.57), etaSeat("slower", 3.9)
	for _, v := range []NodeView{faster, slower} {
		if ok, reason := feasibleFinal(st, v); !ok {
			t.Fatalf("fixture bug: %s must be feasible on a 300 s wall (a ranking matter, not a refusal): %s", v.NodeID, reason)
		}
		if eta, _ := etaFor(st, v); eta <= 300 {
			t.Fatalf("fixture bug: %s reads %.0f s, inside the 300 s wall", v.NodeID, eta)
		}
	}
	for i := 0; i < 60; i++ {
		jobID := fmt.Sprintf("job-%02d", i)
		for _, roster := range [][]NodeView{{faster, slower}, {slower, faster}} {
			if got := Place(jobID, st, localNode(), roster, true); got.NodeID != "faster" {
				t.Fatalf("job %s: Place chose %q, want the 6.57 tok/s seat over the 3.9 tok/s one (roster order must not matter)", jobID, got.NodeID)
			}
		}
	}
}
