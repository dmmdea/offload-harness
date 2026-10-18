package textremote

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/rosterprobe/rostertest"
)

// Every request the text lane builds carries the fleet bearer when one is configured: the dispatch and
// the job poll.
func TestEveryRequestCarriesTheFleetBearer(t *testing.T) {
	const tok = "text-pin-token"
	cfg := config.Default()
	cfg.FleetAuthToken = tok
	node := rostertest.NewAuthNode(t, func(r *http.Request) (int, string) {
		if r.Method == http.MethodPost {
			return http.StatusAccepted, `{"status":"accepted","job_id":"x"}`
		}
		return http.StatusOK, `{"state":"error","error":"stop"}`
	})
	if err := dispatch(context.Background(), cfg, node.URL, []byte(`{"job_id":"x","task":"classify"}`)); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if _, err := wait(context.Background(), cfg, node.URL, "x", core.NopAttribution{}); err != nil {
		t.Fatalf("wait: %v", err)
	}
	seen := node.Seen()
	if len(seen) < 2 || !strings.HasPrefix(seen[0].Path, "/fleet/text") || !strings.HasPrefix(seen[1].Path, "/fleet/jobs/") {
		t.Fatalf("expected a dispatch and a poll, saw %+v", seen)
	}
	node.WantBearer(t, tok)
}
