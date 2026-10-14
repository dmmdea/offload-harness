package fleetnode

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// The server the node runs is the guarded one (SF-16 verify S3): httpServer's handler refuses a
// rebinding Host under enforce, not only s.Handler() called by a test.
func TestTheNodesRealServerIsGuarded(t *testing.T) {
	srv, _ := newTestServer(t, config.Config{FleetIngressGuard: "enforce"}, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/fleet/health", nil)
	req.Host = "attacker.example:18811"
	rec := httptest.NewRecorder()
	srv.httpServer().Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("the node's server answered %d to a rebinding Host, want 421", rec.Code)
	}
}
