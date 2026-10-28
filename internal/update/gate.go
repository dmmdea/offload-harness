package update

import (
	"context"
	"errors"
)

// Verdict is a gate's answer about a staged build.
type Verdict int

const (
	// GatePass lets the build be promoted.
	GatePass Verdict = iota
	// GateReject refuses the build: it is recorded in rejected[] and never retried.
	GateReject
	// GateInconclusive means the gate could not run (a lease timeout, a busy card): nothing changes and the next
	// run tries again. It is not a rejection.
	GateInconclusive
)

// GateInput is what a gate may look at: the candidate, the staged generation directory (already verified,
// extracted and offline self-checked) and the directory of the generation now serving, so a measuring gate can run
// both back to back.
type GateInput struct {
	Component   string
	Candidate   *Candidate
	StagedDir   string
	PreviousDir string // "" when nothing is installed
	Live        string
}

// GateResult is a gate's verdict with the reason and any numbers it measured (recorded in the generation's meta file).
type GateResult struct {
	Verdict Verdict
	Reason  string
	Numbers map[string]float64
}

// Gate is the seam between "the staged build passed the offline self-check" and "it is promoted". The default
// (OfflineGate) adds nothing to the offline self-check, which the verb always runs first. The side-port smoke
// (a completion on a side port under the GPU lease, compared back to back with the previous generation) is a
// later task and plugs in here through Env.Gate; it must return GateInconclusive, not GateReject, when it cannot
// get the lease.
type Gate interface {
	Name() string
	Run(ctx context.Context, in GateInput) GateResult
}

// OfflineGate is the default gate: only the offline self-check, already done, stands between staging and promotion.
type OfflineGate struct{}

// Name implements Gate.
func (OfflineGate) Name() string { return "offline self-check only" }

// Run implements Gate.
func (OfflineGate) Run(context.Context, GateInput) GateResult {
	return GateResult{Verdict: GatePass, Reason: "offline self-check passed; no smoke gate installed"}
}

// softError is a failure after the flip that says nothing about the build (a timeout, a card another process holds,
// a seat that stayed busy): the build is rolled back but not rejected, so the next run tries it again.
type softError struct{ error }

func (s softError) Unwrap() error { return s.error }

// isSoft reports whether err is a softError.
func isSoft(err error) bool {
	var s softError
	return errors.As(err, &s)
}
