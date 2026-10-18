package mediaremote

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/rosterprobe/rostertest"
)

// Every request the media client builds carries the fleet bearer when one is configured, whichever media
// task it carries: the dispatch (each task type, through the plain door and the input-carrying one), the job
// poll and the output fetch. A node gates every task type on the bearer, so one task type whose request
// drops the header is a lane that works for the rest and 401s for it.
func TestEveryRequestCarriesTheFleetBearer(t *testing.T) {
	const tok = "media-pin-token"
	cfg := config.Default()
	cfg.FleetAuthToken = tok
	node := rostertest.NewAuthNode(t, func(r *http.Request) (int, string) {
		switch {
		case r.Method == http.MethodPost:
			return http.StatusAccepted, `{"status":"accepted","job_id":"x"}`
		case strings.HasPrefix(r.URL.Path, "/fleet/media/"):
			return http.StatusOK, "bytes"
		}
		return http.StatusOK, `{"state":"running"}`
	})

	for _, task := range []string{"image-gen", "video-gen", "animate", "audio-gen", "run-graph"} {
		t.Run("dispatch "+task, func(t *testing.T) {
			node.Reset()
			if err := post(context.Background(), cfg, node.URL, "/fleet/dispatch", []byte(`{"job_id":"x","task_type":"`+task+`","payload":{}}`)); err != nil {
				t.Fatalf("post: %v", err)
			}
			node.WantBearer(t, tok)
		})
	}
	t.Run("dispatch through the input door", func(t *testing.T) {
		node.Reset()
		if err := post(context.Background(), cfg, node.URL, "/fleet/media-job", []byte(`{"job_id":"x","task_type":"media-job","payload":{}}`)); err != nil {
			t.Fatalf("post: %v", err)
		}
		node.WantBearer(t, tok)
	})
	t.Run("poll", func(t *testing.T) {
		node.Reset()
		if _, err := getJSON[jobWire](context.Background(), cfg, node.URL+"/fleet/jobs/x"); err != nil {
			t.Fatalf("poll: %v", err)
		}
		node.WantBearer(t, tok)
	})
	t.Run("output fetch", func(t *testing.T) {
		node.Reset()
		dir := t.TempDir()
		tmp, _, err := download(context.Background(), cfg, node.URL, "render-x.png", dir, 0o644, false, -1)
		if err != nil {
			t.Fatalf("download: %v", err)
		}
		_ = os.Remove(filepath.Clean(tmp))
		node.WantBearer(t, tok)
	})
}
