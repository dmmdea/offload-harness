package rig

// A busy fleet is a place in line, not a seat that failed and not a wall that ran out. Three shapes used to
// land on the wrong axis of the report an operator reads:
//
//   - a capacity defer ("capacity wait: no node had room ... the call's deadline") matched the wall-timeout
//     pattern on the bare word "deadline" and was filed under `timeout`, whose remedy is "raise timeout_sec",
//     a key that never bounds that wait (ADR 0073);
//   - a dispatch a node answered with 503 "queue full" is a refusal the delegator outwaits and re-places, and
//     it was filed under `seat-infra` ("fix the serving stack") although no seat was involved;
//   - the same refusal, once the delegator marks its row as a refusal, is not a failed subtask at all.
//
// Rows are built from the JSON a delegator writes, so the tests pin the wire shape as well as the verdict.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

func jsonRow(t *testing.T, s string) Row {
	t.Helper()
	var r Row
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		t.Fatalf("bad fixture %s: %v", s, err)
	}
	return r
}

// capacityRow is a delegation-log row of a subtask that ended as a capacity defer with the given reason.
func capacityRow(id, reason string) Row {
	return row(id, func(r *Row) {
		r.Deferred = true
		r.DeferClass = core.DeferClassCapacity
		r.Result.Reason = reason
		r.Result.StopReason = ""
		r.Result.Steps = 0
	})
}

// TestACapacityDeferIsItsOwnAxisNotATimeout: every wording a capacity wait ends with.
func TestACapacityDeferIsItsOwnAxisNotATimeout(t *testing.T) {
	for name, reason := range map[string]string{
		"held slot, the 0.180.0 wording": "capacity wait: no node had room within 2m0s (waited 2m0s; agent_placement_wait_sec=0; 0 refusal(s); waiting because: every remote with room is already dealt to its headroom; 3 subtask(s) of the call had not started, so this wait could not hold its run slot to the call's deadline) — re-run later, raise agent_placement_wait_sec, or add a node",
		"bounded by the call":            "capacity wait: no node had room before the call's deadline (waited 24m45s; the call had 25m0s left, less a 15s reserve; 2 refusal(s): a: 503) — re-run later or add a node",
		"no deadline word at all":        "capacity wait: no node had room within 2m0s (waited 2m0s; agent_placement_wait_sec=120; 0 refusal(s)) — re-run later or add a node",
		"shed":                           "shed (priority -1): no node had an idle slot for sheddable work (1 refusal(s): a: 503) — sheddable contracts take idle capacity only",
		"not started inside the reserve": "not started: the call's deadline left no room (the call had 9s left, no more than the 15s a placed job needs to run in)",
	} {
		v := Classify(capacityRow(name, reason))
		if v.Axis != Axis("capacity") {
			t.Errorf("%s: classified %q (evidence %q), want %q: the fleet was busy, which is neither a wall nor a seat", name, v.Axis, v.Evidence, "capacity")
		}
	}
}

// TestACapacityDeferNamesTheKnobsThatBoundIt: the remedy is not timeout_sec, which never bounds a capacity wait.
func TestACapacityDeferNamesTheKnobsThatBoundIt(t *testing.T) {
	rem, ok := Remedies[Axis("capacity")]
	if !ok {
		t.Fatalf("no remedy for the capacity axis")
	}
	if strings.Contains(rem.Key, "timeout_sec") {
		t.Errorf("remedy key %q points at timeout_sec, which never bounds the capacity wait", rem.Key)
	}
	for _, want := range []string{"agent_call_deadline_sec", "agent_placement_wait_sec"} {
		if !strings.Contains(rem.Key, want) {
			t.Errorf("remedy key %q does not name %s", rem.Key, want)
		}
	}
}

// TestACapacityAxisSitsAheadOfTimeout: the precedence the report prints puts it ahead of the pattern it must beat.
func TestACapacityAxisSitsAheadOfTimeout(t *testing.T) {
	idx := func(a Axis) int {
		for i, x := range Precedence {
			if x == a {
				return i
			}
		}
		return -1
	}
	c, to, si := idx(Axis("capacity")), idx(AxisTimeout), idx(AxisSeatInfra)
	if c < 0 || c > to || c < si {
		t.Fatalf("precedence %v: capacity must come after seat-infra (a class the run reported) and before timeout (the wall pattern matches the bare word deadline)", Precedence)
	}
}

