package mediaremote

// The ledger row of an ADOPTED ticket names the node it ran on (0.180.0 acceptance, A6).
//
// The process that sends a job writes its row through a handle that was told the node at dispatch (Dispatched), so the row
// carries node, node_id and fleet_job_id. The process that ADOPTS a placed ticket of an owner that ended never dispatches (it
// must not open a second PAIR card), so its handle was never told the node, and its row named none: the only place the node
// appeared was the placement note's "; ran on <node>" tail, and the ledger caps a placement at 120 bytes while the lane sentence
// that precedes the tail (the card ids the lane was held on, the holder's reason) is longer than that. A row that says a call
// ran remotely and not where is not attribution.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/ledger"
)

func TestAnAdoptedTicketsRowNamesTheNodeItRanOn(t *testing.T) {
	rig := newPairRig(t, true, "node-a")
	e := newQEnv(t, 1, false)
	e.cfg.PairWorkloadsEnabled = true
	e.lr.rig = rig
	req := e.req("a red door", nil)
	n := e.nodes[0]

	local, _, _ := localIdentity(context.Background(), e.cfg, "qwen-image-2.1-fast")
	jobID := "media-deadbeef00000002"
	body, _ := json.Marshal(map[string]any{"job_id": jobID, "task_type": taskImage, "payload": map[string]any{
		"prompt": req.Input, "family": "qwen-image-2.1", "seed": 424242, "width": 2048, "height": 2048, "recipe_digest": local.recipe.Digest()}})
	if err := post(context.Background(), e.cfg, n.srv.URL, "/fleet/dispatch", body); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "the node finishes the job", func() bool {
		j, err := getJSON[jobWire](context.Background(), e.cfg, n.srv.URL+"/fleet/jobs/"+jobID)
		return err == nil && j.State == "done"
	})
	st, _ := storeFor(e.cfg)
	// The lane sentence a real hold produces: the cards, the lease's class and reason and its declared term. Its length is the
	// point: the ledger keeps 120 bytes of a placement, so the "; ran on <node>" that follows it does not reach the ledger.
	laneWhy := `card(s) card-two-of-the-rig held by a media-class lease ("a long reason an operator wrote for the hold"), about 2108s of its declared term left`
	dead := ticketFile{ID: "mq-deadbeef0002", ReqKey: reqKey(req), Created: nowMs() - 60_000, Owner: deadOwner, Beat: nowMs() - 5000,
		Task: "generate_image", Out: req.Params["out"].(string), Seed: 424242, State: stPlaced, Node: "node-a", Base: n.srv.URL, Job: jobID,
		LaneWhy: laneWhy}
	if err := st.write(dead); err != nil {
		t.Fatal(err)
	}

	res := runDoor(e.cfg, e.lr, req, callWindow)
	if !res.OK || res.Meta.Node != "node-a" {
		t.Fatalf("the next identical call adopts the job and brings its result: %+v", res)
	}
	if !strings.Contains(res.Meta.Placement, "ran on node-a") {
		t.Fatalf("the result's own placement names the node: %q", res.Meta.Placement)
	}
	if kept := ledger.ShortReason(res.Meta.Placement); strings.Contains(kept, "ran on") {
		t.Fatalf("this test must stand for the real case, a lane sentence long enough that the ledger's cap cuts the node off the placement: %q", kept)
	}
	rows := rig.rows()
	if len(rows) != 1 || rows[0].Deferred {
		t.Fatalf("the adopted call writes one ledger row: %+v", rows)
	}
	row := rows[0]
	if row.NodeID != "node-a" || row.Node == "" || row.FleetJobID != jobID {
		t.Errorf("the adopted call's row must name the node it ran on and the job it carried (node_id %q, node %q, fleet_job_id %q; want node-a, the node's host, %s): %+v",
			row.NodeID, row.Node, row.FleetJobID, jobID, row)
	}
	if !row.CardByCaller {
		t.Errorf("the row's card is decided by the writer (the ended process's card is swept, none is opened here): %+v", row)
	}
	if rig.frameCount() != 0 {
		t.Errorf("naming the node opens no card: %d frames", rig.frameCount())
	}
}
