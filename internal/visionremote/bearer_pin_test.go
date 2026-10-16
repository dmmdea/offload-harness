package visionremote

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/rosterprobe/rostertest"
)

// Every request this lane builds carries the fleet bearer when one is configured: the dispatch of each
// vision task and the job poll. The node refuses an unauthenticated vision call, so a request that drops
// the header is a lane that cannot work, and one that drops it in only one task type would fail only there.
func TestEveryRequestCarriesTheFleetBearer(t *testing.T) {
	const tok = "vision-pin-token"
	cfg := config.Default()
	cfg.FleetAuthToken = tok
	node := rostertest.NewAuthNode(t, func(r *http.Request) (int, string) {
		if r.Method == http.MethodPost {
			return http.StatusAccepted, `{"status":"accepted","job_id":"x"}`
		}
		return http.StatusOK, `{"state":"error","error":"stop"}`
	})
	for _, task := range []core.TaskType{core.TaskVQA, core.TaskOCR, core.TaskAssessImage} {
		t.Run(string(task), func(t *testing.T) {
			node.Reset()
			id, err := dispatch(context.Background(), cfg, node.URL, "n", core.Request{Task: task, Params: map[string]any{"question": "q"}}, "data:image/png;base64,AA==", core.NopAttribution{})
			if err != nil {
				t.Fatalf("dispatch: %v", err)
			}
			if _, err := wait(context.Background(), cfg, node.URL, id, core.NopAttribution{}); err != nil {
				t.Fatalf("wait: %v", err)
			}
			seen := node.Seen()
			if len(seen) < 2 || !strings.HasPrefix(seen[0].Path, "/fleet/vision") || !strings.HasPrefix(seen[1].Path, "/fleet/jobs/") {
				t.Fatalf("expected a dispatch and a poll, saw %+v", seen)
			}
			node.WantBearer(t, tok)
		})
	}
}
