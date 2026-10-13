package delegate

// D-32: a typed "where it ran" on every result.
//
// results[].placement is the placement REASON, a string, and the opencode plugin counted "local" in it. A remote
// result whose reason says "local seat busy" read as local (10 of the 214 remote rows in one delegation log did), so
// the digest told the model the pair had not landed. results[].ran_on is the closed answer: "local" when the
// delegator's own seat ran the subtask, "node" when a fleet node did, absent when no seat ran it. It is stamped in one
// place, WireResponse, from placedWhere: the decision that already writes the hint clause and the pin tallies, so the
// field cannot disagree with them.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	placetable "github.com/dmmdea/offload-harness/internal/placement"
)

// ranOnOf publishes one result through WireResponse and returns what the wire says: the ran_on value and whether the
// key is on the row at all (the assertions read the JSON, not the struct).
func ranOnOf(t *testing.T, pr PlacedResult) (value string, present bool) {
	t.Helper()
	raw, err := json.Marshal(WireResponse([]PlacedResult{pr}, Summary{}, nil))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Results []map[string]json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal(raw, &got); err != nil || len(got.Results) != 1 {
		t.Fatalf("response %s: %v", raw, err)
	}
	rawVal, present := got.Results[0]["ran_on"]
	if present {
		if err := json.Unmarshal(rawVal, &value); err != nil {
			t.Fatalf("ran_on is %s, not a string", rawVal)
		}
	}
	return value, present
}

func TestWireResponseSetsRanOnLocalForALocalRun(t *testing.T) {
	pr := PlacedResult{Node: "this-box", Seat: "seat-a", PlacementReason: "route=auto → local (headroom)", ranLocal: true,
		Result: core.AgentWireResult{Output: "ok"}}
	if v, present := ranOnOf(t, pr); !present || v != "local" {
		t.Fatalf("ran_on = %q (present %v), want \"local\" for a run on the delegator's own seat", v, present)
	}
}

func TestWireResponseSetsRanOnNodeForARemoteRun(t *testing.T) {
	pr := PlacedResult{Node: "node-a", Seat: "seat-b", PlacementReason: "route=spread → node-a (slot 1 of 2)", ranBase: "http://node-a:8080",
		Result: core.AgentWireResult{Output: "ok"}}
	if v, present := ranOnOf(t, pr); !present || v != "node" {
		t.Fatalf("ran_on = %q (present %v), want \"node\" for a run on a fleet node", v, present)
	}
}

func TestWireResponseOmitsRanOnWhenNothingRan(t *testing.T) {
	pr := PlacedResult{PlacementReason: "route=auto, and no node took it", Unplaced: true,
		Result: core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassCapacity, Reason: "no capacity"}}
	if v, present := ranOnOf(t, pr); present {
		t.Fatalf("ran_on = %q on a result no seat ran: the key must be absent", v)
	}
}

// The measured misread: the reason names a node and also says "local seat busy", and the old text reading counted it
// local. The hint clause variant is the one the clause reading exists for: it names the route the caller hinted.
func TestRanOnIgnoresTheWordLocalInAnOverriddenHint(t *testing.T) {
	for _, reason := range []string{
		"route=local was a hint (no pin_reason), overridden: placed on node-a; route=auto → node-a (headroom); the idle local seat's run-cap line is spent",
		"route=spread → node-a (slot 1 of 1); local seat busy: 1 in flight",
	} {
		pr := PlacedResult{Node: "node-a", PlacementReason: reason, ranBase: "http://node-a:8080", Result: core.AgentWireResult{Output: "ok"}}
		if v, _ := ranOnOf(t, pr); v != "node" {
			t.Errorf("ran_on = %q for %q, want \"node\": the word local in a reason says nothing about where it ran", v, reason)
		}
	}
	// And the other way: a remote hint that fell back to the seat, whose reason names no seat at all.
	pr := PlacedResult{Node: "this-box", PlacementReason: "route=remote was a hint (no pin_reason), overridden: placed on the local seat; no eligible remote", ranLocal: true}
	if v, _ := ranOnOf(t, pr); v != "local" {
		t.Errorf("ran_on = %q for a remote hint that ran on the seat, want \"local\"", v)
	}
}

