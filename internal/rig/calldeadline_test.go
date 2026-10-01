package rig

// A subtask the whole-call deadline cut (ADR 0065) is not evidence about a seat's speed.
// Its reason opens with the stable prefix core.CallDeadlineReasonPrefix and used to land on
// the `timeout` axis ("the contract's wall ran out") because the wall-timeout pattern
// matches the bare word "deadline". A cut row for placed work keeps its node and seat, so
// it counted against that seat's timeout share — the rigger's weights, and any remedy read
// from them ("raise timeout_sec"), were steered by calls that simply ran out of time.

import (
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// cutRow is a delegation-log row of a subtask the call deadline cut.
func cutRow(id, reason string) Row {
	return row(id, func(r *Row) {
		r.Deferred = true
		r.DeferClass = core.DeferClassBudget
		r.Result.Reason = reason
		r.Result.StopReason = "canceled"
	})
}

// TestACallDeadlineCutIsItsOwnAxisNotASeatTimeout: every shape the delegator publishes.
func TestACallDeadlineCutIsItsOwnAxisNotASeatTimeout(t *testing.T) {
	for name, reason := range map[string]string{
		"running on the local seat": "call deadline reached; 3 unfinished — this subtask was still running on the local seat when the call's deadline passed; it was cancelled",
		"on a named node":           "call deadline reached; 3 unfinished — this subtask was still on node-a (job agd-1) when the call's deadline passed; it was not taken back from the node, and this call is no longer waiting for it; the run itself failed: withdraw not confirmed: HTTP 405: the node has no withdraw route (an older node)",
		"taken back by the node":    "call deadline reached; 3 unfinished — this subtask was still on node-a (job agd-1) when the call's deadline passed; the node confirmed it took the job back before it started, so it will not run there, and this call is no longer waiting for it",
		"not yet placed":            "call deadline reached; 3 unfinished — this subtask had not been placed on a seat when the call's deadline passed",
		"never started":             "call deadline reached; 3 unfinished — this subtask never started: the call's deadline passed first",
		"abandoned":                 "call deadline reached; 3 unfinished — this subtask did not stop within 250ms of the call's deadline passing; whatever it answers later is discarded (job agd-2)",
	} {
		v := Classify(cutRow(name, reason))
		if v.Axis != AxisCallDeadline {
			t.Errorf("%s: classified %s (evidence %q), want %s: a call that ran out of time says nothing about the seat", name, v.Axis, v.Evidence, AxisCallDeadline)
		}
		if !strings.HasPrefix(v.Evidence, core.CallDeadlineReasonPrefix) {
			t.Errorf("%s: evidence %q does not quote the reason", name, v.Evidence)
		}
	}
	// The neighbours are untouched: a real wall timeout is still a timeout, and a run that
	// reported its own infrastructure failure is still the seat's.
	if v := Classify(cutRow("wall", "wall timeout after 300s (the caller's deadline, not this node's ceiling)")); v.Axis != AxisTimeout {
		t.Errorf("a wall timeout classified %s, want %s", v.Axis, AxisTimeout)
	}
	infra := row("infra", func(r *Row) {
		r.Deferred = true
		r.DeferClass = core.DeferClassInfrastructure
		r.Result.Reason = "call deadline reached; 1 unfinished — this subtask was still running on the local seat when the call's deadline passed; the run itself reported infrastructure: llama-server unreachable"
	})
	if v := Classify(infra); v.Axis != AxisSeatInfra {
		t.Errorf("a cut that quotes an infrastructure verdict classified %s, want %s (the run said so itself)", v.Axis, AxisSeatInfra)
	}
}

// TestACallDeadlineCutDoesNotCountTowardTheSeatsTimeouts: through Build, the report a person
// reads.
func TestACallDeadlineCutDoesNotCountTowardTheSeatsTimeouts(t *testing.T) {
	rows := []Row{
		cutRow("cut-1", "call deadline reached; 2 unfinished — this subtask was still running on the local seat when the call's deadline passed; it was cancelled"),
		cutRow("cut-2", "call deadline reached; 2 unfinished — this subtask was still running on the local seat when the call's deadline passed; it was cancelled"),
		row("real-timeout", func(r *Row) {
			r.Deferred = true
			r.DeferClass = core.DeferClassBudget
			r.Result.Reason = "wall timeout after 300s"
			r.Result.StopReason = "error"
		}),
	}
	rep, err := Build(rows, "seat-a", "", time.Unix(1788600000, 0), time.Unix(1788800000, 0))
	if err != nil {
		t.Fatal(err)
	}
	hits := map[Axis]int{}
	for _, a := range rep.Axes {
		hits[a.Axis] = a.Hits
	}
	if hits[AxisTimeout] != 1 || hits[AxisCallDeadline] != 2 {
		t.Fatalf("timeout hits %d, call-deadline hits %d, want 1 real timeout and 2 cuts", hits[AxisTimeout], hits[AxisCallDeadline])
	}
}

// TestTheCallDeadlineAxisIsPublishedWithARemedy: the precedence the report prints and the
// remedy table both know the axis, and its remedy says the honest thing: not a rule matter.
func TestTheCallDeadlineAxisIsPublishedWithARemedy(t *testing.T) {
	seen := -1
	for i, a := range Precedence {
		if a == AxisCallDeadline {
			seen = i
		}
	}
	if seen < 1 || Precedence[seen-1] != AxisSeatInfra || Precedence[seen+1] != AxisTimeout {
		t.Fatalf("precedence %v: the call-deadline axis belongs between seat-infra (a verdict the run reported) and timeout (the wall the pattern would otherwise match)", Precedence)
	}
	rem, ok := Remedies[AxisCallDeadline]
	if !ok || rem.Applies != "none" || rem.Key != "" {
		t.Fatalf("remedy %+v, want the honest one: no rule shapes a call that ran out of time", rem)
	}
}
