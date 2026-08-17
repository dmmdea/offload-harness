package llamaclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// countingServer returns an httptest server that counts requests and answers
// every one with the canned chat response.
func countingServer(t *testing.T, hits *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(cannedChatResp)); err != nil {
			t.Errorf("write canned response: %v", err)
		}
	}))
}

// TestBaseFor locks the per-model base resolution (Phase A, spec §S1): an
// exact model-id/alias key in the seat map wins; everything else — including
// a client with no overrides at all — resolves to the default base. "" means
// the client's default model, mirroring Generate's own resolution, so the
// default seat itself can be remoted.
func TestBaseFor(t *testing.T) {
	plain := New("http://127.0.0.1:11436", "", "offload-e4b", time.Second)
	if got := plain.BaseFor("lenovo-e4b"); got != "http://127.0.0.1:11436" {
		t.Errorf("no overrides: BaseFor = %q, want the default base", got)
	}
	if got := plain.BaseFor(""); got != "http://127.0.0.1:11436" {
		t.Errorf("no overrides, empty model: BaseFor = %q, want the default base", got)
	}

	c := New("http://127.0.0.1:11436", "", "offload-e4b", time.Second).
		WithSeatEndpoints(map[string]string{
			"lenovo-e4b":  "http://lenovo-m720q:11436/", // trailing slash must be trimmed like New's base
			"offload-e4b": "http://100.77.1.9:11436",    // the DEFAULT model seat, remoted
		})
	cases := []struct {
		model string
		want  string
	}{
		{"lenovo-e4b", "http://lenovo-m720q:11436"},
		{"offload-e4b", "http://100.77.1.9:11436"},
		{"", "http://100.77.1.9:11436"}, // "" = default model, which is overridden here
		{"gemma4-e2b", "http://127.0.0.1:11436"},
	}
	for _, tc := range cases {
		if got := c.BaseFor(tc.model); got != tc.want {
			t.Errorf("BaseFor(%q) = %q, want %q", tc.model, got, tc.want)
		}
	}
}

// TestSeatEndpointOverrideRoutesRequests: a completion for an overridden model
// lands on the override base; every other model stays on the default base.
// Both bases are loopback httptest servers, so the tailnet dial gate passes
// and what is measured is purely the routing.
func TestSeatEndpointOverrideRoutesRequests(t *testing.T) {
	var defaultHits, overrideHits int
	defSrv := countingServer(t, &defaultHits)
	defer defSrv.Close()
	ovrSrv := countingServer(t, &overrideHits)
	defer ovrSrv.Close()

	c := New(defSrv.URL, "", "offload-e4b", 5*time.Second).
		WithSeatEndpoints(map[string]string{"lenovo-e4b": ovrSrv.URL})

	if _, err := c.Generate(context.Background(), "", "sys", "hi", "", 16, 0, 0); err != nil {
		t.Fatalf("default-model Generate: %v", err)
	}
	if defaultHits != 1 || overrideHits != 0 {
		t.Fatalf("default-model call: hits = (default %d, override %d), want (1, 0)", defaultHits, overrideHits)
	}

	if _, err := c.Generate(context.Background(), "lenovo-e4b", "sys", "hi", "", 16, 0, 0); err != nil {
		t.Fatalf("overridden-model Generate: %v", err)
	}
	if defaultHits != 1 || overrideHits != 1 {
		t.Fatalf("overridden-model call: hits = (default %d, override %d), want (1, 1)", defaultHits, overrideHits)
	}
}

// TestSeatEndpointOverrideRidesTheTailnetGuard: the override path must go
// through netguard.SafeTransport — a request for a seat whose base is a
// public IP is refused at DIAL time (the config-load TailnetURL check cannot
// be the only line: a Client can be assembled in code with any map). The
// default seat on the same client stays untouched and keeps working.
func TestSeatEndpointOverrideRidesTheTailnetGuard(t *testing.T) {
	var defaultHits int
	defSrv := countingServer(t, &defaultHits)
	defer defSrv.Close()

	c := New(defSrv.URL, "", "offload-e4b", 5*time.Second).
		WithSeatEndpoints(map[string]string{"rogue-seat": "http://203.0.113.9:80"})

	_, err := c.Generate(context.Background(), "rogue-seat", "sys", "hi", "", 16, 0, 0)
	if err == nil {
		t.Fatal("Generate against a public-IP override succeeded, want a dial-time refusal")
	}
	if !strings.Contains(err.Error(), "tailnet guard") {
		t.Errorf("refusal error = %q, want it to name the tailnet guard", err.Error())
	}

	if _, err := c.Generate(context.Background(), "", "sys", "hi", "", 16, 0, 0); err != nil {
		t.Fatalf("default seat must be unaffected by a rogue override on another seat: %v", err)
	}
	if defaultHits != 1 {
		t.Fatalf("default hits = %d, want 1", defaultHits)
	}
}

// TestWithSeatEndpointsEmptyIsIdentity pins the additive-off contract: an
// empty/nil map installs NOTHING — no override table, no second HTTP client —
// so a config without seat_endpoints produces a client byte-identical to a
// pre-seat build (the absent-key compatibility the plan requires).
func TestWithSeatEndpointsEmptyIsIdentity(t *testing.T) {
	c := New("http://127.0.0.1:11436", "", "offload-e4b", time.Second)
	if got := c.WithSeatEndpoints(nil); got != c {
		t.Fatal("WithSeatEndpoints(nil) must return the same client")
	}
	if c.seatEndpoints != nil || c.safeHTTP != nil {
		t.Fatal("WithSeatEndpoints(nil) must not install an override table or a second HTTP client")
	}
	if got := c.WithSeatEndpoints(map[string]string{}); got != c {
		t.Fatal("WithSeatEndpoints(empty) must return the same client")
	}
	if c.seatEndpoints != nil || c.safeHTTP != nil {
		t.Fatal("WithSeatEndpoints(empty) must not install an override table or a second HTTP client")
	}
}
