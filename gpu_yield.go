package main

// The holders' side of the yield request (plan P11, ADR 0085): the shell that runs a job's own
// --on-yield line, and the one place a holder builds its yield ticker. The decision of WHEN to ask
// lives in internal/gpulease (yield.go); this file only starts the line the job recorded.
//
// SEAM. Behaviour-free for now: the runner starts nothing and reports exit 0.

import (
	"io"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// yieldShell is the runner the holders hand their yield ticker. A variable so a test substitutes it.
var yieldShell gpulease.YieldRunner = runYieldShell

func runYieldShell(line string, env []string) (int, error) { return 0, nil }

// newHolderYieldTicker is the ticker a holder runs beside its term check.
func newHolderYieldTicker(lease *gpulease.Lease, onYield, label string, out io.Writer) *gpulease.YieldTicker {
	return lease.NewYieldTicker(gpulease.YieldTickerOptions{OnYield: onYield, Run: yieldShell, Out: out, Label: label})
}
