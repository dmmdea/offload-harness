// This is an EXTERNAL test package (fleetnode_test, not fleetnode) for one
// reason: it must import internal/delegate to run the delegator's REAL health
// decoder against this node's REAL health handler. Nothing in internal/delegate
// is modified or exercised for its own sake here — the subject under test is
// the fleet node's wire, and delegate.FetchNodeView is the production reader
// that has to keep decoding it.
//
// Re-declaring healthWire's fields inside this package would be the exact
// "seam test that bypasses the logic it certifies" failure: a copy of the
// struct proves a copy still decodes. Only the shipped decoder can prove the
// shipped decoder still decodes.
package fleetnode_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
)

// nopRunner satisfies fleetnode.Runner without doing anything: this file never
// dispatches, it only reads health.
type nopRunner struct{}

func (nopRunner) Run(ctx context.Context, req core.Request) core.Result {
	return core.Result{OK: true, Data: json.RawMessage(`{}`)}
}

// TestHealthWireStaysDecodableByTheDelegator is the wire-compatibility proof
// for the 0.100.0 capacity fields. internal/delegate/nodeview.go decodes health
// into a FIXED struct listing only the six fields placement consumes; the four
// fields added here are not among them. Plain encoding/json ignores unknown
// keys (no DisallowUnknownFields on that path), but "ignores unknown keys" is a
// property of how the decoder is CONFIGURED, and configuration changes — so it
// is pinned here rather than assumed.
//
// The assertion that matters is queue_depth: the delegator's placement
// tie-break (gate.go, `r.QueueDepth < best.QueueDepth`) reads it, and it must
// still arrive with its ORIGINAL meaning — accepted + running — not the running
// count and not the backlog count.
func TestHealthWireStaysDecodableByTheDelegator(t *testing.T) {
	cfg := config.Config{
		ImageGenScript:         "C:/x/comfy-generate.mjs",
		FleetMaxQueueDepth:     7,
		FleetMaxConcurrentJobs: 2,
	}
	jobs := fleetnode.NewJobs(time.Hour, cfg.FleetConcurrencyLimit())
	t.Cleanup(func() { jobs.DrainAndStop(2 * time.Second) })

	srv := fleetnode.New(nopRunner{}, jobs, fleetnode.Options{
		NodeID:  "wire-node",
		Version: "test",
		Snapshot: func() (fleetnode.Snapshot, bool) {
			return fleetnode.Snapshot{TotalGiB: 16, FreeGiB: 12, At: time.Now()}, true
		},
		Cfg: cfg,
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Put the node in a state where the three counters are all different, so a
	// decoder reading the wrong key cannot accidentally look right: with two
	// execution slots, five admitted jobs are 2 running + 3 queued = depth 5.
	release := make(chan struct{})
	defer close(release)
	block := func(ctx context.Context) (json.RawMessage, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return json.RawMessage(`1`), nil
	}
	for _, id := range []string{"w1", "w2", "w3", "w4", "w5"} {
		if !jobs.Accept(id, block) {
			t.Fatalf("Accept(%s) must admit", id)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if q, r := jobs.Counts(); q == 3 && r == 2 {
			break
		}
		if time.Now().After(deadline) {
			q, r := jobs.Counts()
			t.Fatalf("node never settled at 3 queued / 2 running (got %d/%d)", q, r)
		}
		time.Sleep(2 * time.Millisecond)
	}

	// 1) The production decoder still decodes, and queue_depth still means
	//    accepted+running.
	view, err := delegate.FetchNodeView(context.Background(), ts.URL, "")
	if err != nil {
		t.Fatalf("the delegator's health decoder rejected a 0.100.0 payload: %v", err)
	}
	if view.NodeID != "wire-node" {
		t.Fatalf("node_id decoded as %q", view.NodeID)
	}
	if view.QueueDepth != 5 {
		t.Fatalf("QueueDepth decoded as %d, want 5 (2 running + 3 queued) — queue_depth changed meaning on the wire", view.QueueDepth)
	}
	// The capacity triad decodes too (offload_status publishes it, and an
	// operator sent here by a `queue deadline` failure needs it to be right).
	// All three values differ, so a decoder wired to the wrong key is caught.
	if view.JobsRunning != 2 || view.JobsQueued != 3 || view.MaxConcurrentJobs != 2 {
		t.Fatalf("capacity decoded as running=%d queued=%d max=%d, want 2/3/2",
			view.JobsRunning, view.JobsQueued, view.MaxConcurrentJobs)
	}

	// 2) The new fields really are ON that payload (otherwise (1) proves
	//    nothing about tolerating them).
	resp, err := http.Get(ts.URL + "/fleet/health")
	if err != nil {
		t.Fatalf("health GET: %v", err)
	}
	defer resp.Body.Close()
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("health not JSON: %v", err)
	}
	for field, want := range map[string]float64{
		"queue_depth":         5,
		"jobs_running":        2,
		"jobs_queued":         3,
		"max_concurrent_jobs": 2,
		"max_queue_depth":     7,
	} {
		if raw[field] != want {
			t.Fatalf("health %s = %v, want %v", field, raw[field], want)
		}
	}

	// 3) Belt and braces for a FUTURE additive field: a payload carrying a key
	//    no released decoder has ever seen must still decode. This is the
	//    property the four fields above rely on, asserted directly.
	future := `{"node_id":"future-node","queue_depth":9,"a_field_from_2027":{"nested":[1,2,3]}}`
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(future))
	}))
	defer fs.Close()
	fv, err := delegate.FetchNodeView(context.Background(), fs.URL, "")
	if err != nil {
		t.Fatalf("an unknown health field broke the delegator's decode: %v", err)
	}
	if fv.NodeID != "future-node" || fv.QueueDepth != 9 {
		t.Fatalf("future payload decoded as %+v", fv)
	}
	// A pre-0.100.0 node publishes no capacity fields at all; they must decode
	// to zero rather than fail, and a consumer reads 0 as "no usable number".
	if fv.JobsRunning != 0 || fv.JobsQueued != 0 || fv.MaxConcurrentJobs != 0 {
		t.Fatalf("absent capacity fields decoded as %d/%d/%d, want zeros",
			fv.JobsRunning, fv.JobsQueued, fv.MaxConcurrentJobs)
	}
	if !strings.Contains(future, "a_field_from_2027") {
		t.Fatal("test fixture lost its unknown field")
	}
}
