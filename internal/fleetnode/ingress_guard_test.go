package fleetnode

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// SF-16: the fleet node's whole mux sits behind the ingress guard. With
// fleet_ingress_guard=enforce a DNS-rebinding Host and any Origin are refused before a lane
// sees them; the default (warn) and off still serve them, as before.
func TestFleetNodeIngressGuardWrapsEveryRoute(t *testing.T) {
	get := func(mode, host, origin string) int {
		t.Helper()
		srv, _ := newTestServer(t, config.Config{FleetIngressGuard: mode}, nil, nil)
		ts := httptest.NewServer(srv.Handler())
		defer ts.Close()
		req, err := http.NewRequest(http.MethodGet, ts.URL+"/fleet/health", nil)
		if err != nil {
			t.Fatal(err)
		}
		if host != "" {
			req.Host = host
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := get("enforce", "attacker.example:18811", ""); c != http.StatusMisdirectedRequest {
		t.Errorf("enforce, rebinding Host: %d, want 421", c)
	}
	if c := get("enforce", "", "http://evil.example"); c != http.StatusForbidden {
		t.Errorf("enforce, Origin: %d, want 403", c)
	}
	if c := get("enforce", "", ""); c != http.StatusOK {
		t.Errorf("enforce, a clean loopback request: %d, want 200", c)
	}
	for _, mode := range []string{"", "warn", "off"} {
		if c := get(mode, "attacker.example:18811", "http://evil.example"); c != http.StatusOK {
			t.Errorf("mode %q refused a request it must only log or pass: %d", mode, c)
		}
	}
}

// The node refuses a non-JSON POST under enforce (the queue lane checks no Content-Type of its
// own), and warn writes a line to the log (review: dropping Logf stayed green).
func TestFleetNodeIngressGuardRefusesSimplePostsAndLogsInWarn(t *testing.T) {
	post := func(mode string) int {
		t.Helper()
		srv, _ := newTestServer(t, config.Config{FleetIngressGuard: mode}, nil, nil)
		ts := httptest.NewServer(srv.Handler())
		defer ts.Close()
		resp, err := http.Post(ts.URL+"/fleet/text", "text/plain", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := post("enforce"); c != http.StatusUnsupportedMediaType {
		t.Errorf("enforce, text/plain POST: %d, want 415", c)
	}
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	_ = post("warn")
	if !strings.Contains(buf.String(), "fleet_ingress_guard=warn") || !strings.Contains(buf.String(), "Content-Type") {
		t.Errorf("warn did not log the violation: %q", buf.String())
	}
}
