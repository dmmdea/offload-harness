package mediaremote

// Where the ticket queue lives when the config names no state_dir (ADR 0083). Every other fixture of this package sets
// state_dir, which is how the queue shipped switched OFF on a machine whose config sets none: storeFor answered "no queue"
// and every busy call fell back to the local line. The queue's root is the one the GPU lease resolves, so these tests build
// their configs WITHOUT a StateDir and name the root the way production does, through LOCAL_OFFLOAD_STATE_DIR or the
// platform default.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// defaultRoot points the machine's default state root at a directory of the test's and clears the lease overrides, so the
// resolution the test reads is the state_dir / environment / platform-default one.
func defaultRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("LOCAL_OFFLOAD_STATE_DIR", root)
	t.Setenv("GPU_LOCK", "")
	return root
}

// A config with no state_dir keeps the ticket queue, under the machine's default state root: a busy image call is a ticket
// whose file is there, offload_status lists it, and no key had to be set.
func TestTicketQueueNeedsNoStateDirKey(t *testing.T) {
	e := newQEnv(t, 1, true)
	e.cfg.StateDir = "" // the operator's real config sets none
	root := defaultRoot(t)

	req := e.req("a red door", map[string]any{"seed": 7})
	a := answerOf(t, runDoor(e.cfg, e.lr, req, AtOnce))

	file := filepath.Join(root, queueDirName, a.Ticket+".json")
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("the ticket %s is a file under the machine's state root: %v", a.Ticket, err)
	}
	if rows := Queue(e.cfg); len(rows) != 1 || rows[0].Ticket != a.Ticket {
		t.Errorf("offload_status lists the ticket of a machine with no state_dir: %+v", rows)
	}
	// An identical re-send attaches to it, as on a machine that sets state_dir.
	if b := answerOf(t, runDoor(e.cfg, e.lr, req, AtOnce)); b.Ticket != a.Ticket {
		t.Errorf("a re-send must attach to ticket %s, got %s", a.Ticket, b.Ticket)
	}
}

// A graph call on a machine with no state_dir takes and reads the same per-node claims as one that sets it: a node a ticket
// has a job on is in use for the graph, and the row names the ticket.
func TestAGraphReadsTheTicketClaimsOfTheDefaultStateRoot(t *testing.T) {
	inUse := startGraphNode(t, "node-a", []string{krea2File}, 7.96)
	cfg := graphClient(t, inUse)
	cfg.StateDir = ""
	root := defaultRoot(t)

	st, ok := storeFor(cfg)
	if !ok {
		t.Fatal("a machine with no state_dir has the default state root's queue")
	}
	tk := ticketFile{ID: "mq-cccccccccccc", ReqKey: "k", Created: nowMs(), Owner: selfOwner(), Beat: nowMs(), State: stWaiting}
	if got, _, err := st.claimNode(&tk, inUse.srv.URL, "node-a", stPlaced); err != nil || !got {
		t.Fatalf("the ticket claims the node: %v %v", got, err)
	}
	if files := claimFilesIn(t, filepath.Join(root, queueDirName)); len(files) != 1 {
		t.Fatalf("the claim is a file under the default state root: %v", files)
	}

	lr := &laneRunner{verdict: heldLane(), local: queuedLocal()}
	res := Run(context.Background(), cfg, lr, graphReqOf(t, krea2Graph, nil), "auto", nil)
	noPosts(t, "the node a ticket has a job on", inUse)
	row, found := rowFor(clusterOf(t, res), "node-a")
	if !found || row.State != "in-use" || !strings.Contains(row.Why, tk.ID) {
		t.Errorf("the row names the ticket that uses the node: %+v", row)
	}
}

func claimFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "use-") && strings.HasSuffix(e.Name(), ".json") {
			out = append(out, e.Name())
		}
	}
	return out
}

// The queue and the GPU lease are rooted in ONE place for the same config, whatever names it: an explicit state_dir, the
// environment override, or the platform default. Two copies of that default is how a ticket's place token and its file would
// end up on different roots.
func TestTicketStoreAndLeaseShareOneStateRoot(t *testing.T) {
	t.Setenv("GPU_LOCK", "")
	explicit := t.TempDir()
	byEnv := t.TempDir()
	cases := []struct {
		name     string
		stateDir string
		env      string
		open     bool // the lease can be opened (the root is a directory of the test's); the platform default is only resolved
	}{
		{"an explicit state_dir", explicit, byEnv, true},
		{"the environment override", "", byEnv, true},
		{"the platform default", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("LOCAL_OFFLOAD_STATE_DIR", c.env)
			cfg := config.Config{StateDir: c.stateDir}
			st, ok := storeFor(cfg)
			if !ok {
				t.Fatal("the machine has a queue")
			}
			leaseDir, err := gpulease.LeaseDir("", cfg.StateDir)
			if err != nil {
				t.Fatalf("lease dir: %v", err)
			}
			if got, want := filepath.Dir(st.dir), filepath.Dir(filepath.Dir(leaseDir)); got != want {
				t.Errorf("the queue's root %q is not the lease's %q", got, want)
			}
			if filepath.Base(st.dir) != queueDirName {
				t.Errorf("the queue directory is %q", st.dir)
			}
			if c.open {
				m, err := gpulease.OpenAt("", cfg.StateDir)
				if err != nil {
					t.Fatalf("open the lease: %v", err)
				}
				if filepath.Dir(st.dir) != m.Root() {
					t.Errorf("the queue's root %q is not the lease manager's %q", filepath.Dir(st.dir), m.Root())
				}
			}
		})
	}
}

// A state root the lease refuses (a cloud-synced folder) is refused for the queue too, and the refusal is SAID, once: a
// queue that is silently off was the defect.
func TestARefusedStateRootKeepsNoQueueAndSaysSo(t *testing.T) {
	t.Setenv("GPU_LOCK", "")
	var lines []string
	prev := queueLog
	queueLog = func(line string) { lines = append(lines, line) }
	t.Cleanup(func() { queueLog = prev })
	resetNoQueueWarned()

	cfg := config.Config{StateDir: filepath.Join(t.TempDir(), "Google Drive", "state")}
	for i := 0; i < 3; i++ {
		if st, ok := storeFor(cfg); ok || st != nil {
			t.Fatalf("a cloud-synced root keeps no queue: %+v", st)
		}
	}
	if rows := Queue(cfg); rows != nil {
		t.Errorf("no queue, no rows: %+v", rows)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "cloud-sync") {
		t.Errorf("the refusal is logged once, with its reason: %q", lines)
	}

	// The call still runs: it goes to the local line as it did before the queue existed.
	e := newQEnv(t, 1, true)
	e.cfg.StateDir = cfg.StateDir
	res := runDoor(e.cfg, e.lr, e.req("a red door", map[string]any{"seed": 7}), AtOnce)
	if res.Deferred && res.Meta.ErrClass == core.ErrClassGPUQueued && strings.Contains(string(res.Data), `"ticket"`) {
		t.Errorf("no ticket can exist without a queue: %s", res.Data)
	}
}

// resetNoQueueWarned forgets the reasons already logged, so a test sees the first report again.
func resetNoQueueWarned() {
	noQueueWarned.Range(func(k, _ any) bool { noQueueWarned.Delete(k); return true })
}
