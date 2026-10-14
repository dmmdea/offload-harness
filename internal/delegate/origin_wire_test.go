package delegate

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/fleetqueue"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// Register D-101 (ADR 0087): a delegation names the session and run class of the box that asked on the request that
// creates its work on a node, through the one chokepoint every work-creating POST already passes
// (pairworkloads.Emitter.SetWireHeaders), and a call pinned for measurement is a measurement on the node's row
// too, whatever the process is.

func originEnv(t *testing.T, session, class string) {
	t.Helper()
	t.Setenv("LOCAL_OFFLOAD_ORIGIN", session)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv(ledger.EnvRunClass, class)
	t.Setenv("OFFLOAD_PAIR_APPDIR", t.TempDir()) // no PAIR on this box
}

func dispatchOnce(t *testing.T, opts *RunOptions) *fakeNode {
	t.Helper()
	compressPolls(t, 5*time.Millisecond, time.Second)
	node, url := acceptingNode(t, "node-a", "zorblax", nil)
	if _, sum, err := RunWith(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{remoteContract()}, "remote", []string{url}, opts); err != nil || sum.Succeeded != 1 {
		t.Fatalf("Run: err=%v summary=%+v", err, sum)
	}
	return node
}

func seen(v interface{ Load() any }) string {
	s, _ := v.Load().(string)
	return s
}

func TestDispatchCarriesTheProcessOriginAndRunClass(t *testing.T) {
	originEnv(t, "sess-abc", "bake")
	node := dispatchOnce(t, nil)
	if got := seen(&node.lastOrigin); got != "sess-abc" {
		t.Fatalf("%s = %q, want sess-abc", core.OriginHeader, got)
	}
	if got := seen(&node.lastRunClass); got != "bake" {
		t.Fatalf("%s = %q, want bake", core.RunClassHeader, got)
	}

	// A production process with no class sends the session and no class header at all.
	originEnv(t, "sess-abc", "")
	node = dispatchOnce(t, nil)
	if got := seen(&node.lastOrigin); got != "sess-abc" {
		t.Fatalf("production: %s = %q, want sess-abc", core.OriginHeader, got)
	}
	if got := seen(&node.lastRunClass); got != "" {
		t.Fatalf("production: %s = %q, want none (an unlabelled row is production)", core.RunClassHeader, got)
	}

	// A bare environment sends neither.
	originEnv(t, "", "")
	node = dispatchOnce(t, nil)
	if o, c := seen(&node.lastOrigin), seen(&node.lastRunClass); o != "" || c != "" {
		t.Fatalf("bare environment: origin %q class %q, want neither", o, c)
	}
}

// A call pinned for measurement is not a process class but is a measurement all the same: it opens no card anywhere
// (register F48) and now labels the node's row. The process class, when there is one, wins; an unpinned call and a
// pin made for another reason send no class.
func TestMeasurementPinnedDispatchSendsTheMeasurementClass(t *testing.T) {
	originEnv(t, "sess-abc", "")
	node := dispatchOnce(t, &RunOptions{PinReason: PinMeasurement, PinNeedsReason: true})
	if got := seen(&node.lastRunClass); got != ledger.ClassMeasurement {
		t.Fatalf("pinned for measurement: %s = %q, want %q", core.RunClassHeader, got, ledger.ClassMeasurement)
	}
	if got := seen(&node.lastPairCard); got != "" {
		t.Fatalf("pinned for measurement: %s = %q, the card signal must still be withheld", core.PairCardHeader, got)
	}

	node = dispatchOnce(t, &RunOptions{PinReason: PinOperator, PinNeedsReason: true})
	if got := seen(&node.lastRunClass); got != "" {
		t.Fatalf("a pin made for another reason: %s = %q, want none", core.RunClassHeader, got)
	}
	node = dispatchOnce(t, nil)
	if got := seen(&node.lastRunClass); got != "" {
		t.Fatalf("an unpinned call: %s = %q, want none", core.RunClassHeader, got)
	}

	originEnv(t, "sess-abc", "bake")
	node = dispatchOnce(t, &RunOptions{PinReason: PinMeasurement, PinNeedsReason: true})
	if got := seen(&node.lastRunClass); got != "bake" {
		t.Fatalf("a process class and a measurement pin: %s = %q, want the process class bake", core.RunClassHeader, got)
	}
}

