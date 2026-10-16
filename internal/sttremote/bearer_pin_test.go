package sttremote

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/rosterprobe/rostertest"
)

// Every request the stt client builds carries the fleet bearer when one is configured: the audio upload,
// the job poll and the transcript fetch.
func TestEveryRequestCarriesTheFleetBearer(t *testing.T) {
	const tok = "stt-pin-token"
	cfg := config.Default()
	cfg.FleetAuthToken = tok
	node := rostertest.NewAuthNode(t, func(r *http.Request) (int, string) {
		switch {
		case r.Method == http.MethodPost:
			return http.StatusAccepted, `{"status":"accepted","job_id":"x"}`
		case strings.HasPrefix(r.URL.Path, "/fleet/media/"):
			return http.StatusOK, "[]"
		}
		return http.StatusOK, `{"state":"error","error":"stop"}`
	})
	t.Run("upload", func(t *testing.T) {
		node.Reset()
		if _, err := dispatch(context.Background(), cfg, node.URL, "n", core.Request{Task: core.TaskTranscribe}, "wav", []byte("RIFF"), false, core.NopAttribution{}); err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		node.WantBearer(t, tok)
	})
	t.Run("poll", func(t *testing.T) {
		node.Reset()
		if _, err := wait(context.Background(), cfg, node.URL, "x", core.NopAttribution{}); err != nil {
			t.Fatalf("wait: %v", err)
		}
		node.WantBearer(t, tok)
	})
	t.Run("transcript fetch", func(t *testing.T) {
		node.Reset()
		if _, err := fetchSegments(context.Background(), cfg, node.URL, "a-1234abcd.segments.json"); err != nil {
			t.Fatalf("fetchSegments: %v", err)
		}
		node.WantBearer(t, tok)
	})
}
