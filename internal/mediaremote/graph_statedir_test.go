package mediaremote

// A graph is a ticket on a machine whose config sets NO state_dir (ADR 0083). The queue lives under the state root the GPU lease
// resolves (state_dir, else LOCAL_OFFLOAD_STATE_DIR, else the platform default), so no config key is needed and the usual config,
// which sets none, has the queue. The door asks storeFor for its store and nothing else: a door that read cfg.StateDir itself would
// send every graph of that config back to the pipeline's own wait, and no test that sets a state_dir would notice.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// This test failed on the branch it was written on (S5), by design: that branch was cut before F65, which made storeFor resolve the
// lease's state root, so there storeFor still answered "no queue" for a config with no state_dir and a busy graph ran in the local
// line and was no ticket. The merge of that branch onto 0.180.0 brings F65 (ticket.go, storeFor), and it passes with door.go's admit
// calling storeFor, as it does. Do not skip it, and do not give the config a state_dir to make it pass.
func TestABusyGraphIsATicketOnAMachineWithNoStateDirKey(t *testing.T) {
	e := newGEnv(t)
	e.node("node-a", true, nodeOpts{}, krea2File)
	e.start()
	e.cfg.StateDir = "" // the operator's real config sets none
	root := t.TempDir()
	t.Setenv("LOCAL_OFFLOAD_STATE_DIR", root) // the root such a machine resolves, named by the environment so the test touches no machine-wide one
	t.Setenv("GPU_LOCK", "")

	req := e.req(krea2Graph, nil)
	res := runDoor(e.cfg, e.lr, req, AtOnce)
	if res.OK || !res.Deferred || res.Meta.ErrClass != core.ErrClassGPUQueued {
		t.Fatalf("a busy graph on a machine with no state_dir key is a ticket, not a call that runs in the local line: %+v", res)
	}
	a := graphAnswer(t, res)

	dir := filepath.Join(root, queueDirName)
	for _, name := range []string{a.Ticket + ".json", "body-" + a.Ticket + "-graph.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("the ticket's %s is a file under the state root the lease resolves: %v", name, err)
		}
	}
	if rows := Queue(e.cfg); len(rows) != 1 || rows[0].Ticket != a.Ticket || rows[0].Task != string(core.TaskRunGraph) {
		t.Errorf("offload_status lists the graph ticket of a machine with no state_dir: %+v", rows)
	}
	// An identical request from a file of its own attaches to it, as on a machine that sets state_dir.
	if b := graphAnswer(t, runDoor(e.cfg, e.lr, resend(e, req, krea2Graph, nil), AtOnce)); b.Ticket != a.Ticket {
		t.Errorf("a re-send attaches to ticket %s, got %s", a.Ticket, b.Ticket)
	}
}

// A machine whose lease refuses the state root (a cloud-synced folder) keeps no queue, so a busy graph there is no ticket and is not
// offered to the fleet: it waits in the machine's own line, as an image does (TestARefusedStateRootKeepsNoQueueAndSaysSo). In 0.180.0
// the one-shot overflow offered it with only the in-process hold between two calls; this release does not (ADR 0083 Amendment 1).
func TestABusyGraphOnAMachineWithNoQueueIsNotOfferedToTheFleet(t *testing.T) {
	n := startGraphNode(t, "node-a", []string{krea2File}, 7.96) // idle, and it holds the file
	cfg := graphClient(t, n)
	cfg.StateDir = filepath.Join(t.TempDir(), "Google Drive", "state")
	if _, ok := storeFor(cfg); ok {
		t.Fatal("control: a cloud-synced root keeps no queue")
	}
	lr := &laneRunner{verdict: heldLane(), local: queuedLocal()}
	res := Run(context.Background(), cfg, lr, graphReqOf(t, krea2Graph, nil), "auto", nil)
	untouched(t, "the node: with no queue nothing is offered to the fleet", n)
	if _, runs := lr.counts(); runs != 1 {
		t.Errorf("the call goes to this machine's own line, once: %d runs (%+v)", runs, res)
	}
	if strings.Contains(string(res.Data), `"ticket"`) {
		t.Errorf("no ticket can exist without a queue: %s", res.Data)
	}
}
