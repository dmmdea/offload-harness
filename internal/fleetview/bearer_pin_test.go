package fleetview

import (
	"context"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/rosterprobe/rostertest"
)

// Every request the fleet view's poller builds carries the fleet bearer when one is configured: the health
// read and the jobs feed read of each node.
func TestEveryRequestCarriesTheFleetBearer(t *testing.T) {
	const tok = "fleetview-pin-token"
	cfg := config.Default()
	cfg.FleetAuthToken = tok
	node := rostertest.NewAuthNode(t, nil)
	p := NewPoller(cfg, []string{node.URL}, time.Hour, 10)
	for _, path := range []string{"/fleet/health", "/fleet/jobs?limit=5"} {
		t.Run(path, func(t *testing.T) {
			node.Reset()
			if _, err := p.getJSON(context.Background(), node.URL+path); err != nil {
				t.Fatalf("getJSON: %v", err)
			}
			node.WantBearer(t, tok)
		})
	}
}
