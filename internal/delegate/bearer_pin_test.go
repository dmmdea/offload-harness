package delegate

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dmmdea/offload-harness/internal/rosterprobe/rostertest"
)

// Every request the delegator sends to a fleet node carries the fleet bearer when one is configured: the
// agent dispatch, the job poll (with and without the long-poll parameter), the withdraw, the health read
// of the roster, the queue submit and the poll of a queued job. A node gates all of them.
func TestEveryRequestCarriesTheFleetBearer(t *testing.T) {
	const tok = "delegate-pin-token"
	cfg := testCfg(t)
	cfg.FleetAuthToken = tok
	node := rostertest.NewAuthNode(t, func(r *http.Request) (int, string) {
		switch {
		case r.Method == http.MethodPost:
			return http.StatusAccepted, `{"status":"accepted","job_id":"j1"}`
		case r.Method == http.MethodDelete:
			return http.StatusOK, `{"state":"withdrawn","withdrawn":true}`
		case r.URL.Path == "/fleet/health":
			return http.StatusOK, `{"node_id":"n1","schema_version":1}`
		}
		return http.StatusOK, `{"state":"running"}`
	})
	r := &runner{cfg: cfg}
	ctx := context.Background()

	cases := []struct {
		name string
		do   func(t *testing.T)
	}{
		{"agent dispatch", func(t *testing.T) {
			if res := r.dispatchDetailed(ctx, node.URL, "j1", json.RawMessage(`{}`)); res.err != nil {
				t.Fatalf("dispatch: %v", res.err)
			}
		}},
		{"job poll", func(t *testing.T) {
			if _, err := r.pollOnce(ctx, node.URL, "j1"); err != nil {
				t.Fatalf("poll: %v", err)
			}
		}},
		{"intent recovery poll", func(t *testing.T) {
			if _, err := pollJobOnceAt(ctx, cfg, node.URL+"/fleet/jobs/j1"); err != nil {
				t.Fatalf("poll: %v", err)
			}
		}},
		{"withdraw", func(t *testing.T) {
			r.withdraw(ctx, node.URL, "j1")
		}},
		{"node view", func(t *testing.T) {
			if _, err := FetchNodeView(ctx, node.URL, tok); err != nil {
				t.Fatalf("health: %v", err)
			}
		}},
		{"queue submit", func(t *testing.T) {
			if err := queueSubmit(ctx, cfg, node.URL, "j1", "agent", json.RawMessage(`{}`), 60); err != nil {
				t.Fatalf("submit: %v", err)
			}
		}},
		{"queued job poll", func(t *testing.T) {
			if _, err := pollJobOnceAt(ctx, cfg, node.URL+"/fleet/queue/jobs/j1"); err != nil {
				t.Fatalf("poll: %v", err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			node.Reset()
			c.do(t)
			node.WantBearer(t, tok)
		})
	}
}
