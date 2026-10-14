package fleetnode

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/netguard"
)

// The node's guard judges the tailnet zones the operator's CONFIG names, all of them: a node
// shared in from another tailnet is reached by its name under that tailnet's zone, which the
// operator lists in tailnet_suffixes beside the primary tailnet_suffix. Loaded through the real
// config path (which installs the zones), under enforce, so this fails if the guard reads only
// the primary zone or if the load stops installing the list.
func TestFleetNodeAdmitsAHostUnderEveryConfiguredTailnetZone(t *testing.T) {
	prev := netguard.TailnetSuffixes()
	t.Cleanup(func() { _ = netguard.SetTailnetSuffixes(prev) })

	p := filepath.Join(t.TempDir(), "config.json")
	const body = `{"fleet_ingress_guard": "enforce", "tailnet_suffix": "tailnnnnnn.ts.net", "tailnet_suffixes": ["tailmmmmmm.ts.net"]}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := newTestServer(t, cfg, nil, nil)

	for host, want := range map[string]int{
		"node-a.tailnnnnnn.ts.net:18811":  http.StatusOK,                 // the primary zone
		"node-b.tailmmmmmm.ts.net:18811":  http.StatusOK,                 // the second zone: a node shared in
		"node-x.tailkkkkkk.ts.net:18811":  http.StatusMisdirectedRequest, // a tailnet nobody listed
		"evil.tailmmmmmm.ts.net.example":  http.StatusMisdirectedRequest,
		"eviltailmmmmmm.ts.net":           http.StatusMisdirectedRequest, // no label boundary
		"attacker.example:18811":          http.StatusMisdirectedRequest,
		"tailmmmmmm.ts.net":               http.StatusMisdirectedRequest, // the bare zone names no node
		"x.tailmmmmmm.ts.net.attacker.io": http.StatusMisdirectedRequest,
	} {
		req := httptest.NewRequest(http.MethodGet, "/fleet/health", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Host %q: %d, want %d", host, rec.Code, want)
		}
	}
}
