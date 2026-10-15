package netguard

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The refusal body goes to whoever sent the request, and under DNS rebinding that is a page in
// a browser which can read it (the rebound page is same-origin to its own name). The tailnet
// zone is private to the operator: netguard/tailnet.go keeps it out of the repo for that reason.
// So the zones and the config keys that set them belong in the operator's log line, never in
// the body. The body still names the key that relaxes the guard, which is public.

func TestEnforceBodyDoesNotNameTheTailnetZones(t *testing.T) {
	installZones(t, ownZone, sharerZone)
	sink := &logSink{}
	h := IngressGuard(IngressOptions{Mode: "enforce", Key: "fleet_ingress_guard", Logf: sink.logf}, okHandler())

	w := guardedRequest(t, h, http.MethodGet, "rebind.attacker.example:18811", nil)
	if w.Code != http.StatusMisdirectedRequest {
		t.Fatalf("a foreign Host: %d, want 421", w.Code)
	}
	body := w.Body.String()
	for _, secret := range []string{ownZone, sharerZone, "tailnet_suffix"} {
		if strings.Contains(body, secret) {
			t.Errorf("the 421 body names %q, which a rebound page can read:\n%s", secret, body)
		}
	}
	var parsed map[string]string
	if err := json.Unmarshal([]byte(body), &parsed); err != nil || parsed["status"] != "error" {
		t.Errorf("the 421 body is not the guard's JSON error: %v %s", err, body)
	}
	if !strings.Contains(body, "fleet_ingress_guard") {
		t.Errorf("the refusal must still name the key that relaxes it: %s", body)
	}
	if !strings.Contains(body, "rebind.attacker.example") {
		t.Errorf("the refusal should say which Host it refused (the caller sent it): %s", body)
	}

	// The operator's side keeps the full verdict: the zones the Host was judged against.
	logged := sink.all()
	if !strings.Contains(logged, ownZone) || !strings.Contains(logged, sharerZone) {
		t.Errorf("the log line must keep the zone list for the operator: %s", logged)
	}
}

// A value the caller sent is echoed back clipped: a 900 KB Host, Origin or Content-Type must
// not come back as a 900 KB body.
func TestEnforceBodyEchoesAClippedValue(t *testing.T) {
	installZones(t, ownZone)
	long := strings.Repeat("a", 900_000)
	h := IngressGuard(IngressOptions{Mode: "enforce", RequireJSONPost: true, Key: "fleet_ingress_guard"}, okHandler())
	for name, tc := range map[string]struct {
		method, host string
		hdr          map[string]string
	}{
		"Host":         {http.MethodGet, long + ".example", nil},
		"Origin":       {http.MethodGet, "127.0.0.1", map[string]string{"Origin": "http://" + long}},
		"Content-Type": {http.MethodPost, "127.0.0.1", map[string]string{"Content-Type": long}},
	} {
		w := guardedRequest(t, h, tc.method, tc.host, tc.hdr)
		if w.Code == http.StatusOK {
			t.Errorf("%s: a hostile request passed", name)
		}
		if n := w.Body.Len(); n > 2048 {
			t.Errorf("%s: the refusal body is %d bytes: the caller's value was echoed whole", name, n)
		}
	}
}
