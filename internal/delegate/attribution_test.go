package delegate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/fleetqueue"
)

// pairAppDirNamed writes a PAIR app dir whose only member is this box, named name, and points the
// emitter at it for the test.
func pairAppDirNamed(t *testing.T, name string) {
	t.Helper()
	app := t.TempDir()
	for file, body := range map[string]string{
		"node-id.json":                           `{"node_uuid":"self-uuid"}`,
		filepath.Join("cluster", "members.json"): `[{"nodeUuid":"self-uuid","name":"` + name + `"}]`,
	} {
		p := filepath.Join(app, file)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("OFFLOAD_PAIR_APPDIR", app)
}

// D7/D11: a delegation dispatch names its asker, and tells the node to card the job ONLY when this box
// will not: an asker whose emitter is enabled cards it itself, and a second card is the bug.
func TestDispatchCarriesTheAttributionHeaders(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	node, url := acceptingNode(t, "node-a", "zorblax", nil)

	t.Setenv("OFFLOAD_PAIR_APPDIR", t.TempDir()) // no PAIR on this box
	cfg := testCfg(t)
	if _, sum, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{remoteContract()}, "remote", []string{url}, nil); err != nil || sum.Succeeded != 1 {
		t.Fatalf("Run: err=%v summary=%+v", err, sum)
	}
	if got, _ := node.lastPairCard.Load().(string); got != core.PairCardNode {
		t.Fatalf("emitter disabled: %s = %q, want %q", core.PairCardHeader, got, core.PairCardNode)
	}
	if got, _ := node.lastAsker.Load().(string); got == "" {
		t.Fatalf("emitter disabled: %s absent, every asker names itself", core.AskerHeader)
	}

	pairAppDirNamed(t, "Node-Q")
	cfg = testCfg(t)
	cfg.PairWorkloadsEnabled = true
	cfg.PairWorkloadsEndpoint = "http://127.0.0.1:1/v1/workloads/events"
	if _, sum, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{remoteContract()}, "remote", []string{url}, nil); err != nil || sum.Succeeded != 1 {
		t.Fatalf("Run: err=%v summary=%+v", err, sum)
	}
	if got, _ := node.lastPairCard.Load().(string); got != "" {
		t.Fatalf("emitter enabled: %s = %q, want it absent (this box cards the job)", core.PairCardHeader, got)
	}
	if got, _ := node.lastAsker.Load().(string); got != "node-q" {
		t.Fatalf("emitter enabled: %s = %q, want the PAIR member name node-q", core.AskerHeader, got)
	}
}

// D9/D11: the queue submit carries the same two headers to the holder, which stores them on the job
// so whichever node claims it behaves as if the job had been pushed.
func TestQueueSubmitCarriesTheAttributionToTheClaimedJob(t *testing.T) {
	q, err := fleetqueue.Open(t.TempDir() + "/q.db")
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	mux := http.NewServeMux()
	fleetqueue.Mount(mux, q, func(*http.Request) bool { return true })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	claim := func(want string) fleetqueue.Job {
		t.Helper()
		for i := 0; i < 200; i++ {
			job, ok, _ := q.Claim("sim-node", []string{"agent"})
			if ok {
				wire, _ := json.Marshal(core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "sim-node", Seat: "s",
					Output: "RF-9082", Structured: json.RawMessage(`{"shipment_id":"RF-9082"}`)})
				_ = q.Ack(job.ID, "sim-node", wire, "")
				return *job
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatalf("%s: no job was queued", want)
		return fleetqueue.Job{}
	}
	contract := core.AgentContract{Goal: "which shipment?", OutputSchema: json.RawMessage(`{"properties":{"shipment_id":{"type":"string"}}}`), TimeoutSec: 30}
	run := func(enabled bool) {
		cfg := testCfg(t)
		cfg.FleetQueueHolder = srv.URL
		cfg.PairWorkloadsEnabled = enabled
		cfg.PairWorkloadsEndpoint = "http://127.0.0.1:1/v1/workloads/events"
		_, _, _ = Run(context.Background(), cfg, nil, []core.AgentContract{contract}, "queue", nil)
	}

	t.Setenv("OFFLOAD_PAIR_APPDIR", t.TempDir())
	done := make(chan fleetqueue.Job, 1)
	go func() { done <- claim("disabled") }()
	run(false)
	if job := <-done; job.PairCard != core.PairCardNode || job.Asker == "" {
		t.Fatalf("emitter disabled: queued job carries asker %q pair_card %q, want a name and %q", job.Asker, job.PairCard, core.PairCardNode)
	}

	pairAppDirNamed(t, "Node-Q")
	go func() { done <- claim("enabled") }()
	run(true)
	if job := <-done; job.PairCard != "" || job.Asker != "node-q" {
		t.Fatalf("emitter enabled: queued job carries asker %q pair_card %q, want node-q and no card signal", job.Asker, job.PairCard)
	}
}
