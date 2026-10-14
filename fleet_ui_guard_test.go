package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// SF-16: fleet-ui re-serves data the poller fetched with the fleet bearer, so its page sits
// behind the ingress guard: a DNS-rebinding Host is refused under enforce, its own page's
// Origin is admitted, a foreign Origin is not.
func TestFleetUISitsBehindTheIngressGuard(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := fleetUIHandler(ok, "enforce")
	get := func(host, origin string) int {
		r := httptest.NewRequest(http.MethodGet, "http://placeholder/api/overview", nil)
		r.Host = host
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if c := get("attacker.example:18813", ""); c != http.StatusMisdirectedRequest {
		t.Errorf("rebinding Host: %d, want 421", c)
	}
	if c := get("127.0.0.1:18813", "http://127.0.0.1:18813"); c != http.StatusOK {
		t.Errorf("the page's own Origin: %d, want 200", c)
	}
	if c := get("127.0.0.1:18813", "http://evil.example"); c != http.StatusForbidden {
		t.Errorf("a foreign Origin: %d, want 403", c)
	}
}
