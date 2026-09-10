package delegate

// A composite node advertises its layers in health (Task 9's rows); the
// delegator decodes them onto NodeView.Layers and runs the SAME placement
// table over them (council R5) — no second fit heuristic across layers.

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	placetable "github.com/dmmdea/offload-harness/internal/placement"
)

// fixtureRows is what a composite node publishes for the reference box with
// the pair's agent seat loaded and idle and every guard admitting: the rows
// Task 5's RowsFromConfig renders, carried here through the health wire.
func fixtureRows(t *testing.T, pairAgent placetable.SeatState) []placetable.LayerRow {
	t.Helper()
	live := placetable.Live{
		Seat: func(layer, role string) placetable.SeatState {
			if layer == placetable.LayerPair && role == placetable.RoleAgent {
				return pairAgent
			}
			return placetable.SeatState{}
		},
		DeviceFree:  func(string) (float64, bool) { return 16, true },
		DeviceIndex: func(d string) (string, bool) { return d, true },
		HostFree:    func() (float64, bool) { return 80, true },
		Presence:    func() placetable.Presence { return placetable.Presence{Mode: "away", Known: true, Away: true} },
	}
	rows := placetable.RowsFromConfig(config.CompositeFixture(), live)
	if len(rows) != 4 {
		t.Fatalf("fixture rows = %d, want the four layers", len(rows))
	}
	return rows
}

// pairOnlyRows keeps just the pair layer with its AGENT seat: a node whose
// largest window is 163,840.
func pairOnlyRows(t *testing.T) []placetable.LayerRow {
	t.Helper()
	for _, r := range fixtureRows(t, idlePair()) {
		if r.Name == placetable.LayerPair {
			var seats []placetable.SeatRow
			for _, s := range r.Seats {
				if s.Role == placetable.RoleAgent {
					seats = append(seats, s)
				}
			}
			r.Seats = seats
			return []placetable.LayerRow{r}
		}
	}
	t.Fatal("no pair row in the fixture")
	return nil
}

// healthWithLayers is agentHealthJSON's node with the rows appended.
func healthWithLayers(t *testing.T, rows []placetable.LayerRow) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(agentHealthJSON), &m); err != nil {
		t.Fatal(err)
	}
	m["layers"] = rows
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestFetchNodeViewDecodesLayerRowsAndOlderNodesGetNone(t *testing.T) {
	rows := fixtureRows(t, busyPair(9))
	srv := healthServer(t, healthWithLayers(t, rows), nil)
	got, err := FetchNodeView(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatalf("FetchNodeView: %v", err)
	}
	// The rows must survive the wire exactly as the node rendered them (a JSON
	// round trip of the fixture is the reference, so omitempty zeroes compare
	// equal on both sides).
	b, _ := json.Marshal(rows)
	var want []placetable.LayerRow
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Layers, want) {
		t.Fatalf("Layers:\n got %+v\nwant %+v", got.Layers, want)
	}
	if got.AgentSeat != "offload-e4b" || !got.AgentEnabled || got.AgentCtxTokens != 8192 {
		t.Fatalf("the agent fields still decode beside the rows: %+v", got)
	}
	layers, live := placetable.FromRows(got.Layers)
	if !reflect.DeepEqual(layers, config.CompositeFixture().Layers) {
		t.Fatalf("the spec rebuilt from the fetched rows differs from the fixture:\n got %+v", layers)
	}
	if st := live.Seat(placetable.LayerPair, placetable.RoleAgent); !st.Known || st.Inflight != 9 {
		t.Fatalf("occupancy from the fetched rows: %+v", st)
	}
	for _, body := range []string{agentHealthJSON, mediaHealthJSON} {
		v, err := FetchNodeView(context.Background(), healthServer(t, body, nil).URL, "")
		if err != nil {
			t.Fatalf("FetchNodeView: %v", err)
		}
		if v.Layers != nil {
			t.Fatalf("an older node decodes to no layers, got %+v", v.Layers)
		}
	}
}

