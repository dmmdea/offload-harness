package rosterprobe

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/rosterprobe/rostertest"
)

// Every request the roster probe builds carries the fleet bearer when one is configured: the health read
// of each member and the token probe.
func TestEveryRequestCarriesTheFleetBearer(t *testing.T) {
	const tok = "roster-pin-token"
	node := rostertest.NewAuthNode(t, func(r *http.Request) (int, string) {
		if r.Method == http.MethodPost {
			return http.StatusBadRequest, `{"status":"error","error":"job_id required"}`
		}
		return http.StatusOK, `{"node_id":"n1","schema_version":1}`
	})
	t.Run("health read", func(t *testing.T) {
		node.Reset()
		readings := Probe(context.Background(), []string{node.URL}, tok, 2*time.Second)
		if len(readings) != 1 || readings[0].Err != nil {
			t.Fatalf("probe: %+v", readings)
		}
		node.WantBearer(t, tok)
	})
	t.Run("token probe", func(t *testing.T) {
		node.Reset()
		if _, _, err := CheckToken(context.Background(), node.URL, tok); err != nil {
			t.Fatalf("CheckToken: %v", err)
		}
		node.WantBearer(t, tok)
	})
}
