package fleetnode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/fleetqueue"
	"github.com/dmmdea/offload-harness/internal/netguard"
)

// A node's ledger row for a dispatched run has to be joinable to the delegator's
// row for the same run (ADR 0064, register C-63). The id both sides know is the
// dispatch envelope's job_id — until now the node's row carried an id of its own
// (the name of a temp directory), so the join needed a guess on latency and got
// it wrong for one run in a hundred. The node stamps the envelope id on the
// request it builds; the pipeline carries it to the row (see the pipeline test).

// TestDispatchCarriesTheFleetJobIDToTheRunner: every dispatched job's request
// reaches the runner naming the job id it was dispatched under.
func TestDispatchCarriesTheFleetJobIDToTheRunner(t *testing.T) {
	runner := &fakeRunner{}
	cfg := agentLaneCfg(t, withdrawToken)
	cfg.ImageGenScript = "C:/x/comfy-generate.mjs"
	s, _ := newTestServer(t, cfg, runner, authOpts(true))

	if rec := do(t, s, http.MethodPost, "/fleet/dispatch",
		`{"job_id":"media-42","task_type":"image-gen","payload":{"prompt":"hi"}}`, nil); rec.Code != http.StatusAccepted {
		t.Fatalf("media dispatch = %d (body %s)", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, http.MethodPost, "/fleet/dispatch", agentLaneBody("agd-77"), withdrawAuth); rec.Code != http.StatusAccepted {
		t.Fatalf("agent dispatch = %d (body %s)", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(runner.requests()) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the runner never saw both dispatched jobs")
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := map[string]string{}
	for _, req := range runner.requests() {
		got[string(req.Task)] = req.FleetJobID
	}
	if got["image-gen"] != "media-42" && got["generate_image"] != "media-42" {
		t.Errorf("media request fleet job id: %v, want media-42 on the image request", got)
	}
	if got["agent"] != "agd-77" {
		t.Errorf("agent request fleet job id = %q, want agd-77 (%v)", got["agent"], got)
	}
}

// TestPulledJobCarriesTheFleetJobIDToTheRunner: the pull door hand-writes the same
// sequence as the push door, and a fix landed on one alone is the drift class the
// register keeps recording — so the pulled job's request names its id too.
func TestPulledJobCarriesTheFleetJobIDToTheRunner(t *testing.T) {
	runner := &fakeRunner{}
	cfg := imageCfg()
	cfg.Home = t.TempDir()
	cfg.FleetAgentEnabled = true
	cfg.AgentModel = "agent-seat"
	cfg.FleetAuthToken = "tok"
	s, jobs := newTestServer(t, cfg, runner, &Options{
		NodeID:           "testnode",
		Snapshot:         goodSnapshot,
		Footprints:       func() []FootprintEntry { return nil },
		LoopbackListener: true,
	})
	served := false
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fleet/queue/claim" {
			if served {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			served = true
			_ = json.NewEncoder(w).Encode(fleetqueue.Job{
				ID: "pulled-9", TaskType: "agent",
				Payload: json.RawMessage(`{"schema_version":1,"goal":"g","output_schema":` + agentSchemaJSON + `}`),
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer holder.Close()
	client := &http.Client{Timeout: 5 * time.Second, Transport: netguard.SafeTransport(nil)}
	if id, ok := s.claimOne(context.Background(), client, holder.URL, "testnode", cfg); !ok || id != "pulled-9" {
		t.Fatalf("claimOne = %q, %v", id, ok)
	}
	waitJobState(t, jobs, "pulled-9", JobDone)
	reqs := runner.requests()
	if len(reqs) != 1 || reqs[0].FleetJobID != "pulled-9" {
		t.Fatalf("runner requests = %+v, want one naming fleet job id pulled-9", reqs)
	}
}