// The queue submit carries the same two headers to the holder, which stores them on the job so whichever node claims
// it stamps the same session as if the job had been pushed.
func TestQueueSubmitCarriesTheOriginToTheClaimedJob(t *testing.T) {
	q, err := fleetqueue.Open(t.TempDir() + "/q.db")
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	mux := http.NewServeMux()
	fleetqueue.Mount(mux, q, func(*http.Request) bool { return true })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	originEnv(t, "sess-abc", "bake")
	done := make(chan fleetqueue.Job, 1)
	go func() {
		for i := 0; i < 200; i++ {
			job, ok, _ := q.Claim("sim-node", []string{"agent"})
			if ok {
				wire, _ := json.Marshal(core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "sim-node", Seat: "s",
					Output: "RF-9082", Structured: json.RawMessage(`{"shipment_id":"RF-9082"}`)})
				_ = q.Ack(job.ID, "sim-node", wire, "")
				done <- *job
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		done <- fleetqueue.Job{}
	}()
	cfg := testCfg(t)
	cfg.FleetQueueHolder = srv.URL
	contract := core.AgentContract{Goal: "which shipment?", OutputSchema: json.RawMessage(`{"properties":{"shipment_id":{"type":"string"}}}`), TimeoutSec: 30}
	_, _, _ = Run(t.Context(), cfg, nil, []core.AgentContract{contract}, "queue", nil)
	job := <-done
	if job.ID == "" {
		t.Fatal("no job was queued")
	}
	if job.Origin != "sess-abc" || job.RunClass != "bake" {
		t.Fatalf("queued job = origin %q class %q, want sess-abc and bake", job.Origin, job.RunClass)
	}
}

// The floor the session-share reader documents for the queue lane, pinned from the writer's side: a route "queue"
// delegation builds no runner and writes no row on the ASKER's ledger (the claiming node's own ledger holds it, as a
// served row), so on the asker's box it is in neither the remote nor the local number. The control is the push path
// on the same config, whose finished row carries the fleet job id the reader counts as remote; without it an empty
// ledger could mean a ledger that was never writable. If the queue lane ever writes an asker row, this fails and the
// reader's documented floor (ledger.SessionShare, ADR 0087) must change with it.
func TestQueueRouteWritesNoRowOnTheAskersLedger(t *testing.T) {
	q, err := fleetqueue.Open(t.TempDir() + "/q.db")
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	mux := http.NewServeMux()
	fleetqueue.Mount(mux, q, func(*http.Request) bool { return true })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	claimed := make(chan string, 1)
	go func() {
		for i := 0; i < 200; i++ {
			job, ok, _ := q.Claim("sim-node", []string{"agent"})
			if ok {
				wire, _ := json.Marshal(core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "sim-node", Seat: "s",
					Output: "RF-9082", Structured: json.RawMessage(`{"shipment_id":"RF-9082"}`)})
				_ = q.Ack(job.ID, "sim-node", wire, "")
				claimed <- job.ID
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		claimed <- ""
	}()
	cfg := testCfg(t)
	cfg.FleetQueueHolder = srv.URL
	contract := core.AgentContract{Goal: "which shipment?", OutputSchema: json.RawMessage(`{"properties":{"shipment_id":{"type":"string"}}}`), TimeoutSec: 30}
	_, sum, err := Run(t.Context(), cfg, nil, []core.AgentContract{contract}, "queue", nil)
	if err != nil || sum.Succeeded != 1 {
		t.Fatalf("queue run: err=%v summary=%+v", err, sum)
	}
	if id := <-claimed; id == "" {
		t.Fatal("no node claimed the job")
	}
	rows, err := ledger.ReadAll(cfg.LedgerPath)
	if err != nil || len(rows) != 0 {
		t.Fatalf("after a queue-routed delegation the asker's ledger holds %d rows (%v): %+v; want none", len(rows), err, rows)
	}

	// The control: the push path on the same config writes the finished row, with the fleet job id.
	compressPolls(t, 5*time.Millisecond, time.Second)
	_, url := acceptingNode(t, "node-a", "zorblax", nil)
	if _, sum, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{remoteContract()}, "remote", []string{url}, nil); err != nil || sum.Succeeded != 1 {
		t.Fatalf("push run: err=%v summary=%+v", err, sum)
	}
	all, err := ledger.ReadAll(cfg.LedgerPath)
	pushed := ledger.JobRows(all)
	if err != nil || len(pushed) != 1 || pushed[0].FleetJobID == "" {
		t.Fatalf("after a push-routed delegation the ledger holds %+v (%v); want one job row carrying a fleet job id", pushed, err)
	}
}
