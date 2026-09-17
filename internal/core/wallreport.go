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
