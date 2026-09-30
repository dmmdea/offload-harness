package delegate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
)

// The fake-node tests above prove the delegator against the delegator's idea of
// the node. This one runs the REAL fleet node handler behind the REAL delegator,
// so a change to either side's withdraw wire (the route, the answer's shape, the
// state string, the bearer gate) fails here instead of in production: nothing
// else checks that the two agree.

// e2eSeat is the one fake: a seat whose roster lists the agent seat, so the real
// node's residency probe advertises it as served.
func e2eSeat(t *testing.T, seat string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"object":"model"}]}`, seat)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// blockedRunner is the node's Runner: it never completes a delegated job until
// released, and records whether one ever reached it.
type blockedRunner struct {
	release chan struct{}
	ran     atomic.Int64
}

func (b *blockedRunner) Run(ctx context.Context, req core.Request) core.Result {
	b.ran.Add(1)
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return core.Result{OK: true, Data: json.RawMessage(`{"schema_version":1,"output":"late","structured":{"answer":"late"}}`)}
}

// TestWithdrawAgainstTheRealNodeHandler: a delegated job queues behind a busy
// slot on a real fleet node, the delegator gives it up at the queue deadline, and
// the node's own handler answers the real DELETE. The node ends with the job
// withdrawn — terminal, never run — and the delegator with the intent closed and
// the failure saying the job was taken back.
func TestWithdrawAgainstTheRealNodeHandler(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 20*time.Millisecond)
	compressWallUnit(t, 10*time.Millisecond)

	const seat, token = "e2e-agent-seat", "e2e-fleet-token"
	seatSrv := e2eSeat(t, seat)
	nodeCfg := config.Default()
	nodeCfg.Home = t.TempDir()
	nodeCfg.Endpoint = seatSrv.URL
	nodeCfg.Model = "workhorse"
	nodeCfg.AgentModel = seat
	nodeCfg.AgentCtxTokens = 16384
	nodeCfg.FleetNodeID = "e2e-node"
	nodeCfg.FleetAgentEnabled = true
	nodeCfg.FleetAuthToken = token
	nodeCfg.FleetMaxConcurrentJobs = 1 // one slot: the delegated job has to wait behind the holder

	runner := &blockedRunner{release: make(chan struct{})}
	jobs := fleetnode.NewJobs(time.Hour, nodeCfg.FleetConcurrencyLimit())
	node := fleetnode.New(runner, jobs, fleetnode.Options{
		NodeID: nodeCfg.FleetNodeID,
		Snapshot: func() (fleetnode.Snapshot, bool) {
			return fleetnode.Snapshot{TotalGiB: 16, FreeGiB: 12, At: time.Now()}, true
		},
		Footprints:       func() []fleetnode.FootprintEntry { return nil },
		GpuVendor:        "nvidia",
		GpuArch:          "ampere",
		LoopbackListener: true,
		Cfg:              nodeCfg,
	})
	node.RefreshAgentResidency() // a cold cache advertises the seat as not resident (fail-closed)
	// Someone else's job takes the only slot BETWEEN the delegator's health probe
	// (which saw room) and its dispatch — the stale-headroom race that leaves a job
	// in a node's backlog. Injected in front of the real handler, on the first
	// dispatch, so the delegated job is admitted behind it and waits `accepted`.
	var holderOnce sync.Once
	holderRunning := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/fleet/dispatch" {
			holderOnce.Do(func() {
				if !jobs.Admit("holder", fleetnode.AcceptSpec{Agent: true}, func(ctx context.Context) (json.RawMessage, error) {
					close(holderRunning)
					<-runner.release
					return json.RawMessage(`{}`), nil
				}) {
					t.Error("holder job was not admitted")
					return
				}
				<-holderRunning
			})
		}
		// Answer polls at once, as a node without the long poll does: a real long
		// poll would park for its full (uncompressed) wait and this test would spend
		// twelve seconds proving nothing more.
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/fleet/jobs/") {
			q := r.URL.Query()
			q.Del("wait")
			r.URL.RawQuery = q.Encode()
		}
		node.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(runner.release); jobs.DrainAndStop(3 * time.Second) })

	cfg := testCfg(t)
	cfg.FleetAuthToken = token
	contract := withdrawContract()
	contract.TimeoutSec = 30
	results, _, err := Run(t.Context(), cfg, neverLocal(t), []core.AgentContract{contract}, "remote", []string{srv.URL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	r := results[0]
	if !strings.Contains(r.Err, "queue deadline") || !strings.Contains(r.Err, "withdrawn from the node") {
		t.Fatalf("err = %q (deferred %v: %s), want the queue deadline with the job reported withdrawn from the node — the real handler's answer was not read as a confirmation", r.Err, r.Result.Deferred, r.Result.Reason)
	}
	if runner.ran.Load() != 0 {
		t.Fatalf("the node's runner saw %d delegated job(s): a withdrawn job must never run", runner.ran.Load())
	}
	v, ok := jobs.Get(r.JobID)
	if !ok || v.State != fleetnode.JobError || v.Error != fleetnode.ErrWithdrawn {
		t.Fatalf("node's record of %s = ok=%v %+v, want terminal with %q", r.JobID, ok, v, fleetnode.ErrWithdrawn)
	}
	if closed, open := intentNotes(t, cfg.StateDir); closed[r.JobID] != intentNoteWithdrawn || len(open) != 0 {
		t.Fatalf("intent closed as %q (open=%v), want %q", closed[r.JobID], open, intentNoteWithdrawn)
	}
	// The pairing the source cannot enforce across packages: the state string the
	// delegator reads is the one the node writes.
	if fleetnode.WithdrawnState != withdrawnState {
		t.Fatalf("fleetnode.WithdrawnState = %q, delegate's withdrawnState = %q: the two ends disagree", fleetnode.WithdrawnState, withdrawnState)
	}
}
