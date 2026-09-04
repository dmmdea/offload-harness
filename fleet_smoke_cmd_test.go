package main

import "testing"

// TestExitError is the regression for `fleet-smoke --json` exiting 0 on
// failure: runFleetSmoke's --json branch used to return right after encoding
// the rows, before the same non-zero-exit check the table branch ran below
// it — so a real DEFER/FAIL row still exited 0 as long as --json was passed,
// exactly the mode a script actually checks. Both output branches now call
// this one extracted helper, so there is only one place left to get wrong.
func TestExitError(t *testing.T) {
	if err := exitError(nil); err != nil {
		t.Fatalf("exitError(nil) = %v, want nil", err)
	}
	if err := exitError([]smokeRow{}); err != nil {
		t.Fatalf("exitError(empty) = %v, want nil", err)
	}
	allPass := []smokeRow{{Base: "a", Verdict: "PASS"}, {Base: "b", Verdict: "PASS"}}
	if err := exitError(allPass); err != nil {
		t.Fatalf("exitError(all PASS) = %v, want nil", err)
	}
	oneFail := []smokeRow{{Base: "a", Verdict: "PASS"}, {Base: "b", Verdict: "FAIL"}}
	if err := exitError(oneFail); err == nil {
		t.Fatal("exitError with one FAIL row = nil, want a non-nil error")
	}
	oneDefer := []smokeRow{{Base: "a", Verdict: "DEFER"}}
	if err := exitError(oneDefer); err == nil {
		t.Fatal("exitError with one DEFER row = nil, want a non-nil error — a defer is real fleet signal, not a soft pass")
	}
	mixed := []smokeRow{{Base: "a", Verdict: "PASS"}, {Base: "b", Verdict: "FAIL"}, {Base: "c", Verdict: "DEFER"}}
	if err := exitError(mixed); err == nil {
		t.Fatal("exitError with mixed rows = nil, want a non-nil error")
	}
}
