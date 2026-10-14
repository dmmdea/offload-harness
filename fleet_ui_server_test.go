package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The server runFleetUI runs is the guarded one (SF-16 verify S3): a DNS-rebinding Host is
// refused under enforce by the very server object, not only by the handler helper.
func TestTheRealFleetUIServerIsGuarded(t *testing.T) {
	srv := newFleetUIServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), "enforce")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "attacker.example:18813"
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("fleet-ui's server answered %d to a rebinding Host, want 421", rec.Code)
	}
}

// fleet-ui refuses a config that failed to load (SF-16 verify S2): its guard mode would
// otherwise fall back to the default without a word.
func TestFleetUIRefusesAConfigThatFailedToLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.json")
	if err := os.WriteFile(path, []byte(`{"fleet_ingress_guard": "enforce",}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// An address that cannot be bound: without the gate the run fails at once on it instead of
	// serving forever, so a regression fails this test fast.
	err := runFleetUI([]string{"--config", path, "--remote", "http://127.0.0.1:1", "--listen", "256.256.256.256:1"})
	if err == nil || !strings.Contains(err.Error(), "refusing to serve") {
		t.Fatalf("fleet-ui on a broken config: %v, want a refusal", err)
	}
}