// ran_on is one assignment from placedWhere, so it can be checked against it over every shape placedWhere decides on.
func TestRanOnFollowsPlacedWhere(t *testing.T) {
	cases := []struct {
		name string
		pr   PlacedResult
		want string
	}{
		{"nothing set", PlacedResult{}, ""},
		{"local", PlacedResult{ranLocal: true}, "local"},
		{"remote", PlacedResult{ranBase: "http://node-a:8080"}, "node"},
		{"unplaced beats a local flag", PlacedResult{ranLocal: true, Unplaced: true}, ""},
		{"unplaced beats a remote base", PlacedResult{ranBase: "http://node-a:8080", Unplaced: true}, ""},
		{"a refusal keeps the base it dialled but ran nowhere", PlacedResult{ranBase: "http://node-a:8080", refused: true}, ""},
		{"a local run that deferred on the seat was placed there", PlacedResult{ranLocal: true,
			Result: core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassCapacity}}, "local"},
	}
	for _, c := range cases {
		where := placedWhere(c.pr)
		wantFromWhere := map[int]string{placedNowhere: "", placedLocal: "local", placedRemote: "node"}[where]
		if wantFromWhere != c.want {
			t.Fatalf("%s: placedWhere says %q, the table says %q: fix the table, not the test", c.name, wantFromWhere, c.want)
		}
		v, present := ranOnOf(t, c.pr)
		if v != c.want || present != (c.want != "") {
			t.Errorf("%s: ran_on = %q (present %v), want %q", c.name, v, present, c.want)
		}
	}
}

// Every outcome nothing ran, built by the producers that make them, publishes no ran_on. These are the constructors the
// plan listed: a decided defer, a refusal chain that ended in no node, and the local seat's own capacity defer after
// nothing else had room.
func TestRanOnIsAbsentForEveryUnplacedOutcome(t *testing.T) {
	r := &runner{}
	dec := placetable.Decision{Defer: true, DeferClass: core.DeferClassCapacity}
	dec.Reason = "no seat"
	decided := r.decidedDefer(NodeView{NodeID: "this-box"}, dec, "route=auto")
	chain := exhausted(PlacedResult{ranBase: "http://node-a:8080", refused: true, refusalStatus: 503, Node: "node-a"}, []string{"node-a: 503"}, "no further eligible remote")
	capacityLocal := exhausted(PlacedResult{ranLocal: true, Result: core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassCapacity}}, []string{"this-box: capacity"}, "no further eligible remote")
	leaseWait := PlacedResult{PlacementReason: "waiting on a lease", Result: core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassCapacity}}
	for name, pr := range map[string]PlacedResult{"decided defer": decided, "refusal chain": chain, "local capacity defer after a chain": capacityLocal, "lease-wait defer": leaseWait} {
		if v, present := ranOnOf(t, pr); present {
			t.Errorf("%s: ran_on = %q, want the key absent", name, v)
		}
	}
}

// The wire fixture: a local run, a node run and a result nothing ran, as published. It pins the whole row, so a field
// that moved or appeared beside ran_on shows up here too ("nothing else in the payload changes").
func TestWireResponseMatchesTheRanOnFixture(t *testing.T) {
	results := []PlacedResult{
		{Node: "this-box", Seat: "seat-a", PlacementReason: "route=auto → local (headroom)", JobID: "agd-0001", ranLocal: true, wallMs: 1200,
			Result: core.AgentWireResult{Output: "local answer"}},
		{Node: "node-a", Seat: "seat-b", PlacementReason: "route=spread → node-a (slot 1 of 1); local seat busy: 1 in flight", JobID: "agd-0002", ranBase: "http://node-a:8080", wallMs: 3400,
			Result: core.AgentWireResult{Output: "node answer"}},
		{PlacementReason: "route=auto, and no node took it", JobID: "agd-0003", Unplaced: true,
			Result: core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassCapacity, Reason: "no capacity"}},
	}
	raw, err := json.Marshal(WireResponse(results, Summary{Succeeded: 2, Deferred: 1}, nil))
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "ran_on_wire.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got, w := string(raw), strings.TrimSpace(strings.ReplaceAll(string(want), "\r\n", "\n")); got != w {
		t.Fatalf("published response differs from testdata/ran_on_wire.json:\n got %s\nwant %s", got, w)
	}
}
