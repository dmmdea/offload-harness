package fleetnode_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
)

// The shipped decoder reads the shipped handler's media_routes (ADR 0076): the verdicts and the media-job
// task arrive as the node published them.
func TestMediaRoutesAndMediaJobSurviveTheDelegatorsHealthDecoder(t *testing.T) {
	cfg := config.Config{
		RunGraphScript: "render/comfy-run-graph.mjs", FleetMediaInputs: true, FleetAuthToken: "tok", MediaDir: t.TempDir(),
	}
	jobs := fleetnode.NewJobs(time.Hour, cfg.FleetConcurrencyLimit())
	t.Cleanup(func() { jobs.DrainAndStop(2 * time.Second) })
	srv := fleetnode.New(nopRunner{}, jobs, fleetnode.Options{
		NodeID: "wire-media", Version: "test", Cfg: cfg,
		Snapshot: func() (fleetnode.Snapshot, bool) {
			return fleetnode.Snapshot{TotalGiB: 16, FreeGiB: 12, At: time.Now()}, true
		},
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	v, err := delegate.FetchNodeView(context.Background(), ts.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if !v.MediaRoutesKnown {
		t.Fatal("the node published media_routes and the decoder did not see them")
	}
	if st, ok := v.RouteState("run_graph"); !ok || st == "" {
		t.Fatalf("run_graph has no verdict: %+v", v.MediaRoutes)
	}
	found := map[string]bool{}
	for _, task := range v.Tasks {
		found[task] = true
	}
	if !found["run-graph"] || !found["media-job"] {
		t.Fatalf("supported tasks %v: want run-graph and media-job", v.Tasks)
	}
}