// TestRemoteEligibilityRunsTheSameTableOverTheNodesRows: a 200k-token contract
// fits only the pair's LONG seat. A node advertising [pair/agent 163k,
// pair/long 262k] is eligible and the dispatched copy carries layer=pair; a
// node advertising the pair's agent seat alone is ineligible with the reason
// naming its largest window — the table's verdict, not a second heuristic.
func TestRemoteEligibilityRunsTheSameTableOverTheNodesRows(t *testing.T) {
	st := Subtask{Contract: contractOfTokens(200_000)}
	st.EstTokens = EstimateTokens(st.Contract)
	if st.EstTokens < 200_000 {
		t.Fatalf("estimate = %d, want ~200k", st.EstTokens)
	}
	both := eligibleRemote()
	both.Layers = fixtureRows(t, idlePair())
	if !remoteEligible(st, both) {
		t.Fatal("a node whose pair/long row holds the contract must be eligible")
	}
	dec, ok := remoteDecision(st, both)
	if !ok || dec.Layer != placetable.LayerPair || dec.Role != placetable.RoleLong || dec.Seat != "qwen3.8-27b-262k" {
		t.Fatalf("decision over the rows = %+v", dec)
	}
	// The agent-only node's ceiling (8192) would have refused it under the old
	// arithmetic too; the point is the REASON now comes from the table.
	only := eligibleRemote()
	only.Layers = pairOnlyRows(t)
	if remoteEligible(st, only) {
		t.Fatal("a node whose largest window is 163,840 cannot hold a 200k contract")
	}
	if dec, _ := remoteDecision(st, only); !dec.Defer || !strings.Contains(dec.Reason, "largest 163840") {
		t.Fatalf("decision = %+v, want a contract defer naming the largest window", dec)
	}
	// A node without rows keeps today's gate exactly: 8192 - reserve < 200k → ineligible.
	if remoteEligible(st, eligibleRemote()) {
		t.Fatal("the implicit single layer keeps adequate()")
	}
	small := schemaSubtask()
	if !remoteEligible(small, eligibleRemote()) || !remoteEligible(small, both) {
		t.Fatal("a small contract is eligible on both the implicit layer and the rows")
	}
	// The fit score ranks the DECIDED seat's window for a node with rows.
	if s := scoreFit(st, both); s != -262144 {
		t.Fatalf("scoreFit over rows = %d, want -(the long seat's window): the goal reads mechanical, so the smallest ADEQUATE seat wins", s)
	}
}

// TestRemoteRunCarriesTheDecidedLayerAndPublishesPlaced drives the wire: the
// dispatched contract names the layer the delegator decided, and the result
// carries the node's own placed block once it answers.
func TestRemoteRunCarriesTheDecidedLayerAndPublishesPlaced(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	cfg := compositeTestCfg(t)
	var seenLayer atomic.Value
	nodePlaced := &core.Placed{Tier: "blackwell-2x16", Layer: "pair", Role: "long", Seat: "qwen3.8-27b-262k", Reason: "node re-decided"}
	node, url := acceptingNode(t, "qube-remote", "qube found the needle", func(f *fakeNode) {
		f.layers = fixtureRows(t, idlePair())
		f.decodeCap = cfg.AgentContextCapBytes()
		f.onDispatch = func(_ string, c core.AgentContract) { seenLayer.Store(c.Layer) }
		f.pollByJob = func(jobID string, n int64) (map[string]any, int) {
			w := remoteWire("qube found the needle", `{"answer":"qube"}`)
			w.NodeID = "qube-remote"
			w.Seat = "qwen3.8-27b-262k"
			w.Placed = nodePlaced
			return doneWire(t, w), http.StatusOK
		}
	})
	results, sum, err := RunWith(context.Background(), cfg, neverLocal(t),
		[]core.AgentContract{contractOfTokens(200_000)}, "remote", []string{url}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Succeeded != 1 || node.dispatches.Load() != 1 {
		t.Fatalf("summary=%+v dispatches=%d", sum, node.dispatches.Load())
	}
	if got, _ := seenLayer.Load().(string); got != "pair" {
		t.Fatalf("dispatched contract layer = %q, want pair", got)
	}
	pr := results[0]
	if pr.Placed == nil || pr.Placed.Reason != "node re-decided" || pr.Seat != "qwen3.8-27b-262k" {
		t.Fatalf("result placed = %+v seat = %q, want the node's own block and seat", pr.Placed, pr.Seat)
	}
	if rw := wireOf(t, results, sum); rw["placed"].(map[string]any)["layer"] != "pair" {
		t.Fatalf("wire placed = %v", rw["placed"])
	}

	// The pair-only node: route=remote defers with the table's reason.
	only, onlyURL := acceptingNode(t, "qube-small", "never", func(f *fakeNode) { f.layers = pairOnlyRows(t) })
	results, sum, err = RunWith(context.Background(), cfg, neverLocal(t),
		[]core.AgentContract{contractOfTokens(200_000)}, "remote", []string{onlyURL}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Deferred != 1 || only.dispatches.Load() != 0 {
		t.Fatalf("summary=%+v dispatches=%d, want an undispatched defer", sum, only.dispatches.Load())
	}
	if r := results[0].Result.Reason; !strings.Contains(r, "qube-small") || !strings.Contains(r, "largest 163840") {
		t.Fatalf("reason = %q, want the node and its largest window named", r)
	}
	if results[0].Result.DeferClass != core.DeferClassContract {
		t.Fatalf("defer_class = %q, want contract (the fleet is fine; the contract does not fit)", results[0].Result.DeferClass)
	}
}
