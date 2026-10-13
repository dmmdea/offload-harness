// An EXTERNAL test package for the same reason healthwire_compat_test.go is: it runs the delegator's real health
// decoder (delegate.FetchNodeView) against this node's real health handler.
package fleetnode_test

import (
	"context"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
)

func ptrInt(v int) *int { return &v }

// F11 is additive: the delegator decodes cards into the same gpuprobe.Device the node publishes, so a node's
// temperature and throttle reasons arrive at the delegator with no change on its side, and a card without them
// decodes exactly as before.
func TestHealthThermalFieldsAreReadByTheDelegator(t *testing.T) {
	cfg := config.Config{FleetMaxConcurrentJobs: 1, FleetMaxQueueDepth: 4}
	jobs := fleetnode.NewJobs(time.Hour, cfg.FleetConcurrencyLimit())
	t.Cleanup(func() { jobs.DrainAndStop(2 * time.Second) })

	devs := []fleetnode.GPUDevice{
		{Index: 0, UUID: "GPU-1111aaaa-2222-3333-4444-555566667777", Name: "synthetic 16 GB", TotalGiB: 16, FreeGiB: 15, UtilKnown: true,
			TemperatureC: ptrInt(83), ThrottleReasons: []string{"sw_thermal_slowdown", "hw_thermal_slowdown"}},
		{Index: 1, UUID: "GPU-2222bbbb-3333-4444-5555-666677778888", Name: "synthetic 16 GB", TotalGiB: 16, FreeGiB: 15, UtilKnown: true},
	}
	srv := fleetnode.New(nopRunner{}, jobs, fleetnode.Options{
		NodeID:  "thermal-node",
		Version: "test",
		Snapshot: func() (fleetnode.Snapshot, bool) {
			return fleetnode.Snapshot{TotalGiB: 16, FreeGiB: 15, Devices: devs, At: time.Now()}, true
		},
		Cfg: cfg,
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	view, err := delegate.FetchNodeView(context.Background(), ts.URL, "")
	if err != nil {
		t.Fatalf("the delegator's health decoder rejected a payload with thermal fields: %v", err)
	}
	if len(view.Devices) != 2 {
		t.Fatalf("delegator decoded %d cards, want 2", len(view.Devices))
	}
	hot := view.Devices[0]
	if hot.TemperatureC == nil || *hot.TemperatureC != 83 ||
		!reflect.DeepEqual(hot.ThrottleReasons, []string{"sw_thermal_slowdown", "hw_thermal_slowdown"}) {
		t.Errorf("card 0 decoded as %+v, want 83 C and both thermal reasons", hot)
	}
	if cool := view.Devices[1]; cool.TemperatureC != nil || len(cool.ThrottleReasons) != 0 {
		t.Errorf("card 1 decoded as %+v, want no thermal fields", cool)
	}
}
