package composeremote

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/rosterprobe/rostertest"
)

// Every request the compose client builds carries the fleet bearer when one is configured: the dispatch
// through each door (the composition door and the project door), the job poll and the output fetch.
func TestEveryRequestCarriesTheFleetBearer(t *testing.T) {
	const tok = "compose-pin-token"
	cfg := config.Default()
	cfg.FleetAuthToken = tok
	node := rostertest.NewAuthNode(t, func(r *http.Request) (int, string) {
		switch {
		case r.Method == http.MethodPost:
			return http.StatusAccepted, `{"status":"accepted","job_id":"x"}`
		case strings.HasPrefix(r.URL.Path, "/fleet/media/"):
			return http.StatusOK, "bytes"
		}
		return http.StatusOK, `{"state":"error","error":"stop"}`
	})
	for _, route := range []string{"/fleet/dispatch", "/fleet/compose-project"} {
		t.Run("dispatch "+route, func(t *testing.T) {
			node.Reset()
			if err := post(context.Background(), cfg, node.URL, route, []byte(`{"job_id":"x"}`)); err != nil {
				t.Fatalf("post: %v", err)
			}
			node.WantBearer(t, tok)
		})
	}
	t.Run("poll", func(t *testing.T) {
		node.Reset()
		if _, _, err := wait(context.Background(), cfg, node.URL, "x", core.NopAttribution{}); err != nil {
			t.Fatalf("wait: %v", err)
		}
		node.WantBearer(t, tok)
	})
	t.Run("output fetch", func(t *testing.T) {
		node.Reset()
		if err := download(context.Background(), cfg, node.URL, "render-x.mp4", filepath.Join(t.TempDir(), "out.mp4")); err != nil {
			t.Fatalf("download: %v", err)
		}
		node.WantBearer(t, tok)
	})
}
