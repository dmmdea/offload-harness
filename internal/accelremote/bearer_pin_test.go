package accelremote

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/rosterprobe/rostertest"
)

// Every request the accelerator lane builds carries the fleet bearer when one is configured.
func TestEveryRequestCarriesTheFleetBearer(t *testing.T) {
	const tok = "accel-pin-token"
	cfg := config.Default()
	cfg.FleetAuthToken = tok
	node := rostertest.NewAuthNode(t, func(r *http.Request) (int, string) {
		if r.Method == http.MethodPost {
			return http.StatusAccepted, `{"status":"accepted","job_id":"x"}`
		}
		return http.StatusOK, `{"state":"error","error":"stop"}`
	})
	id, err := dispatch(context.Background(), cfg, node.URL, []byte(`{"tool":"classify"}`))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if _, err := wait(context.Background(), cfg, node.URL, id); err != nil {
		t.Fatalf("wait: %v", err)
	}
	seen := node.Seen()
	if len(seen) < 2 || !strings.HasPrefix(seen[0].Path, "/fleet/dispatch") || !strings.HasPrefix(seen[1].Path, "/fleet/jobs/") {
		t.Fatalf("expected a dispatch and a poll, saw %+v", seen)
	}
	node.WantBearer(t, tok)
}
