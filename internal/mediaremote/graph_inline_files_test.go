package mediaremote

// The MCP door writes an inline graph_json and manifest_json to temp files of its own, hands their paths to RunWith, and removes
// them when RunWith returns (internal/mcpserver, rungraph_tempfiles_test.go). A call that becomes a ticket returns long before the
// ticket has run: the files are gone while the ticket still waits. That is safe only because a graph ticket keeps its own copy of the
// bytes it was admitted with (ticket.go writeBodies) and runs from those, not from the caller's paths. This holds that chain end to
// end: admit, remove the caller's files the way the door does, let a lane free, and see the lane run the bytes the call was made with.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

func TestAGraphTicketStillRunsOnThisMachinesLaneOnceTheDoorHasRemovedItsInlineFiles(t *testing.T) {
	e := newGEnv(t)
	e.node("node-a", true, nodeOpts{}, krea2File)
	e.start()
	br := &bodyRunner{keeperRunner: e.lr}
	q1, _ := manual(t)
	ctx := context.Background()
	mpath := writeFile(t, t.TempDir(), "m.json", []byte(bodyManifestA))
	req := e.req(krea2Graph, map[string]any{"manifest_path": mpath})
	gpath := req.Params["graph_path"].(string)
	ticket := admitOn(t, q1, e.cfg, br, req) // RunWith's work up to the point it returns "queued"

	// What the door does the moment its call returns.
	for _, path := range []string{gpath, mpath} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}

	q1.cycle(ctx) // the first scan stands on the reading the ticket was made with; the lane is still held
	e.lr.set(core.LaneVerdict{Free: true})
	q1.cycle(ctx)
	waitFor(t, 5*time.Second, "the ticket runs on this machine's lane", func() bool { return isDone(ticket) })
	graphs, manifests := br.ran()
	if len(graphs) != 1 || graphs[0] != krea2Graph || manifests[0] != bodyManifestA {
		t.Fatalf("the lane must run the bytes the call was admitted with (graph %q, manifest %q) though the door's files are gone, got graphs %q manifests %q",
			krea2Graph, bodyManifestA, graphs, manifests)
	}
}
