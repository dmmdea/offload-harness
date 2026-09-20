package core

import "context"

// The WALL REPORT (register D-116).
//
// A contract whose caller named no timeout_sec is stamped `timeout_auto` and
// the EXECUTING node sizes the wall from its own seat's measured rate, inside
// the wire bounds. Until this existed, that number reached the delegator only
// on the FINAL result (AgentWireResult.WallSec) — which is exactly the case a
// dead node never produces. So the delegator, which cannot see the wall the
// node chose, held its poll clock open to the cap for every auto contract; the
// 0.126.0 review wrote that down as an accepted cost.
//
// The report closes it from the node's side: the lane that stamps the wall
// publishes it, the fleet node writes it onto the job record, and the job's
// poll payload carries it while the job is still RUNNING. It is a context
// value rather than a parameter because the lane that knows the wall
// (pipeline.runAgentTask) and the lane that owns the job record
// (fleetnode.Server) are three call frames apart through an interface, and a
// node door that sets no reporter simply reports nothing — the previous
// behaviour, exactly.
// LiveProgress is a running job's liveness as the executing lane reports it
// and the fleet node publishes it on every poll (0.131.0): the delegator
// keeps polling while LastProgressMs keeps moving inside AllowanceMs.
type LiveProgress struct {
	Step           int     `json:"step,omitempty"`
	TokensOut      int     `json:"tokens_out,omitempty"`
	TokS           float64 `json:"tok_s,omitempty"`
	Phase          string  `json:"phase,omitempty"`
	LastProgressMs int64   `json:"last_progress_ms,omitempty"`
	AllowanceMs    int64   `json:"allowance_ms,omitempty"`
	CeilingSec     int     `json:"ceiling_sec,omitempty"`
}

type progressReportKey struct{}

// WithProgressReport installs the sink a lane reports its LiveProgress to —
// the fleet node's job record. Same shape as WithWallReport.
func WithProgressReport(ctx context.Context, fn func(LiveProgress)) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, progressReportKey{}, fn)
}

// ReportProgress hands p to the installed sink, if any.
func ReportProgress(ctx context.Context, p LiveProgress) {
	if fn, _ := ctx.Value(progressReportKey{}).(func(LiveProgress)); fn != nil {
		fn(p)
	}
}

type wallReportKey struct{}

// WithWallReport returns a context whose ReportWall calls fn. fn must be safe
// to call from the run's own goroutine and may be called more than once; the
// LAST value wins (a wall is stamped once today).
func WithWallReport(ctx context.Context, fn func(sec int)) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, wallReportKey{}, fn)
}

// ReportWall publishes the wall (in seconds) the run is executing under. A
// context with no reporter, or a non-positive wall, does nothing.
func ReportWall(ctx context.Context, sec int) {
	if sec <= 0 {
		return
	}
	if fn, _ := ctx.Value(wallReportKey{}).(func(sec int)); fn != nil {
		fn(sec)
	}
}