// TestARealWallTimeoutAndASeatsOwnInfraStayWhereTheyWere: the neighbours of the new axis are untouched.
func TestARealWallTimeoutAndASeatsOwnInfraStayWhereTheyWere(t *testing.T) {
	if v := Classify(cutRow("wall", "wall timeout after 300s")); v.Axis != AxisTimeout {
		t.Errorf("a wall timeout classified %q, want timeout", v.Axis)
	}
	infra := row("infra", func(r *Row) {
		r.Deferred = true
		r.DeferClass = core.DeferClassInfrastructure
		r.Result.Reason = "capacity wait: the seat is down"
	})
	if v := Classify(infra); v.Axis != AxisSeatInfra {
		t.Errorf("an infrastructure-class row classified %q, want seat-infra: the class the run reported decides", v.Axis)
	}
}

const refusalRowJSON = `{"ts":1788700000,"job_id":"agd-1","node":"n1","seat":"seat-a","deferred":false,"acceptance_pass":false,` +
	`"error":"dispatch http://n1:18811/fleet/dispatch: status 503: {\"error\":\"queue full (2 jobs accepted+running, limit 2): retry later\"}",` +
	`"refusal":{"status":503,"retry_after_sec":133},"contract":{"goal":"g"}}`

// TestARefusedDispatchInsideAWaitIsNotAFailedSubtask: a row the delegator marked as a node's capacity refusal is a
// placement retry. It is not a bad row, it is not on any failure axis, and the report counts it on its own line.
func TestARefusedDispatchInsideAWaitIsNotAFailedSubtask(t *testing.T) {
	r := jsonRow(t, refusalRowJSON)
	if r.Bad() {
		t.Fatalf("a refused attempt that was re-placed counts as a bad row: %+v", r)
	}
	if v := Classify(r); v.Axis != "" {
		t.Errorf("a refused attempt classified %q, want no axis", v.Axis)
	}
	rows := []Row{
		r,
		row("landed", func(r *Row) { r.AcceptancePass = true }),
		row("real-failure", func(r *Row) { r.Result.Reason = "something else entirely" }),
	}
	rep, err := Build(rows, "seat-a", "", time.Unix(1788600000, 0), time.Unix(1788800000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Rows != 2 || rep.Bad != 1 {
		t.Errorf("rows %d bad %d, want 2 subtask rows and 1 bad: a refusal is an attempt, not a subtask", rep.Rows, rep.Bad)
	}
	wire, _ := json.Marshal(rep)
	var asMap map[string]any
	_ = json.Unmarshal(wire, &asMap)
	if n, _ := asMap["refusals"].(float64); n != 1 {
		t.Errorf("report refusals = %v, want the one refused attempt counted on its own line", asMap["refusals"])
	}
	for _, a := range rep.Axes {
		if a.Axis == AxisSeatInfra && a.Hits != 0 {
			t.Errorf("seat-infra hits %d: a refusal is not the serving stack's failure", a.Hits)
		}
	}
	if md := Markdown(rep); !strings.Contains(md, "refused") {
		t.Errorf("the markdown does not mention the refused attempts: %s", md)
	}
}

// TestAQueueFullRowFromBeforeTheMarkerIsCapacityNotSeatInfra: the window a report reads still holds rows written
// by delegators that did not mark refusals. Their error says a node answered 503 queue full: capacity, kept bad
// because the row cannot say whether the chain ended there.
func TestAQueueFullRowFromBeforeTheMarkerIsCapacityNotSeatInfra(t *testing.T) {
	old := jsonRow(t, strings.Replace(refusalRowJSON, `"refusal":{"status":503,"retry_after_sec":133},`, "", 1))
	if !old.Bad() {
		t.Fatalf("an unmarked row must stay bad: it may be the end of the chain")
	}
	if v := Classify(old); v.Axis != Axis("capacity") {
		t.Errorf("an unmarked 503 queue full row classified %q (evidence %q), want capacity", v.Axis, v.Evidence)
	}
	for name, errText := range map[string]string{
		"shed, the older node wording": "dispatch: node http://n1:18811 answered HTTP 503: shed: no idle execution slot",
		"429":                          "dispatch http://n1:18811/fleet/dispatch: status 429: queue full (2 jobs accepted+running, limit 2)",
	} {
		r := row(name, func(r *Row) { r.Error = errText; r.Result = nil })
		if v := Classify(r); v.Axis != Axis("capacity") {
			t.Errorf("%s: classified %q, want capacity", name, v.Axis)
		}
	}
	// What is not a capacity answer stays the serving stack's.
	for name, errText := range map[string]string{
		"dial refused":                 "dispatch http://n1:18811/fleet/dispatch: dial tcp 192.0.2.10:18811: connect: connection refused",
		"a 503 that is not about room": "dispatch http://n1:18811/fleet/dispatch: status 503: route is not configured on this node",
	} {
		r := row(name, func(r *Row) { r.Error = errText; r.Result = nil })
		if v := Classify(r); v.Axis != AxisSeatInfra {
			t.Errorf("%s: classified %q, want seat-infra", name, v.Axis)
		}
	}
}
