package gpulease

import (
	"math"
	"testing"
)

// DisplayFloorGiB and DisplayBudgetGiB are the allocator's display-card arithmetic as pure functions, so the
// surfaces that report the cap (the card table's display_cap row, audit-yaml's display-cap-unseen note) use the
// ONE rule the allocator admits by. These tests pin the numbers themselves: a stub that returns (0, false) would
// pass a table of "no answer" rows, so every row that has an answer asserts ok.

const budgetTol = 1e-9

func TestDisplayFloorAndBudgetAreTheAllocatorsArithmetic(t *testing.T) {
	for _, tc := range []struct {
		name         string
		total        float64
		capPct       int
		declared     float64
		wantFloor    float64
		wantBudget   float64
		wantFloorOK  bool
		wantBudgetOK bool
	}{
		// 15.9 GiB at 60 percent: 40 percent of the card (6.36) is the desktop's, 9.54 is a job's.
		{"the default cap", 15.9, 60, 0, 6.36, 9.54, true, true},
		// A declared floor larger than the rest of the card binds: 7 beats 6.36, so the budget is 8.9.
		{"a stricter declared floor binds", 15.9, 60, 7, 7, 8.9, true, true},
		// A declared floor smaller than the rest of the card is not the floor: 6.36 stays.
		{"a laxer declared floor does not bind", 15.9, 60, 2, 6.36, 9.54, true, true},
		// 100 percent hands over the whole card, less only what the display layer declared.
		{"a cap of 100 with no floor is the whole card", 15.9, 100, 0, 0, 15.9, true, true},
		{"a cap of 100 keeps a declared floor", 15.9, 100, 5, 5, 10.9, true, true},
		// 0 closes the card: the floor is the whole card and nothing is left to a job.
		{"a cap of 0 leaves no budget", 15.9, 0, 0, 15.9, 0, true, true},
		// A floor past the card's total leaves no budget, never a negative one.
		{"a floor past the total is a budget of zero", 15.9, 60, 20, 20, 0, true, true},
		// No total: a cap below 100 needs the total to mean anything, so there is no answer.
		{"no total at a cap below 100", 0, 60, 0, 0, 0, false, false},
		// The allocator's own rule returns the declared floor at 100 even with no total; a budget needs the total.
		{"no total at a cap of 100", 0, 100, 3, 3, 0, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			floor, fok := DisplayFloorGiB(tc.total, tc.capPct, tc.declared)
			if fok != tc.wantFloorOK || math.Abs(floor-tc.wantFloor) > budgetTol {
				t.Errorf("DisplayFloorGiB(%v, %d, %v) = (%v, %v), want (%v, %v)", tc.total, tc.capPct, tc.declared, floor, fok, tc.wantFloor, tc.wantFloorOK)
			}
			budget, bok := DisplayBudgetGiB(tc.total, tc.capPct, tc.declared)
			if bok != tc.wantBudgetOK || math.Abs(budget-tc.wantBudget) > budgetTol {
				t.Errorf("DisplayBudgetGiB(%v, %d, %v) = (%v, %v), want (%v, %v)", tc.total, tc.capPct, tc.declared, budget, bok, tc.wantBudget, tc.wantBudgetOK)
			}
		})
	}
}
