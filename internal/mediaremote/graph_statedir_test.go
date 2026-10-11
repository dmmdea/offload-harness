package mediaremote

// A graph is a ticket on a machine whose config sets NO state_dir (ADR 0083). The queue lives under the state root the GPU lease
// resolves (state_dir, else LOCAL_OFFLOAD_STATE_DIR, else the platform default), so no config key is needed and the usual config,
// which sets none, has the queue. The door asks storeFor for its store and nothing else: a door that read cfg.StateDir itself would
// send every graph of that config back to the pipeline's own wait, and no test that sets a state_dir would notice.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// FOR THE INTEGRATION MERGE: this test FAILS on the branch it was written on, and it is meant to. That branch (S5) was cut before F65,
// which made storeFor resolve the lease's state root; there storeFor still answers "no queue" for a config with no state_dir, so a busy
// graph runs in the local line and is no ticket. The merge brings F65 (ticket.go, storeFor), and then this passes with door.go's
// admit calling storeFor, as it does. Do not skip it, and do not give the config a state_dir to make it pass.
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
