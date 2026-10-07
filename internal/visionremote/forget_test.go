package visionremote

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/rosterprobe"
	"github.com/dmmdea/offload-harness/internal/rosterprobe/rostertest"
)

// A call can pick a node whose health it just read while another call's probe of the same node failed a
// moment earlier (it was starting up), leaving a negative verdict behind. When this lane's dispatch is then
// ACCEPTED the node is evidently serving, and the verdict is dropped at once instead of being replayed, to
// every lane in the process, for the rest of the window.
func TestAcceptedDispatchDropsTheNegativeVerdictAgainstThatNode(t *testing.T) {
	rostertest.Zones(t)
	rosterprobe.Default.Reset()
	t.Cleanup(rosterprobe.Default.Reset)
	node := rostertest.NewLateNode(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	probe := func() rosterprobe.Reading {
		return rosterprobe.Probe(context.Background(), []string{node.Base}, "", 5*time.Second)[0]
	}

	// 1. The node is down: the first probe records a refusal and the second is served from the cache.
	if r := probe(); r.Err == nil || r.Source != rosterprobe.SourceProbe {
		t.Fatalf("precondition: a node that is down must fail its probe, got %v / %q", r.Err, r.Source)
	}
	if r := probe(); r.Source != rosterprobe.SourceNegative {
		t.Fatalf("precondition: the failure must now be held in the cache, got %v / %q", r.Err, r.Source)
	}

	// 2. It comes up, and this lane's dispatch to it is accepted.
	node.Start()
	cfg := config.Default()
	cfg.FleetAuthToken = "the-fleet-bearer"
	if err := dispatchAccepted(t, cfg, node.Base); err != nil {
		t.Fatalf("dispatch to the node that is up: %v", err)
	}

	// 3. The verdict is gone: the next probe dials the node and gets its health.
	if r := probe(); r.Err != nil || r.Source != rosterprobe.SourceProbe {
		t.Fatalf("after an accepted dispatch the node must be dialled again, got %v / %q", r.Err, r.Source)
	}
}

func dispatchAccepted(t *testing.T, cfg config.Config, base string) error {
	t.Helper()
	ctx := context.Background()
	_, err := dispatch(ctx, cfg, base, "node-late", core.Request{Task: core.TaskVQA}, "data:image/png;base64,AA==", core.NopAttribution{})
	return err
}
